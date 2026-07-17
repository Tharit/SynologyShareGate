package drive

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

// errDownloadDisabled is returned when a share's permissions don't allow
// downloads (view-only tier). Synology's own download endpoint rejects these
// requests with an opaque error, so the check needs to happen before we ever
// call it, both to give a clear response and to avoid a wasted upstream call.
var errDownloadDisabled = errors.New("downloads are not enabled for this share")

// checkBrowseDownloadAllowed re-verifies the share's root capabilities before
// forwarding a download request to the NAS. This proxy keeps no server-side
// session state, so the only source of truth for "is this share downloadable
// right now" is the NAS itself — a stale or forged client-side canDownload
// value must never be trusted for this check.
func (h *Handler) checkBrowseDownloadAllowed(ctx context.Context, id, link, sid string) error {
	boot, err := fetchBootstrap(ctx, h.client, browseAPIBase(id, link), id, link, sid)
	if err != nil {
		return err
	}
	if boot.ErrCode != 0 || boot.File == nil || !boot.File.Capabilities.CanDownload {
		return errDownloadDisabled
	}
	return nil
}

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

// Handler holds dependencies for the drive route group.
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
	PermanentLink       string
	SharingLink         string
	ShareName           string
	IsFolder            bool
	RootFileID          string
	CanDownload         bool
	IsPasswordProtected bool
	FileSizeHuman       string // only set for single-file shares
	ModTime             string // only set for single-file shares
	Error               *errorPage
}

// uploadData is the template data for upload.html.
type uploadData struct {
	FileRequestID       string
	SharingLink         string
	Title               string
	Description         string
	TargetFolderID      string
	IsPasswordProtected bool
	Error               *errorPage
}

// fileJSONEntry is a single file/folder entry in the APIList JSON response.
type fileJSONEntry struct {
	Name      string `json:"name"`
	FileID    string `json:"file_id"`
	IsDir     bool   `json:"is_dir"`
	SizeHuman string `json:"size_human"`
	ModTime   string `json:"mtime"`
}

// BrowseByLink handles GET /drive/d/s/{permanentLink}/{sharingLink}.
func (h *Handler) BrowseByLink(w http.ResponseWriter, r *http.Request) {
	h.browsePage(w, r, r.PathValue("permanentLink"), r.PathValue("sharingLink"))
}

// BrowseInviteOnly handles GET /drive/d/f/{permanentLink} — invite-only shares
// always fail with "requires a Synology account login", detected via
// getDriveErrCode() == 1002 (no HTTP redirect, unlike Photos).
func (h *Handler) BrowseInviteOnly(w http.ResponseWriter, r *http.Request) {
	h.browsePage(w, r, r.PathValue("permanentLink"), "")
}

func (h *Handler) browsePage(w http.ResponseWriter, r *http.Request, permanentLink, sharingLink string) {
	if !validID(permanentLink) || (sharingLink != "" && !validID(sharingLink)) {
		h.renderBrowseError(w, &errorPage{"Invalid Share", "The share link is invalid."})
		return
	}

	bc, err := GetBrowseContext(r.Context(), h.client, permanentLink, sharingLink, "")
	if err != nil {
		h.logger.Debug("drive browse context error", middleware.F("err", err.Error()))
		h.renderBrowseError(w, driveErrorPage(err))
		return
	}

	// Show the password form without setting a session cookie — the
	// unauthenticated token must not be persisted as if it were valid.
	if bc.ErrCode == 1037 {
		h.renderBrowse(w, &browseData{
			PermanentLink:       permanentLink,
			SharingLink:         sharingLink,
			IsPasswordProtected: true,
		})
		return
	}

	if bc.ErrCode != 0 || bc.Root == nil {
		h.renderBrowseError(w, &errorPage{"Share Error", fmt.Sprintf("The Drive server returned an unexpected error (%d).", bc.ErrCode)})
		return
	}

	h.setBrowseCookie(w, bc.Token, bc.TokenMaxAge, bc.TokenExpires)
	h.renderBrowse(w, browseDataFromRoot(permanentLink, sharingLink, bc.Root))
}

func browseDataFromRoot(permanentLink, sharingLink string, root *DriveNode) *browseData {
	data := &browseData{
		PermanentLink: permanentLink,
		SharingLink:   sharingLink,
		ShareName:     root.Name,
		IsFolder:      root.IsDir(),
		RootFileID:    root.FileID,
		CanDownload:   root.Capabilities.CanDownload,
	}
	if !root.IsDir() {
		data.FileSizeHuman = formatSize(root.Size)
		data.ModTime = time.Unix(root.ModifiedTime, 0).UTC().Format("2 Jan 2006")
	}
	return data
}

// RequestPage handles GET /drive/d/r/{fileRequestID}/{sharingLink} — renders the upload page.
func (h *Handler) RequestPage(w http.ResponseWriter, r *http.Request) {
	fileRequestID := r.PathValue("fileRequestID")
	sharingLink := r.PathValue("sharingLink")
	if !validID(fileRequestID) || !validID(sharingLink) {
		h.renderUploadError(w, &errorPage{"Invalid Share", "The share link is invalid."})
		return
	}

	rl, tr, err := FetchRequestLanding(r.Context(), h.client, fileRequestID, sharingLink)
	if err != nil {
		h.logger.Debug("drive request landing error", middleware.F("err", err.Error()))
		h.renderUploadError(w, driveErrorPage(err))
		return
	}

	if rl.State == "file_request_password" {
		h.renderUpload(w, &uploadData{
			FileRequestID:       fileRequestID,
			SharingLink:         sharingLink,
			Title:               rl.Title,
			Description:         rl.Description,
			TargetFolderID:      rl.FileID,
			IsPasswordProtected: true,
		})
		return
	}

	h.setUploadCookie(w, tr.Value, tr.MaxAge, tr.Expires)
	h.renderUpload(w, &uploadData{
		FileRequestID:  fileRequestID,
		SharingLink:    sharingLink,
		Title:          rl.Title,
		Description:    rl.Description,
		TargetFolderID: rl.FileID,
	})
}

// APIUnlockBrowse handles POST /api/drive/browse/unlock.
// Request body: JSON {"id": permanent_link, "link": sharing_link, "password": "..."}.
func (h *Handler) APIUnlockBrowse(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10) // 4 KB cap

	var req struct {
		ID       string `json:"id"`
		Link     string `json:"link"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "invalid request body",
		})
		return
	}
	if !validID(req.ID) || !validID(req.Link) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "missing or invalid share id",
		})
		return
	}

	tr, err := AuthBrowsePassword(r.Context(), h.client, req.ID, req.Link, req.Password)
	if err != nil {
		h.logger.Debug("drive browse unlock error", middleware.F("err", err.Error()))
		var synoErr *proxy.SynoError
		if errors.As(err, &synoErr) && synoErr.Code == 1037 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"success": false, "error": "Wrong password.",
			})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"success": false, "error": "Could not authenticate with the Drive server.",
		})
		return
	}

	h.setBrowseCookie(w, tr.Value, tr.MaxAge, tr.Expires)

	// Fetch the root node so the frontend can transition without a page reload.
	boot, err := fetchBootstrap(r.Context(), h.client, browseAPIBase(req.ID, req.Link), req.ID, req.Link, tr.Value)
	if err != nil || boot.File == nil {
		if err != nil {
			h.logger.Debug("drive unlock bootstrap error", middleware.F("err", err.Error()))
		}
		writeJSON(w, http.StatusOK, map[string]any{"success": true})
		return
	}
	data := browseDataFromRoot(req.ID, req.Link, boot.File)
	writeJSON(w, http.StatusOK, map[string]any{
		"success":     true,
		"shareName":   data.ShareName,
		"isFolder":    data.IsFolder,
		"rootFileId":  data.RootFileID,
		"canDownload": data.CanDownload,
	})
}

// APIUnlockRequest handles POST /api/drive/upload/unlock.
// Request body: JSON {"id": file_request_id, "link": sharing_link, "password": "..."}.
func (h *Handler) APIUnlockRequest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10) // 4 KB cap

	var req struct {
		ID       string `json:"id"`
		Link     string `json:"link"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "invalid request body",
		})
		return
	}
	if !validID(req.ID) || !validID(req.Link) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "missing or invalid share id",
		})
		return
	}

	tr, err := AuthRequestPassword(r.Context(), h.client, req.ID, req.Link, req.Password)
	if err != nil {
		h.logger.Debug("drive upload unlock error", middleware.F("err", err.Error()))
		var synoErr *proxy.SynoError
		if errors.As(err, &synoErr) && synoErr.Code == 1037 {
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"success": false, "error": "Wrong password.",
			})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"success": false, "error": "Could not authenticate with the Drive server.",
		})
		return
	}

	h.setUploadCookie(w, tr.Value, tr.MaxAge, tr.Expires)
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// APIList handles GET /api/drive/browse/list — returns the children of a folder.
// Query params: id (permanent_link), link (sharing_link), file_id (folder to list).
func (h *Handler) APIList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, link, fileID := q.Get("id"), q.Get("link"), q.Get("file_id")

	sid, err := h.browseSID(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"success": false, "error": "no session — please reload the share page",
		})
		return
	}

	if !validID(id) || !validID(link) || !validFileID(fileID) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "missing or invalid id, link, or file_id parameter",
		})
		return
	}

	items, err := ListFiles(r.Context(), h.client, id, link, sid, fileID)
	if err != nil {
		h.logger.Debug("drive list error", middleware.F("err", err.Error()))
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"success": false, "error": driveErrorPage(err).Title,
		})
		return
	}

	files := make([]fileJSONEntry, 0, len(items))
	for _, it := range items {
		files = append(files, fileJSONEntry{
			Name:      it.Name,
			FileID:    it.FileID,
			IsDir:     it.IsDir(),
			SizeHuman: formatSize(it.Size),
			ModTime:   time.Unix(it.ModifiedTime, 0).UTC().Format("2 Jan 2006"),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true, "files": files})
}

// APIDownload handles GET /api/drive/browse/download — streams a single non-folder file.
// Query params: id, link, file_id, name (display filename hint).
func (h *Handler) APIDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, link, fileID, name := q.Get("id"), q.Get("link"), q.Get("file_id"), q.Get("name")

	sid, err := h.browseSID(r)
	if err != nil {
		http.Error(w, "no session — please reload the share page", http.StatusUnauthorized)
		return
	}

	if !validID(id) || !validID(link) || !validFileID(fileID) {
		http.Error(w, "missing or invalid id, link, or file_id parameter", http.StatusBadRequest)
		return
	}
	if name == "" {
		name = "download"
	}

	if err := h.checkBrowseDownloadAllowed(r.Context(), id, link, sid); err != nil {
		if errors.Is(err, errDownloadDisabled) {
			http.Error(w, "downloads are not enabled for this share", http.StatusForbidden)
			return
		}
		h.logger.Debug("drive download permission check error", middleware.F("err", err.Error()))
		http.Error(w, driveErrorPage(err).Title, http.StatusBadGateway)
		return
	}

	resp, err := DownloadFile(r.Context(), h.client, id, link, sid, fileID, name)
	if err != nil {
		h.logger.Debug("drive download error", middleware.F("err", err.Error()))
		http.Error(w, driveErrorPage(err).Title, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, "file unavailable", http.StatusBadGateway)
		return
	}

	for _, hdr := range []string{"Content-Type", "Content-Length"} {
		if v := resp.Header.Get(hdr); v != "" {
			w.Header().Set(hdr, v)
		}
	}
	setAttachmentHeader(w, name)

	io.Copy(w, resp.Body) //nolint:errcheck
}

// APIDownloadZip handles GET /api/drive/browse/download-zip — streams a folder
// or multi-select download as a ZIP via the 3-step async archive flow.
// Query params: id, link, repeated file_id, name (archive name hint, no extension).
func (h *Handler) APIDownloadZip(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, link, name := q.Get("id"), q.Get("link"), q.Get("name")
	fileIDs := q["file_id"]

	sid, err := h.browseSID(r)
	if err != nil {
		http.Error(w, "no session — please reload the share page", http.StatusUnauthorized)
		return
	}

	if !validID(id) || !validID(link) || len(fileIDs) == 0 {
		http.Error(w, "missing or invalid id, link, or file_id parameter", http.StatusBadRequest)
		return
	}
	if len(fileIDs) > 500 {
		http.Error(w, "too many items requested", http.StatusBadRequest)
		return
	}
	for _, fid := range fileIDs {
		if !validFileID(fid) {
			http.Error(w, "invalid file_id parameter", http.StatusBadRequest)
			return
		}
	}

	archiveName := sanitizeFilename(name)
	if archiveName == "" {
		archiveName = "Download"
	}

	if err := h.checkBrowseDownloadAllowed(r.Context(), id, link, sid); err != nil {
		if errors.Is(err, errDownloadDisabled) {
			http.Error(w, "downloads are not enabled for this share", http.StatusForbidden)
			return
		}
		h.logger.Debug("drive zip download permission check error", middleware.F("err", err.Error()))
		http.Error(w, driveErrorPage(err).Title, http.StatusBadGateway)
		return
	}

	resp, err := DownloadZip(r.Context(), h.client, id, link, sid, archiveName, fileIDs)
	if err != nil {
		h.logger.Debug("drive zip download error", middleware.F("err", err.Error()))
		http.Error(w, driveErrorPage(err).Title, http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, "archive unavailable", http.StatusBadGateway)
		return
	}

	for _, hdr := range []string{"Content-Type", "Content-Length"} {
		if v := resp.Header.Get(hdr); v != "" {
			w.Header().Set(hdr, v)
		}
	}
	setAttachmentHeader(w, archiveName+".zip")

	io.Copy(w, resp.Body) //nolint:errcheck
}

// APIUploadInit handles POST /api/drive/upload/init — creates the per-uploader
// subfolder once at the start of an upload batch and returns its file_id.
// Request body: JSON {"id", "link", "uploaderName", "targetFolderId"}.
func (h *Handler) APIUploadInit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10) // 4 KB cap

	var req struct {
		ID             string `json:"id"`
		Link           string `json:"link"`
		UploaderName   string `json:"uploaderName"`
		TargetFolderID string `json:"targetFolderId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "invalid request body",
		})
		return
	}

	sid, err := h.uploadSID(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"success": false, "error": "no session — please reload the share page",
		})
		return
	}

	uploaderName := sanitizeUploaderName(req.UploaderName)
	if !validID(req.ID) || !validID(req.Link) || !validFileID(req.TargetFolderID) || uploaderName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "missing or invalid id, link, targetFolderId, or uploaderName",
		})
		return
	}

	folderID, err := CreateUploadFolder(r.Context(), h.client, req.ID, req.Link, sid, req.TargetFolderID, uploaderName)
	if err != nil {
		h.logger.Debug("drive upload-init error", middleware.F("err", err.Error()))
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"success": false, "error": driveErrorPage(err).Title,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true, "folderId": folderID})
}

// APIUploadFile handles POST /api/drive/upload/file — streams one file into the
// uploader's subfolder using the slice-upload protocol.
// The request must be multipart/form-data. Fields must arrive in this order:
// id, link, folder_id, file_size, file — so all metadata is read before the
// file stream begins.
func (h *Handler) APIUploadFile(w http.ResponseWriter, r *http.Request) {
	sid, err := h.uploadSID(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"success": false, "error": "no session — please reload the share page", "retryable": false,
		})
		return
	}

	// Cap the total request body to prevent DoS via oversized uploads.
	r.Body = http.MaxBytesReader(w, r.Body, h.maxUploadBytes)

	mr, err := r.MultipartReader()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "expected multipart/form-data request", "retryable": false,
		})
		return
	}

	var id, link, folderID, filename string
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
		case "link":
			b, _ := io.ReadAll(io.LimitReader(part, 64))
			link = strings.TrimSpace(string(b))
		case "folder_id":
			b, _ := io.ReadAll(io.LimitReader(part, 32))
			folderID = strings.TrimSpace(string(b))
		case "file_size":
			b, _ := io.ReadAll(io.LimitReader(part, 20))
			if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil && n >= 0 {
				fileSize = n
			}
		case "file":
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

		if filePart != nil && id != "" && link != "" && folderID != "" {
			break
		}
	}

	if !validID(id) || !validID(link) || !validFileID(folderID) || filePart == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "missing or invalid id, link, folder_id, or file field", "retryable": false,
		})
		return
	}

	if err := UploadFileSlice(r.Context(), h.client, id, link, sid, folderID, filename, fileSize, filePart); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
				"success": false, "error": fmt.Sprintf("file exceeds the %d-byte upload limit", maxErr.Limit), "retryable": false,
			})
			return
		}
		h.logger.Debug("drive upload error", middleware.F("err", err.Error()))
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"success": false, "error": driveErrorPage(err).Title, "retryable": isRetryableUploadError(err),
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// APIUploadNotify handles POST /api/drive/upload/notify — notifies the share
// owner once after all files in a batch have finished uploading.
// Request body: JSON {"id", "link", "folderId", "uploaderName", "requestTitle", "filenames": [...]}.
func (h *Handler) APIUploadNotify(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10) // 64 KB cap — filename list

	var req struct {
		ID           string   `json:"id"`
		Link         string   `json:"link"`
		FolderID     string   `json:"folderId"`
		UploaderName string   `json:"uploaderName"`
		RequestTitle string   `json:"requestTitle"`
		Filenames    []string `json:"filenames"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "invalid request body",
		})
		return
	}

	sid, err := h.uploadSID(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"success": false, "error": "no session — please reload the share page",
		})
		return
	}

	uploaderName := sanitizeUploaderName(req.UploaderName)
	if !validID(req.ID) || !validID(req.Link) || !validFileID(req.FolderID) || uploaderName == "" || len(req.Filenames) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "missing or invalid id, link, folderId, uploaderName, or filenames",
		})
		return
	}

	if err := NotifyUploadRequest(r.Context(), h.client, req.ID, req.Link, sid, uploaderName, req.RequestTitle, req.FolderID, req.Filenames); err != nil {
		h.logger.Debug("drive notify error", middleware.F("err", err.Error()))
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"success": false, "error": driveErrorPage(err).Title,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// setBrowseCookie sets the drive browse session cookie, scoped to the
// /api/drive/browse/ path so it cannot collide with an upload-request session.
func (h *Handler) setBrowseCookie(w http.ResponseWriter, token string, maxAge int, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     "sid",
		Value:    token,
		Path:     "/api/drive/browse/",
		HttpOnly: true,
		Secure:   !h.devMode,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAge,
		Expires:  expires,
	})
}

// setUploadCookie sets the drive upload-request session cookie, scoped to the
// /api/drive/upload/ path so it cannot collide with a browse session.
func (h *Handler) setUploadCookie(w http.ResponseWriter, token string, maxAge int, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     "sid",
		Value:    token,
		Path:     "/api/drive/upload/",
		HttpOnly: true,
		Secure:   !h.devMode,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAge,
		Expires:  expires,
	})
}

func (h *Handler) browseSID(r *http.Request) (string, error) {
	c, err := r.Cookie("sid")
	if err != nil {
		return "", err
	}
	return c.Value, nil
}

func (h *Handler) uploadSID(r *http.Request) (string, error) {
	c, err := r.Cookie("sid")
	if err != nil {
		return "", err
	}
	return c.Value, nil
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

// driveErrorPage maps a Synology or network error to user-facing text.
func driveErrorPage(err error) *errorPage {
	if errors.Is(err, errUnsupportedShare) {
		return &errorPage{"Unsupported Share", "This share requires a Synology account to access, which is not supported."}
	}
	var httpErr *proxy.HTTPError
	if errors.As(err, &httpErr) {
		if httpErr.StatusCode == http.StatusNotFound {
			return &errorPage{"Share Not Found", "This share link does not exist or has been removed."}
		}
		return &errorPage{"Server Error", fmt.Sprintf("The Drive server returned an unexpected response (%d).", httpErr.StatusCode)}
	}
	var synoErr *proxy.SynoError
	if errors.As(err, &synoErr) {
		switch synoErr.Code {
		case 1037:
			return &errorPage{"Password Required", "This share is password-protected."}
		case 1054:
			return &errorPage{"Upload Failed", "The Drive server rejected the upload. Please try again."}
		}
		return &errorPage{"Share Error", fmt.Sprintf("The Drive server returned an error (%d).", synoErr.Code)}
	}
	return &errorPage{"Server Unavailable", "The Drive server could not be reached. Please try again later."}
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

// validID returns true if id is a plausible Synology Drive link/passphrase:
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

// validFileID returns true if id is a plausible Drive file_id: 1–32 ASCII digits.
func validFileID(id string) bool {
	if id == "" || len(id) > 32 {
		return false
	}
	for _, ch := range id {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// sanitizeUploaderName strips characters that could break the "id:{folder}/{name}"
// path parameter (notably '/', which has structural meaning there) or spoof the
// displayed name, mirroring sharing/photo's uploader-name sanitization.
func sanitizeUploaderName(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r == '/' || isBidiOrInvisible(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
}

// sanitizeFilename strips characters unsafe for a Content-Disposition filename
// or a synoQuote'd path segment.
func sanitizeFilename(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\\' || r == '/' || r == '\r' || r == '\n' || isBidiOrInvisible(r) {
			return -1
		}
		return r
	}, strings.TrimSpace(s))
}

// setAttachmentHeader sets Content-Disposition to force a download with a
// sanitized filename (RFC 5987/6266), matching sharing/photo's handling.
func setAttachmentHeader(w http.ResponseWriter, filename string) {
	safeName := sanitizeFilename(filename)
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
