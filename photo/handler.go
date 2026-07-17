package photo

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/tharit/synologysharegate/middleware"
	"github.com/tharit/synologysharegate/proxy"
)

// errDownloadDisabled is returned when a share's privacy_type is "public-view",
// which does not permit downloads.
var errDownloadDisabled = errors.New("downloads are not enabled for this share")

// checkDownloadAllowed re-verifies the share's privacy_type before forwarding a
// download request to the NAS. This proxy keeps no server-side session state, so
// the only source of truth for "is this share downloadable right now" is the NAS
// itself — a stale or forged client-side canDownload value must never be trusted
// for this check. The landing page fetch needs no session cookie.
func (h *Handler) checkDownloadAllowed(ctx context.Context, id string) error {
	l, err := FetchLanding(ctx, h.client, basePathSharing, id)
	if err != nil {
		return err
	}
	if l.PrivacyType == "public-view" {
		return errDownloadDisabled
	}
	return nil
}

//go:embed templates/*
var templateFS embed.FS

var (
	galleryTmpl *template.Template
	uploadTmpl  *template.Template
)

func init() {
	galleryTmpl = template.Must(
		template.New("gallery.html").ParseFS(templateFS, "templates/gallery.html"))
	uploadTmpl = template.Must(
		template.New("upload.html").ParseFS(templateFS, "templates/upload.html"))
}

// Handler holds dependencies for the photo sharing route group.
type Handler struct {
	client         *proxy.Client
	logger         *middleware.Logger
	maxUploadBytes int64
	devMode        bool
}

// NewHandler creates a Handler.
func NewHandler(client *proxy.Client, logger *middleware.Logger, maxUploadBytes int64, devMode bool) *Handler {
	return &Handler{client: client, logger: logger, maxUploadBytes: maxUploadBytes, devMode: devMode}
}

// errorPage is the data passed to templates when an error should be displayed.
type errorPage struct {
	Title  string
	Detail string
}

// galleryData is the template data for gallery.html.
type galleryData struct {
	AlbumName           string
	ItemCount           int
	SharingID           string
	CanDownload         bool
	IsPasswordProtected bool
	Error               *errorPage
}

// uploadData is the template data for upload.html.
type uploadData struct {
	SharingID   string
	Subject     string
	Description string
	Error       *errorPage
}

// itemJSON is a single photo/video entry in the APIList JSON response.
type itemJSON struct {
	ID            int64  `json:"id"`
	Filename      string `json:"filename"`
	IsVideo       bool   `json:"isVideo"`
	SizeHuman     string `json:"sizeHuman"`
	TakenDate     string `json:"takenDate"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	ThumbCacheKey string `json:"thumbCacheKey"`
	ThumbUnitID   int64  `json:"thumbUnitId"`
}

// BrowsePage handles GET /photo/mo/sharing/{id} — renders the gallery skeleton
// (album name/count embedded server-side) or a password prompt for locked shares.
// Photo listing itself is fetched client-side via /api/photo/list.
func (h *Handler) BrowsePage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		h.renderGalleryError(w, &errorPage{"Invalid Share", "The share link is invalid."})
		return
	}

	l, err := FetchLanding(r.Context(), h.client, basePathSharing, id)
	if err != nil {
		h.logger.Debug("photo landing error", middleware.F("err", err.Error()))
		h.renderGalleryError(w, photoErrorPage(err))
		return
	}

	// Show the password form without setting a session cookie — the
	// unauthenticated SID must not be persisted as if it were valid.
	// privacy_type is a property of the album itself (not the auth state), so it's
	// safe to embed here even before unlock — the JS keeps using this same value
	// after a successful unlock rather than re-fetching it.
	if l.IsPasswordProtected {
		h.renderGallery(w, &galleryData{
			SharingID:           id,
			IsPasswordProtected: true,
			CanDownload:         l.PrivacyType != "public-view",
		})
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "sid",
		Value:    l.SID,
		Path:     "/api/photo/",
		HttpOnly: true,
		Secure:   !h.devMode,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   l.SIDMaxAge,
		Expires:  l.SIDExpires,
	})

	name, itemCount, err := GetAlbumInfo(r.Context(), h.client, id, l.SID)
	if err != nil {
		h.logger.Debug("album info error", middleware.F("err", err.Error()))
		h.renderGalleryError(w, photoErrorPage(err))
		return
	}

	h.renderGallery(w, &galleryData{
		AlbumName:   name,
		ItemCount:   itemCount,
		SharingID:   id,
		CanDownload: l.PrivacyType != "public-view",
	})
}

// RequestPage handles GET /photo/mo/request/{id} — renders the upload page.
// Upload requests are always public: no password prompt or invite-only branch.
func (h *Handler) RequestPage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID(id) {
		h.renderUploadError(w, &errorPage{"Invalid Share", "The share link is invalid."})
		return
	}

	l, err := FetchLanding(r.Context(), h.client, basePathRequest, id)
	if err != nil {
		h.logger.Debug("photo request landing error", middleware.F("err", err.Error()))
		h.renderUploadError(w, photoErrorPage(err))
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "sid",
		Value:    l.SID,
		Path:     "/api/photo/",
		HttpOnly: true,
		Secure:   !h.devMode,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   l.SIDMaxAge,
		Expires:  l.SIDExpires,
	})

	subject, description, _, err := GetPhotoRequestInfo(r.Context(), h.client, id, l.SID)
	if err != nil {
		h.logger.Debug("photo request info error", middleware.F("err", err.Error()))
		h.renderUploadError(w, photoErrorPage(err))
		return
	}

	h.renderUpload(w, &uploadData{
		SharingID:   id,
		Subject:     subject,
		Description: description,
	})
}

// APIUnlock handles POST /api/photo/unlock — authenticates a password-protected
// photo share and sets the session cookie on success.
// Request body: JSON {"id": "...", "password": "..."}.
func (h *Handler) APIUnlock(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10) // 4 KB cap — id + password only

	var req struct {
		ID       string `json:"id"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "invalid request body",
		})
		return
	}

	if !validID(req.ID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "missing or invalid share id",
		})
		return
	}

	sr, err := LoginWithPassword(r.Context(), h.client, req.ID, req.Password)
	if err != nil {
		h.logger.Debug("photo unlock error", middleware.F("err", err.Error()))
		var synoErr *proxy.SynoError
		if errors.As(err, &synoErr) && synoErr.Code == 1001 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"success": false, "error": "Wrong password.",
			})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"success": false, "error": "Could not authenticate with the photo server.",
		})
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "sid",
		Value:    sr.Value,
		Path:     "/api/photo/",
		HttpOnly: true,
		Secure:   !h.devMode,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   sr.MaxAge,
		Expires:  sr.Expires,
	})

	// Fetch album info so the frontend can transition without a page reload.
	name, itemCount, err := GetAlbumInfo(r.Context(), h.client, req.ID, sr.Value)
	if err != nil {
		h.logger.Debug("photo unlock album info error", middleware.F("err", err.Error()))
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success":   true,
		"albumName": name,
		"itemCount": itemCount,
	})
}

// APIList handles GET /api/photo/list — returns one page of the photo/video listing.
// Query params: id, offset, limit. SID is read from the "sid" session cookie.
func (h *Handler) APIList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := q.Get("id")

	sidCookie, err := r.Cookie("sid")
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"success": false, "error": "no session — please reload the share page",
		})
		return
	}
	sid := sidCookie.Value

	if !validID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "missing or invalid id parameter",
		})
		return
	}

	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 100
	}

	items, err := ListItems(r.Context(), h.client, id, sid, offset, limit)
	if err != nil {
		h.logger.Debug("photo list error", middleware.F("err", err.Error()))
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"success": false, "error": photoErrorPage(err).Title,
		})
		return
	}

	out := make([]itemJSON, 0, len(items))
	for _, it := range items {
		out = append(out, itemJSON{
			ID:            it.ID,
			Filename:      it.Filename,
			IsVideo:       it.IsVideo,
			SizeHuman:     formatSize(it.FileSize),
			TakenDate:     time.Unix(it.Time, 0).UTC().Format("2 Jan 2006"),
			Width:         it.Width,
			Height:        it.Height,
			ThumbCacheKey: it.ThumbCacheKey,
			ThumbUnitID:   it.ThumbUnitID,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true, "items": out})
}

// APIThumbnail handles GET /api/photo/thumbnail — streams a thumbnail image from
// Synology. Query params: id (passphrase), unit (unit_id), cache_key, size (sm/m/xl).
// SID is read from the "sid" session cookie — Synology's own web client always sends
// it, even though doc/api/photos.md notes the endpoint doesn't strictly require it.
func (h *Handler) APIThumbnail(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := q.Get("id")
	cacheKey := q.Get("cache_key")
	size := q.Get("size")

	sidCookie, err := r.Cookie("sid")
	if err != nil {
		http.Error(w, "no session — please reload the share page", http.StatusUnauthorized)
		return
	}
	sid := sidCookie.Value

	if !validID(id) || cacheKey == "" {
		http.Error(w, "missing or invalid parameters", http.StatusBadRequest)
		return
	}
	if size != "sm" && size != "m" && size != "xl" {
		http.Error(w, "invalid size parameter", http.StatusBadRequest)
		return
	}
	unitID, err := strconv.ParseInt(q.Get("unit"), 10, 64)
	if err != nil {
		http.Error(w, "invalid unit parameter", http.StatusBadRequest)
		return
	}

	resp, err := FetchThumbnail(r.Context(), h.client, id, sid, unitID, cacheKey, size)
	if err != nil {
		h.logger.Debug("thumbnail fetch error", middleware.F("err", err.Error()))
		http.Error(w, "thumbnail unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, "thumbnail unavailable", http.StatusBadGateway)
		return
	}

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "image/jpeg")
	}
	// Thumbnails are keyed by an immutable cache_key — safe to cache aggressively.
	w.Header().Set("Cache-Control", "public, max-age=604800, immutable")

	io.Copy(w, resp.Body) //nolint:errcheck
}

// APIDownload handles GET /api/photo/download — streams one or more photos/videos.
// Query params: id, repeated item_id, optional name (used only for the download's
// display filename when a single item is requested). SID is read from the "sid"
// session cookie.
func (h *Handler) APIDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := q.Get("id")
	itemIDStrs := q["item_id"]
	nameHint := q.Get("name")

	sidCookie, err := r.Cookie("sid")
	if err != nil {
		http.Error(w, "no session — please reload the share page", http.StatusUnauthorized)
		return
	}
	sid := sidCookie.Value

	if !validID(id) || len(itemIDStrs) == 0 {
		http.Error(w, "missing or invalid id or item_id parameter", http.StatusBadRequest)
		return
	}
	if len(itemIDStrs) > 500 {
		http.Error(w, "too many items requested", http.StatusBadRequest)
		return
	}

	if err := h.checkDownloadAllowed(r.Context(), id); err != nil {
		if errors.Is(err, errDownloadDisabled) {
			http.Error(w, "downloads are not enabled for this share", http.StatusForbidden)
			return
		}
		h.logger.Debug("photo download permission check error", middleware.F("err", err.Error()))
		http.Error(w, photoErrorPage(err).Title, http.StatusBadGateway)
		return
	}

	itemIDs := make([]int64, 0, len(itemIDStrs))
	for _, s := range itemIDStrs {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			http.Error(w, "invalid item_id parameter", http.StatusBadRequest)
			return
		}
		itemIDs = append(itemIDs, n)
	}

	rangeHeader := r.Header.Get("Range")

	resp, err := DownloadItems(r.Context(), h.client, id, sid, itemIDs, rangeHeader)
	if err != nil {
		h.logger.Debug("photo download error", middleware.F("err", err.Error()))
		http.Error(w, photoErrorPage(err).Title, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// 206 Partial Content is expected for Range requests (used by <video> elements to
	// stream playback); 416 is a legitimate response to an out-of-bounds Range and is
	// forwarded as-is rather than mapped to a generic error.
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent, http.StatusRequestedRangeNotSatisfiable:
	default:
		http.Error(w, "file unavailable", http.StatusBadGateway)
		return
	}

	for _, hdr := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(hdr); v != "" {
			w.Header().Set(hdr, v)
		}
	}

	// Only force a download prompt for full, non-Range requests. Range requests are
	// how <video> elements stream playback, and Content-Disposition: attachment would
	// undermine that (the viewer points a <video> src directly at this endpoint).
	if rangeHeader == "" {
		outName := "download"
		switch {
		case len(itemIDs) > 1:
			outName = "photos.zip"
		case nameHint != "":
			outName = nameHint
		}
		setAttachmentHeader(w, outName)
	}

	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body) //nolint:errcheck
}

// APIDownloadAlbum handles GET /api/photo/download-album — streams the full shared
// album as a ZIP. Query params: id. SID is read from the "sid" session cookie.
func (h *Handler) APIDownloadAlbum(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := q.Get("id")

	sidCookie, err := r.Cookie("sid")
	if err != nil {
		http.Error(w, "no session — please reload the share page", http.StatusUnauthorized)
		return
	}
	sid := sidCookie.Value

	if !validID(id) {
		http.Error(w, "missing or invalid id parameter", http.StatusBadRequest)
		return
	}

	if err := h.checkDownloadAllowed(r.Context(), id); err != nil {
		if errors.Is(err, errDownloadDisabled) {
			http.Error(w, "downloads are not enabled for this share", http.StatusForbidden)
			return
		}
		h.logger.Debug("photo album download permission check error", middleware.F("err", err.Error()))
		http.Error(w, photoErrorPage(err).Title, http.StatusBadGateway)
		return
	}

	albumName, _, err := GetAlbumInfo(r.Context(), h.client, id, sid)
	if err != nil {
		h.logger.Debug("album download info error", middleware.F("err", err.Error()))
		http.Error(w, photoErrorPage(err).Title, http.StatusBadGateway)
		return
	}

	resp, err := DownloadAlbum(r.Context(), h.client, id, sid, albumName)
	if err != nil {
		h.logger.Debug("album download error", middleware.F("err", err.Error()))
		http.Error(w, photoErrorPage(err).Title, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, "album unavailable", http.StatusBadGateway)
		return
	}

	for _, hdr := range []string{"Content-Type", "Content-Length"} {
		if v := resp.Header.Get(hdr); v != "" {
			w.Header().Set(hdr, v)
		}
	}
	setAttachmentHeader(w, albumName+".zip")

	io.Copy(w, resp.Body) //nolint:errcheck
}

// APIUpload handles POST /api/photo/upload — streams a file to Synology's upload
// request endpoint. The request must be multipart/form-data. Fields must arrive in
// this order: id, guest_name, file — so all metadata is read before the file stream
// begins. SID is read from the "sid" session cookie.
func (h *Handler) APIUpload(w http.ResponseWriter, r *http.Request) {
	sidCookie, err := r.Cookie("sid")
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"success": false, "error": "no session — please reload the share page", "retryable": false,
		})
		return
	}
	sid := sidCookie.Value

	// Cap the total request body to prevent DoS via oversized uploads.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxUploadBytes)

	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "expected multipart/form-data request", "retryable": false,
		})
		return
	}

	var id, guestName, filename string
	var filePart io.Reader

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
					"success": false, "error": fmt.Sprintf("upload exceeds the %d-byte limit", maxErr.Limit), "retryable": false,
				})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"success": false, "error": "error reading upload", "retryable": false,
			})
			return
		}

		switch part.FormName() {
		case "id":
			b, _ := io.ReadAll(io.LimitReader(part, 64))
			id = strings.TrimSpace(string(b))
		case "guest_name":
			b, _ := io.ReadAll(io.LimitReader(part, 256))
			guestName = strings.Map(func(r rune) rune {
				if r == '"' || r == '\\' || isBidiOrInvisible(r) {
					return -1
				}
				return r
			}, strings.TrimSpace(string(b)))
		case "file":
			// Strip directory components from either Unix or Windows path styles.
			filename = path.Base(strings.ReplaceAll(part.FileName(), `\`, "/"))
			filename = strings.Map(func(r rune) rune {
				if r < 0x20 || r == 0x7f || isBidiOrInvisible(r) {
					return -1
				}
				return r
			}, filename)
			if filename == "" || filename == "." {
				filename = "upload"
			}
			filePart = part
		}

		if filePart != nil && id != "" && guestName != "" {
			break
		}
	}

	if !validID(id) || guestName == "" || filePart == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "missing or invalid id, guest_name, or file field", "retryable": false,
		})
		return
	}

	if err := UploadPhotoRequestItem(r.Context(), h.client, id, sid, guestName, filename, filePart); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"success": false, "error": fmt.Sprintf("file exceeds the %d-byte upload limit", maxErr.Limit), "retryable": false,
			})
			return
		}
		h.logger.Debug("photo upload error", middleware.F("err", err.Error()))
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"success": false, "error": photoErrorPage(err).Title, "retryable": isRetryableUploadError(err),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func (h *Handler) renderGallery(w http.ResponseWriter, data *galleryData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := galleryTmpl.Execute(w, data); err != nil {
		h.logger.Error("gallery template error", middleware.F("err", err.Error()))
	}
}

func (h *Handler) renderGalleryError(w http.ResponseWriter, ep *errorPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	galleryTmpl.Execute(w, &galleryData{Error: ep}) //nolint:errcheck
}

func (h *Handler) renderUpload(w http.ResponseWriter, data *uploadData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := uploadTmpl.Execute(w, data); err != nil {
		h.logger.Error("upload template error", middleware.F("err", err.Error()))
	}
}

func (h *Handler) renderUploadError(w http.ResponseWriter, ep *errorPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	uploadTmpl.Execute(w, &uploadData{Error: ep}) //nolint:errcheck
}

// isRetryableUploadError reports whether err is a transient failure between the
// app and the NAS rather than an error the NAS itself returned in its response.
func isRetryableUploadError(err error) bool {
	var synoErr *proxy.SynoError
	var httpErr *proxy.HTTPError
	var maxErr *http.MaxBytesError
	return !errors.As(err, &synoErr) && !errors.As(err, &httpErr) && !errors.As(err, &maxErr)
}

// photoErrorPage maps a Synology or network error to user-facing text.
func photoErrorPage(err error) *errorPage {
	if errors.Is(err, errInviteOnly) {
		return &errorPage{"Unsupported Share", "This share requires a Synology account to access, which is not supported."}
	}
	var httpErr *proxy.HTTPError
	if errors.As(err, &httpErr) {
		if httpErr.StatusCode == http.StatusNotFound {
			return &errorPage{"Share Not Found", "This share link does not exist or has been removed."}
		}
		return &errorPage{"Server Error", "The photo server returned an unexpected response."}
	}
	var synoErr *proxy.SynoError
	if errors.As(err, &synoErr) {
		switch synoErr.Code {
		case 114:
			return &errorPage{"Share Link Expired", "This share link has expired or is no longer valid."}
		case 105:
			return &errorPage{"Access Denied", "You don't have permission to access this share."}
		case 408:
			return &errorPage{"Invalid Share", "The share link is invalid."}
		}
		return &errorPage{"Share Error", "The photo server reported an unexpected error."}
	}
	return &errorPage{"Server Unavailable", "The photo server could not be reached. Please try again later."}
}

// formatSize converts a byte count to a human-readable string.
func formatSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// validID returns true if id is a plausible Synology passphrase/sharing ID:
// 1–64 alphanumeric characters (plus hyphen and underscore).
func validID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, ch := range id {
		if !unicode.IsLetter(ch) && !unicode.IsDigit(ch) && ch != '-' && ch != '_' {
			return false
		}
	}
	return true
}

// setAttachmentHeader sets Content-Disposition to force a download with a
// sanitized filename (RFC 5987/6266), matching sharing/handler.go's handling.
func setAttachmentHeader(w http.ResponseWriter, filename string) {
	safeName := strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r == '\r' || r == '\n' || isBidiOrInvisible(r) {
			return -1
		}
		return r
	}, filename)
	// ASCII fallback for filename= (non-ASCII replaced with _); RFC 5987 extended
	// value for filename*= so non-ASCII characters round-trip correctly per RFC 6266.
	asciiName := strings.Map(func(r rune) rune {
		if r > 0x7e {
			return '_'
		}
		return r
	}, safeName)
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		`attachment; filename="%s"; filename*=UTF-8''%s`, asciiName, rfc5987Encode(safeName)))
}

// rfc5987Encode percent-encodes s for use in an RFC 5987 / RFC 6266 filename*
// extended value (UTF-8 byte sequence, percent-encoded except for attr-chars).
func rfc5987Encode(s string) string {
	var b strings.Builder
	for _, r := range s {
		encoded := []byte(string(r))
		for _, byt := range encoded {
			if isRFC5987AttrChar(byt) {
				b.WriteByte(byt)
			} else {
				fmt.Fprintf(&b, "%%%02X", byt)
			}
		}
	}
	return b.String()
}

// isRFC5987AttrChar reports whether b is an RFC 5987 attr-char that needs no
// percent-encoding: ALPHA / DIGIT / "!" / "#" / "$" / "&" / "+" / "-" / "." /
// "^" / "_" / "`" / "|" / "~".
func isRFC5987AttrChar(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') ||
		b == '!' || b == '#' || b == '$' || b == '&' || b == '+' || b == '-' ||
		b == '.' || b == '^' || b == '_' || b == '`' || b == '|' || b == '~'
}

// isBidiOrInvisible reports whether r is a Unicode bidirectional control or
// invisible character that could be used to spoof displayed filenames.
func isBidiOrInvisible(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F: // zero-width and directional marks
		return true
	case r >= 0x202A && r <= 0x202E: // bidi embedding/override controls
		return true
	case r >= 0x2066 && r <= 0x2069: // bidi isolate controls
		return true
	case r == 0xFEFF: // zero-width no-break space / BOM
		return true
	}
	return false
}
