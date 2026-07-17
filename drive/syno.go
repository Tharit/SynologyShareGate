package drive

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tharit/synologysharegate/proxy"
)

// listResponse mirrors the Synology JSON for SYNO.SynologyDrive.Files.list.
type listResponse struct {
	Data struct {
		Items []DriveNode `json:"items"`
	} `json:"data"`
	Error struct {
		Code int `json:"code"`
	} `json:"error"`
	Success bool `json:"success"`
}

// ListFiles fetches the children of the folder identified by fileID.
func ListFiles(ctx context.Context, client *proxy.Client, permanentLink, sharingLink, token, fileID string) ([]DriveNode, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	body := url.Values{}
	body.Set("api", "SYNO.SynologyDrive.Files")
	body.Set("method", "list")
	body.Set("version", "2")
	body.Set("offset", "0")
	body.Set("limit", "1000")
	body.Set("filter", `{"include_transient":true}`)
	body.Set("sort_by", `"name"`)
	body.Set("sort_direction", `"asc"`)
	body.Set("path", synoQuote("id:"+fileID))
	body.Set("sharing_token", synoQuote(token))

	reqURL := client.BaseURL() + browseAPIBase(permanentLink, sharingLink) + "/webapi/entry.cgi"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(body.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build list request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: browseCookieName(sharingLink), Value: token})

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MB cap
	if err != nil {
		return nil, fmt.Errorf("read list body: %w", err)
	}

	var lr listResponse
	if err := json.Unmarshal(raw, &lr); err != nil {
		return nil, fmt.Errorf("parse list response: %w", err)
	}
	if !lr.Success {
		return nil, proxy.SynoErrorFromCode(lr.Error.Code)
	}
	return lr.Data.Items, nil
}

// DownloadFile streams a single non-folder file. The caller must close the
// returned response body.
func DownloadFile(ctx context.Context, client *proxy.Client, permanentLink, sharingLink, token, fileID, filename string) (*http.Response, error) {
	filesJSON, err := json.Marshal([]string{"id:" + fileID})
	if err != nil {
		return nil, fmt.Errorf("encode files param: %w", err)
	}

	params := url.Values{}
	params.Set("api", "SYNO.SynologyDrive.Files")
	params.Set("method", "download")
	params.Set("version", "2")
	params.Set("files", string(filesJSON))
	params.Set("force_download", "true")
	params.Set("is_preview", "false")
	params.Set("sharing_token", synoQuote(token))

	reqURL := client.BaseURL() + browseAPIBase(permanentLink, sharingLink) +
		"/webapi/entry.cgi/" + url.PathEscape(filename) + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build download request: %w", err)
	}
	req.AddCookie(&http.Cookie{Name: browseCookieName(sharingLink), Value: token})

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download request: %w", err)
	}
	return resp, nil
}

// zipTaskResponse mirrors the JSON shape shared by the dry-run/start-job calls
// of SYNO.SynologyDrive.Files.download.
type zipTaskResponse struct {
	Data struct {
		AsyncTaskID string `json:"async_task_id"`
	} `json:"data"`
	Error struct {
		Code int `json:"code"`
	} `json:"error"`
	Success bool `json:"success"`
}

// zipStatusResponse mirrors the JSON for SYNO.SynologyDrive.Tasks.get.
type zipStatusResponse struct {
	Data struct {
		Progress int    `json:"progress"`
		Status   string `json:"status"`
	} `json:"data"`
	Error struct {
		Code int `json:"code"`
	} `json:"error"`
	Success bool `json:"success"`
}

// startZipJob issues either the dry-run feasibility check or the real job-start
// call, depending on dryRun, and returns the parsed response.
func startZipJob(ctx context.Context, client *proxy.Client, permanentLink, sharingLink, token, archiveName string, fileIDs []string, dryRun bool) (zipTaskResponse, error) {
	filesArr := make([]string, len(fileIDs))
	for i, id := range fileIDs {
		filesArr[i] = "id:" + id
	}
	filesJSON, err := json.Marshal(filesArr)
	if err != nil {
		return zipTaskResponse{}, fmt.Errorf("encode files param: %w", err)
	}

	body := url.Values{}
	body.Set("api", "SYNO.SynologyDrive.Files")
	body.Set("method", "download")
	body.Set("version", "2")
	if dryRun {
		body.Set("dry_run", "true")
	}
	body.Set("archive_name", synoQuote(archiveName))
	body.Set("download_type", synoQuote("download"))
	body.Set("files", string(filesJSON))
	body.Set("force_download", "true")
	body.Set("json_error", "true")
	body.Set("sharing_token", synoQuote(token))

	reqURL := client.BaseURL() + browseAPIBase(permanentLink, sharingLink) + "/webapi/entry.cgi"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(body.Encode()))
	if err != nil {
		return zipTaskResponse{}, fmt.Errorf("build zip job request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: browseCookieName(sharingLink), Value: token})

	resp, err := client.Do(req)
	if err != nil {
		return zipTaskResponse{}, fmt.Errorf("zip job request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10)) // 64 KB cap
	if err != nil {
		return zipTaskResponse{}, fmt.Errorf("read zip job response: %w", err)
	}

	var tr zipTaskResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		return zipTaskResponse{}, fmt.Errorf("parse zip job response: %w", err)
	}
	if !tr.Success {
		return zipTaskResponse{}, proxy.SynoErrorFromCode(tr.Error.Code)
	}
	return tr, nil
}

// pollZipTask polls SYNO.SynologyDrive.Tasks.get until the task finishes.
func pollZipTask(ctx context.Context, client *proxy.Client, permanentLink, sharingLink, token, taskID string) error {
	base := client.BaseURL() + browseAPIBase(permanentLink, sharingLink) + "/webapi/entry.cgi"
	cookieName := browseCookieName(sharingLink)

	for {
		body := url.Values{}
		body.Set("api", "SYNO.SynologyDrive.Tasks")
		body.Set("method", "get")
		body.Set("version", "1")
		body.Set("task_id", synoQuote(taskID))
		body.Set("sharing_token", synoQuote(token))

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, strings.NewReader(body.Encode()))
		if err != nil {
			return fmt.Errorf("build task poll request: %w", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: cookieName, Value: token})

		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("task poll request: %w", err)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10)) // 64 KB cap
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("read task poll response: %w", err)
		}

		var sr zipStatusResponse
		if err := json.Unmarshal(raw, &sr); err != nil {
			return fmt.Errorf("parse task poll response: %w", err)
		}
		if !sr.Success {
			return proxy.SynoErrorFromCode(sr.Error.Code)
		}
		switch sr.Data.Status {
		case "finished":
			return nil
		case "failed", "error":
			return fmt.Errorf("archive task failed")
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(700 * time.Millisecond):
		}
	}
}

// DownloadZip runs the 3-step async archive flow (dry-run, start, poll) and
// returns a streaming response for the finished ZIP. fileIDs may be a single
// folder, or any mix of files/folders for a multi-select download. The caller
// must close the returned response body.
func DownloadZip(ctx context.Context, client *proxy.Client, permanentLink, sharingLink, token, archiveName string, fileIDs []string) (*http.Response, error) {
	dryCtx, dryCancel := context.WithTimeout(ctx, 30*time.Second)
	defer dryCancel()
	if _, err := startZipJob(dryCtx, client, permanentLink, sharingLink, token, archiveName, fileIDs, true); err != nil {
		return nil, fmt.Errorf("zip dry run: %w", err)
	}

	startCtx, startCancel := context.WithTimeout(ctx, 30*time.Second)
	defer startCancel()
	job, err := startZipJob(startCtx, client, permanentLink, sharingLink, token, archiveName, fileIDs, false)
	if err != nil {
		return nil, fmt.Errorf("zip start: %w", err)
	}
	if job.Data.AsyncTaskID == "" {
		return nil, fmt.Errorf("zip start returned no async_task_id")
	}

	pollCtx, pollCancel := context.WithTimeout(ctx, 10*time.Minute)
	defer pollCancel()
	if err := pollZipTask(pollCtx, client, permanentLink, sharingLink, token, job.Data.AsyncTaskID); err != nil {
		return nil, fmt.Errorf("zip poll: %w", err)
	}

	params := url.Values{}
	params.Set("api", "SYNO.SynologyDrive.Files")
	params.Set("method", "download")
	params.Set("version", "2")
	params.Set("task_id", synoQuote(job.Data.AsyncTaskID))
	params.Set("_dc", fmt.Sprintf("%d", time.Now().UnixMilli()))
	params.Set("sharing_token", synoQuote(token))

	reqURL := client.BaseURL() + browseAPIBase(permanentLink, sharingLink) +
		"/webapi/entry.cgi/SYNO.SynologyDrive.Files/" + url.PathEscape(archiveName) + ".zip?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build zip download request: %w", err)
	}
	req.AddCookie(&http.Cookie{Name: browseCookieName(sharingLink), Value: token})

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("zip download request: %w", err)
	}
	return resp, nil
}

// createResponse mirrors the Synology JSON for SYNO.SynologyDrive.Files.create.
type createResponse struct {
	Data  DriveNode `json:"data"`
	Error struct {
		Code int `json:"code"`
	} `json:"error"`
	Success bool `json:"success"`
}

// CreateUploadFolder creates a per-uploader subfolder inside the target folder,
// auto-renaming on conflict, and returns the new subfolder's file_id.
func CreateUploadFolder(ctx context.Context, client *proxy.Client, fileRequestID, sharingLink, token, targetFolderID, uploaderName string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	body := url.Values{}
	body.Set("api", "SYNO.SynologyDrive.Files")
	body.Set("method", "create")
	body.Set("version", "2")
	body.Set("type", `"folder"`)
	body.Set("path", synoQuote("id:"+targetFolderID+"/"+uploaderName))
	body.Set("conflict_action", `"autorename"`)
	body.Set("sharing_type", `"file_request"`)
	body.Set("sharing_token", synoQuote(token))

	reqURL := client.BaseURL() + requestAPIBase(fileRequestID) + "/webapi/entry.cgi"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(body.Encode()))
	if err != nil {
		return "", fmt.Errorf("build create-folder request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: requestCookieName(sharingLink), Value: token})

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("create-folder request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10)) // 64 KB cap
	if err != nil {
		return "", fmt.Errorf("read create-folder response: %w", err)
	}

	var cr createResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return "", fmt.Errorf("parse create-folder response: %w", err)
	}
	if !cr.Success {
		return "", proxy.SynoErrorFromCode(cr.Error.Code)
	}
	if cr.Data.FileID == "" {
		return "", fmt.Errorf("create-folder succeeded but response contained no file_id")
	}
	return cr.Data.FileID, nil
}

// genTmpFileID generates a plain 32-character lowercase hex string, the exact
// format SYNO.SynologyDrive.Files.upload's X-Tmp-File header requires — any
// other format (a UUID with dashes, a prefixed string) makes the reservation
// request fail with error 1054 ("blocked by flock").
func genTmpFileID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate tmp file id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// sliceUploadResponse mirrors the JSON shape shared by both slice-upload requests.
type sliceUploadResponse struct {
	Error struct {
		Code int `json:"code"`
	} `json:"error"`
	Success bool `json:"success"`
}

// sliceUploadRequest issues one step of the SLICEUPLOAD protocol. If reservedSize
// is non-negative, this is the reservation request (empty blob); otherwise it is
// the final request carrying the real file bytes from src.
func sliceUploadRequest(ctx context.Context, client *proxy.Client, fileRequestID, sharingLink, token, folderID, filename, tmpFile string, reservedSize int64, chunkEnd bool, src io.Reader) error {
	params := url.Values{}
	params.Set("api", "SYNO.SynologyDrive.Files")
	params.Set("method", "upload")
	params.Set("version", "2")
	params.Set("path", synoQuote("id:"+folderID+"/"+filename))
	params.Set("conflict_action", `"autorename"`)
	params.Set("type", `"file"`)
	params.Set("json_error", "true")
	if reservedSize >= 0 {
		params.Set("reserved_size", strconv.FormatInt(reservedSize, 10))
	}

	reqURL := client.BaseURL() + requestAPIBase(fileRequestID) +
		"/webapi/entry.cgi/SYNO.SynologyDrive.Files?" + params.Encode()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		var closeErr error
		defer func() {
			mw.Close()
			pw.CloseWithError(closeErr)
		}()

		// These four fields must be plain (non-JSON-quoted) form fields —
		// omitting sharing_token/sharing_type makes the final request fail
		// with error 1002 ("no access permission when create").
		now := time.Now()
		fields := map[string]string{
			"modified_time": fmt.Sprintf("%d.%03d", now.Unix(), now.Nanosecond()/1e6),
			"mute":          "false",
			"sharing_token": token,
			"sharing_type":  "file_request",
		}
		for k, v := range fields {
			if err := mw.WriteField(k, v); err != nil {
				closeErr = err
				return
			}
		}

		// The blob has no real filename — the real client appends it via
		// FormData with no third argument, defaulting to "blob". The actual
		// destination filename comes entirely from the path query parameter.
		part, err := mw.CreateFormFile("file", "blob")
		if err != nil {
			closeErr = err
			return
		}
		if _, err := io.Copy(part, src); err != nil {
			closeErr = err
		}
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, pr)
	if err != nil {
		pr.CloseWithError(err)
		return fmt.Errorf("build slice-upload request: %w", err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+mw.Boundary())
	req.Header.Set("X-Type-Name", "SLICEUPLOAD")
	req.Header.Set("X-File-Chunk-End", strconv.FormatBool(chunkEnd))
	req.Header.Set("X-Tmp-File", tmpFile)
	req.AddCookie(&http.Cookie{Name: requestCookieName(sharingLink), Value: token})

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("slice-upload request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10)) // 64 KB cap
	if err != nil {
		return fmt.Errorf("read slice-upload response: %w", err)
	}

	var ur sliceUploadResponse
	if err := json.Unmarshal(raw, &ur); err != nil {
		return fmt.Errorf("parse slice-upload response: %w", err)
	}
	if !ur.Success {
		return proxy.SynoErrorFromCode(ur.Error.Code)
	}
	return nil
}

// UploadFileSlice uploads a file to an upload-request share's per-uploader
// subfolder using the two-step SLICEUPLOAD protocol: an empty reservation
// request, then the real bytes from src.
func UploadFileSlice(ctx context.Context, client *proxy.Client, fileRequestID, sharingLink, token, folderID, filename string, size int64, src io.Reader) error {
	tmpFile, err := genTmpFileID()
	if err != nil {
		return err
	}

	reserveCtx, reserveCancel := context.WithTimeout(ctx, 30*time.Second)
	defer reserveCancel()
	if err := sliceUploadRequest(reserveCtx, client, fileRequestID, sharingLink, token, folderID, filename, tmpFile, size, false, http.NoBody); err != nil {
		return fmt.Errorf("upload reservation: %w", err)
	}

	if err := sliceUploadRequest(ctx, client, fileRequestID, sharingLink, token, folderID, filename, tmpFile, -1, true, src); err != nil {
		return fmt.Errorf("upload finalize: %w", err)
	}
	return nil
}

// notifyResponse mirrors the Synology JSON for SYNO.SynologyDrive.FileRequest.Public.notify.
type notifyResponse struct {
	Error struct {
		Code int `json:"code"`
	} `json:"error"`
	Success bool `json:"success"`
}

// NotifyUploadRequest notifies the share owner that a batch of files has been
// uploaded. Sent once after all files in a batch have finished, not per-file.
func NotifyUploadRequest(ctx context.Context, client *proxy.Client, fileRequestID, sharingLink, token, uploader, requestTitle, folderID string, filenames []string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	itemsJSON, err := json.Marshal(filenames)
	if err != nil {
		return fmt.Errorf("encode upload_items: %w", err)
	}

	body := url.Values{}
	body.Set("api", "SYNO.SynologyDrive.FileRequest.Public")
	body.Set("method", "notify")
	body.Set("version", "1")
	body.Set("uploader", synoQuote(uploader))
	body.Set("request_title", synoQuote(requestTitle))
	body.Set("upload_items", string(itemsJSON))
	body.Set("path", synoQuote("id:"+folderID))
	body.Set("sharing_token", synoQuote(token))

	reqURL := client.BaseURL() + requestAPIBase(fileRequestID) + "/webapi/entry.cgi"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(body.Encode()))
	if err != nil {
		return fmt.Errorf("build notify request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: requestCookieName(sharingLink), Value: token})

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("notify request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10)) // 64 KB cap
	if err != nil {
		return fmt.Errorf("read notify response: %w", err)
	}

	var nr notifyResponse
	if err := json.Unmarshal(raw, &nr); err != nil {
		return fmt.Errorf("parse notify response: %w", err)
	}
	if !nr.Success {
		return proxy.SynoErrorFromCode(nr.Error.Code)
	}
	return nil
}
