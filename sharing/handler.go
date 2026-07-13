package sharing

import (
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

//go:embed templates/*
var templateFS embed.FS

var (
	browseTmpl *template.Template
	uploadTmpl *template.Template
)

func init() {
	browseTmpl = template.Must(
		template.New("browse.html").ParseFS(templateFS, "templates/browse.html"))
	uploadTmpl = template.Must(
		template.New("upload.html").ParseFS(templateFS, "templates/upload.html"))
}

// Handler holds dependencies for the sharing route group.
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

// browseData is the template data for browse.html.
type browseData struct {
	ShareName           string
	SharingID           string
	IsPasswordProtected bool
	Error               *errorPage
}

// uploadData is the template data for upload.html.
type uploadData struct {
	SharingID   string
	RequestName string
	RequestInfo string
	Error       *errorPage
}

// fileJSONEntry is a single file/folder entry in the APIList JSON response.
type fileJSONEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	IsDir     bool   `json:"is_dir"`
	SizeHuman string `json:"size_human"`
	ModTime   string `json:"mtime"`
}

// Browse handles GET /sharing/{id} — renders the browse or upload skeleton.
// File listing is not included; the JS fetches it via /api/sharing/list.
// For password-protected shares a password prompt is rendered instead.
func (h *Handler) Browse(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validSharingID(id) {
		h.renderBrowseError(w, &errorPage{"Invalid Share", "The share link is invalid."})
		return
	}

	// Re-use an existing authenticated SID from the session cookie so that
	// returning visitors (including those who have already unlocked a
	// password-protected share) are not prompted again unnecessarily.
	sid := ""
	if c, err := r.Cookie("sid"); err == nil {
		sid = c.Value
	}

	sc, err := GetContext(r.Context(), h.client, h.logger, id, sid)
	if err != nil {
		h.logger.Debug("get context error", middleware.F("err", err.Error()))
		h.renderBrowseError(w, sharingErrorPage(err))
		return
	}

	// Show the password form without setting a session cookie — the
	// unauthenticated SID must not be persisted as if it were valid.
	if sc.SharingStatus == "password" {
		h.renderBrowse(w, &browseData{
			SharingID:           id,
			IsPasswordProtected: true,
		})
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "sid",
		Value:    sc.SID,
		Path:     "/api/sharing/",
		HttpOnly: true,
		Secure:   !h.devMode,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   sc.SIDMaxAge,
		Expires:  sc.SIDExpires,
	})

	if sc.IsUpload {
		h.renderUpload(w, &uploadData{
			SharingID:   id,
			RequestName: sc.Extra.RequestName,
			RequestInfo: sc.Extra.RequestInfo,
		})
		return
	}

	h.renderBrowse(w, &browseData{
		ShareName: sc.Extra.Filename,
		SharingID: id,
	})
}

// APIUnlock handles POST /api/sharing/unlock — authenticates a password-protected
// share and sets the session cookie on success.
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

	if !validSharingID(req.ID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "missing or invalid share id",
		})
		return
	}

	sr, err := LoginWithPassword(r.Context(), h.client, req.ID, req.Password)
	if err != nil {
		h.logger.Debug("unlock error", middleware.F("err", err.Error()))
		var synoErr *proxy.SynoError
		if errors.As(err, &synoErr) && synoErr.Code == 1001 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"success": false, "error": "Wrong password.",
			})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"success": false, "error": "Could not authenticate with the file server.",
		})
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "sid",
		Value:    sr.Value,
		Path:     "/api/sharing/",
		HttpOnly: true,
		Secure:   !h.devMode,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   sr.MaxAge,
		Expires:  sr.Expires,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// APIList handles GET /api/sharing/list — returns a JSON file listing.
// Query params: id, path. SID is read from the "sid" session cookie.
func (h *Handler) APIList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := q.Get("id")
	folderPath := q.Get("path")

	sidCookie, err := r.Cookie("sid")
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"success": false, "error": "no session — please reload the share page",
		})
		return
	}
	sid := sidCookie.Value

	if !validSharingID(id) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "missing or invalid id or path parameter",
		})
		return
	}

	sc, err := GetContext(r.Context(), h.client, h.logger, id, sid)
	if err != nil {
		h.logger.Debug("list get context error", middleware.F("err", err.Error()))
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"success": false, "error": sharingErrorPage(err).Title,
		})
		return
	}

	fullPath := path.Join(sc.RootPath, folderPath)

	if !withinRoot(fullPath, sc.RootPath) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"success": false, "error": "path is outside the share",
		})
		return
	}

	// Single-file share: SYNO.FolderSharing.List does not work on a file path.
	// Return a synthetic one-entry listing; the frontend treats it like a folder.
	if !sc.Extra.IsFolder && !sc.IsUpload {
		writeJSON(w, http.StatusOK, map[string]any{
			"success": true,
			"files": []fileJSONEntry{{
				Name:  path.Base(sc.RootPath),
				Path:  "/",
				IsDir: false,
			}},
		})
		return
	}

	entries, err := ListFiles(r.Context(), h.client, id, sid, fullPath)
	if err != nil {
		h.logger.Debug("list files error", middleware.F("err", err.Error()))
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"success": false, "error": sharingErrorPage(err).Title,
		})
		return
	}

	files := make([]fileJSONEntry, 0, len(entries))
	for _, e := range entries {
		relPath := strings.TrimPrefix(e.Path, sc.RootPath)
		if relPath == "" {
			relPath = "/"
		}
		files = append(files, fileJSONEntry{
			Name:      e.Name,
			Path:      relPath,
			IsDir:     e.IsDir,
			SizeHuman: formatSize(e.Size),
			ModTime:   e.MTime.Format("2 Jan 2006"),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true, "files": files})
}

// APIDownload handles GET /api/sharing/download — streams a file from Synology.
// Query params: id, path. SID is read from the "sid" session cookie.
func (h *Handler) APIDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := q.Get("id")
	filePath := q.Get("path")

	sidCookie, err := r.Cookie("sid")
	if err != nil {
		http.Error(w, "no session — please reload the share page", http.StatusUnauthorized)
		return
	}
	sid := sidCookie.Value

	if !validSharingID(id) || filePath == "" {
		http.Error(w, "missing or invalid id or path parameter", http.StatusBadRequest)
		return
	}

	sc, err := GetContext(r.Context(), h.client, h.logger, id, sid)
	if err != nil {
		h.logger.Debug("download get context error", middleware.F("err", err.Error()))
		http.Error(w, sharingErrorPage(err).Title, http.StatusBadGateway)
		return
	}

	fullPath := path.Join(sc.RootPath, filePath)

	if !withinRoot(fullPath, sc.RootPath) {
		http.Error(w, "path is outside the share", http.StatusForbidden)
		return
	}

	resp, err := DownloadFile(r.Context(), h.client, id, sid, fullPath)
	if err != nil {
		h.logger.Debug("download error", middleware.F("err", err.Error()))
		http.Error(w, sharingErrorPage(err).Title, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Reject non-2xx responses from Synology rather than proxying error bodies.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, "file unavailable", http.StatusBadGateway)
		return
	}

	// Propagate safe content headers from Synology.
	for _, hdr := range []string{"Content-Type", "Content-Length"} {
		if v := resp.Header.Get(hdr); v != "" {
			w.Header().Set(hdr, v)
		}
	}
	// Always force download; sanitize filename to prevent header injection.
	safeName := strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r == '\r' || r == '\n' || isBidiOrInvisible(r) {
			return -1
		}
		return r
	}, path.Base(fullPath))
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

	io.Copy(w, resp.Body) //nolint:errcheck
}

// APIDownloadFolder handles GET /api/sharing/download-folder — streams a folder as a zip from Synology.
// Query params: id, path (relative folder path; empty or "/" means the share root).
// SID is read from the "sid" session cookie.
func (h *Handler) APIDownloadFolder(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := q.Get("id")
	folderPath := q.Get("path")

	sidCookie, err := r.Cookie("sid")
	if err != nil {
		http.Error(w, "no session — please reload the share page", http.StatusUnauthorized)
		return
	}
	sid := sidCookie.Value

	if !validSharingID(id) {
		http.Error(w, "missing or invalid id parameter", http.StatusBadRequest)
		return
	}

	sc, err := GetContext(r.Context(), h.client, h.logger, id, sid)
	if err != nil {
		h.logger.Debug("download folder get context error", middleware.F("err", err.Error()))
		http.Error(w, sharingErrorPage(err).Title, http.StatusBadGateway)
		return
	}

	fullPath := path.Join(sc.RootPath, folderPath)
	if !withinRoot(fullPath, sc.RootPath) {
		http.Error(w, "path is outside the share", http.StatusForbidden)
		return
	}

	resp, err := DownloadFolder(r.Context(), h.client, id, sid, fullPath)
	if err != nil {
		h.logger.Debug("folder download error", middleware.F("err", err.Error()))
		http.Error(w, sharingErrorPage(err).Title, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, "folder unavailable", http.StatusBadGateway)
		return
	}

	for _, hdr := range []string{"Content-Type", "Content-Length"} {
		if v := resp.Header.Get(hdr); v != "" {
			w.Header().Set(hdr, v)
		}
	}

	safeName := strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r == '\r' || r == '\n' || isBidiOrInvisible(r) {
			return -1
		}
		return r
	}, path.Base(fullPath)+".zip")
	asciiName := strings.Map(func(r rune) rune {
		if r > 0x7e {
			return '_'
		}
		return r
	}, safeName)
	w.Header().Set("Content-Disposition", fmt.Sprintf(
		`attachment; filename="%s"; filename*=UTF-8''%s`, asciiName, rfc5987Encode(safeName)))

	io.Copy(w, resp.Body) //nolint:errcheck
}

// APIUpload handles POST /api/sharing/upload — streams a file to Synology.
// The request must be multipart/form-data. Fields must arrive in this order:
// id, uploader_name, file_size, file — so all metadata is read before the file stream begins.
// SID is read from the "sid" session cookie.
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

	var id, uploaderName, filename string
	var fileSize int64
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
		case "uploader_name":
			b, _ := io.ReadAll(io.LimitReader(part, 256))
			// Normalise once here; the same cleaned value is used in both
			// CheckUploadPermission and UploadFile so they see identical strings.
			uploaderName = strings.Map(func(r rune) rune {
				if r == '"' || r == '\\' || isBidiOrInvisible(r) {
					return -1
				}
				return r
			}, strings.TrimSpace(string(b)))
		case "file_size":
			// Browser supplies file.size before the file part so Synology's
			// CheckPermission receives an accurate size for quota checks.
			// Malicious clients can lie, but MaxBytesReader enforces the hard cap.
			b, _ := io.ReadAll(io.LimitReader(part, 20))
			if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && n >= 0 {
				fileSize = n
			}
		case "file":
			// Strip directory components from either Unix or Windows path styles.
			filename = path.Base(strings.ReplaceAll(part.FileName(), `\`, `/`))
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

		if filePart != nil && id != "" && uploaderName != "" {
			break
		}
	}

	if !validSharingID(id) || uploaderName == "" || filePart == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "missing or invalid id, uploader_name, or file field", "retryable": false,
		})
		return
	}

	if err := CheckUploadPermission(r.Context(), h.client, id, sid, uploaderName, filename, fileSize); err != nil {
		h.logger.Debug("upload permission denied", middleware.F("err", err.Error()))
		writeJSON(w, http.StatusForbidden, map[string]any{
			"success": false, "error": sharingErrorPage(err).Title, "retryable": isRetryableUploadError(err),
		})
		return
	}

	if err := UploadFile(r.Context(), h.client, id, sid, uploaderName, filename, fileSize, time.Now(), filePart); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"success": false, "error": fmt.Sprintf("file exceeds the %d-byte upload limit", maxErr.Limit), "retryable": false,
			})
			return
		}
		h.logger.Debug("upload error", middleware.F("err", err.Error()))
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"success": false, "error": sharingErrorPage(err).Title, "retryable": isRetryableUploadError(err),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

func (h *Handler) renderBrowse(w http.ResponseWriter, data *browseData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := browseTmpl.Execute(w, data); err != nil {
		h.logger.Error("browse template error", middleware.F("err", err.Error()))
	}
}

func (h *Handler) renderBrowseError(w http.ResponseWriter, ep *errorPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadGateway)
	browseTmpl.Execute(w, &browseData{Error: ep}) //nolint:errcheck
}

func (h *Handler) renderUpload(w http.ResponseWriter, data *uploadData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := uploadTmpl.Execute(w, data); err != nil {
		h.logger.Error("upload template error", middleware.F("err", err.Error()))
	}
}

// isRetryableUploadError reports whether err is a transient failure between the
// app and the NAS rather than an error the NAS itself returned in its response.
func isRetryableUploadError(err error) bool {
	var synoErr *proxy.SynoError
	var httpErr *proxy.HTTPError
	var maxErr *http.MaxBytesError
	return !errors.As(err, &synoErr) && !errors.As(err, &httpErr) && !errors.As(err, &maxErr)
}

// sharingErrorPage maps a Synology or network error to user-facing text.
func sharingErrorPage(err error) *errorPage {
	var httpErr *proxy.HTTPError
	if errors.As(err, &httpErr) {
		if httpErr.StatusCode == http.StatusNotFound {
			return &errorPage{"Share Not Found", "This share link does not exist or has been removed."}
		}
		return &errorPage{"Server Error", fmt.Sprintf("The file server returned an unexpected response (%d).", httpErr.StatusCode)}
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
		return &errorPage{"Share Error", fmt.Sprintf("The file server returned an error (%d).", synoErr.Code)}
	}
	return &errorPage{"Server Unavailable", "The file server could not be reached. Please try again later."}
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

// validSharingID returns true if id is a plausible Synology sharing ID:
// 1–64 alphanumeric characters (plus hyphen and underscore).
func validSharingID(id string) bool {
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

// withinRoot returns true if filePath is the share root or a descendant of it.
// Both paths are cleaned before comparison to prevent traversal via "..".
func withinRoot(filePath, rootPath string) bool {
	clean := path.Clean(filePath)
	root := path.Clean(rootPath)
	return clean == root || strings.HasPrefix(clean, root+"/")
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
