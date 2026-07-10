package sharing

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/tharit/synologysharegate/proxy"
)

// encodeDLink hex-encodes a NAS file path for the Synology download dlink parameter.
// The result is wrapped in literal double quotes as Synology expects.
func encodeDLink(filePath string) string {
	return `"` + hex.EncodeToString([]byte(filePath)) + `"`
}

// synoQuote wraps s in the literal double quotes Synology's API expects and
// strips embedded double quotes and backslashes to prevent malformed or escaped parameters.
func synoQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, "")
	s = strings.ReplaceAll(s, `"`, "")
	return `"` + s + `"`
}

// FileEntry represents a single file or folder returned by SYNO.FolderSharing.List.
type FileEntry struct {
	Name   string
	Path   string
	IsDir  bool
	Size   int64
	MTime  time.Time
	Type   string // e.g. "JPG", "PDF"
}

// listResponse mirrors the Synology JSON for SYNO.FolderSharing.List.
type listResponse struct {
	Data struct {
		Files []struct {
			Name  string `json:"name"`
			Path  string `json:"path"`
			IsDir bool   `json:"isdir"`
			Additional struct {
				Size int64 `json:"size"`
				Time struct {
					MTime int64 `json:"mtime"`
				} `json:"time"`
				Type string `json:"type"`
			} `json:"additional"`
		} `json:"files"`
		Offset int `json:"offset"`
		Total  int `json:"total"`
	} `json:"data"`
	Error struct {
		Code int `json:"code"`
	} `json:"error"`
	Success bool `json:"success"`
}

// uploadCheckResponse mirrors the Synology JSON for SYNO.FileStation.CheckPermission.
type uploadCheckResponse struct {
	Data    struct{} `json:"data"`
	Error   struct {
		Code int `json:"code"`
	} `json:"error"`
	Success bool `json:"success"`
}

// uploadResponse mirrors the Synology JSON for SYNO.FileStation.Upload.
type uploadResponse struct {
	Error struct {
		Code int `json:"code"`
	} `json:"error"`
	Success bool `json:"success"`
}

// ListFiles fetches the file listing for the given folder path using the sharing ID.
// sid is the sharing_sid session cookie value obtained from GetContext.
func ListFiles(ctx context.Context, client *proxy.Client, sharingID, sid, folderPath string) ([]FileEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	body := url.Values{}
	body.Set("api", "SYNO.FolderSharing.List")
	body.Set("method", "list")
	body.Set("version", "2")
	body.Set("offset", "0")
	body.Set("limit", "1000")
	body.Set("sort_by", `"name"`)
	body.Set("sort_direction", `"ASC"`)
	body.Set("action", `"enum"`)
	body.Set("additional", `["size","time","type"]`)
	body.Set("filetype", `"all"`)
	body.Set("folder_path", synoQuote(folderPath))
	body.Set("_sharing_id", synoQuote(sharingID))

	reqURL := client.BaseURL() + "/sharing/webapi/entry.cgi"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL,
		strings.NewReader(body.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build list request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "sharing_sid", Value: sid})
	req.Header.Set("X-Syno-Sharing", sharingID)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MB cap
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

	entries := make([]FileEntry, 0, len(lr.Data.Files))
	for _, f := range lr.Data.Files {
		entries = append(entries, FileEntry{
			Name:  f.Name,
			Path:  f.Path,
			IsDir: f.IsDir,
			Size:  f.Additional.Size,
			MTime: time.Unix(f.Additional.Time.MTime, 0),
			Type:  f.Additional.Type,
		})
	}
	return entries, nil
}

// DownloadFolder initiates a streaming zip download of a folder from Synology.
// sid is the sharing_sid session cookie value. The caller must close the returned response body.
func DownloadFolder(ctx context.Context, client *proxy.Client, sharingID, sid, folderPath string) (*http.Response, error) {
	basename := path.Base(folderPath) + ".zip"
	dlink := encodeDLink(folderPath)

	params := url.Values{}
	params.Set("dlink", dlink)
	params.Set("noCache", strconv.FormatInt(time.Now().UnixMilli(), 10))
	params.Set("_sharing_id", synoQuote(sharingID))
	params.Set("api", "SYNO.FolderSharing.Download")
	params.Set("version", "2")
	params.Set("method", "download")
	params.Set("mode", "download")
	params.Set("stdhtml", "true")

	reqURL := client.BaseURL() + "/fsdownload/webapi/file_download.cgi/" +
		url.PathEscape(basename) + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build folder download request: %w", err)
	}
	req.AddCookie(&http.Cookie{Name: "sharing_sid", Value: sid})
	req.Header.Set("X-Syno-Sharing", sharingID)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("folder download request: %w", err)
	}
	return resp, nil
}

// DownloadFile initiates a streaming download from Synology.
// sid is the sharing_sid session cookie value. The caller must close the returned response body.
func DownloadFile(ctx context.Context, client *proxy.Client, sharingID, sid, filePath string) (*http.Response, error) {
	basename := path.Base(filePath)
	dlink := encodeDLink(filePath)

	params := url.Values{}
	params.Set("dlink", dlink)
	params.Set("noCache", strconv.FormatInt(time.Now().UnixMilli(), 10))
	params.Set("_sharing_id", synoQuote(sharingID))
	params.Set("api", "SYNO.FolderSharing.Download")
	params.Set("version", "2")
	params.Set("method", "download")
	params.Set("mode", "download")
	params.Set("stdhtml", "true")

	reqURL := client.BaseURL() + "/fsdownload/webapi/file_download.cgi/" +
		url.PathEscape(basename) + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build download request: %w", err)
	}
	req.AddCookie(&http.Cookie{Name: "sharing_sid", Value: sid})
	req.Header.Set("X-Syno-Sharing", sharingID)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download request: %w", err)
	}
	return resp, nil
}

// CheckUploadPermission verifies the uploader can write a file to the share.
// sid is the sharing_sid session cookie value.
func CheckUploadPermission(ctx context.Context, client *proxy.Client, sharingID, sid, uploaderName, filename string, size int64) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	body := url.Values{}
	body.Set("api", "SYNO.FileStation.CheckPermission")
	body.Set("method", "write")
	body.Set("version", "3")
	body.Set("sharing_id", synoQuote(sharingID))
	body.Set("uploader_name", synoQuote(uploaderName))
	body.Set("size", strconv.FormatInt(size, 10))
	body.Set("filename", synoQuote(filename))
	body.Set("overwrite", "true")

	reqURL := client.BaseURL() + "/sharing/webapi/entry.cgi/SYNO.FileStation.CheckPermission"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL,
		strings.NewReader(body.Encode()))
	if err != nil {
		return fmt.Errorf("build permission request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "sharing_sid", Value: sid})
	req.Header.Set("X-Syno-Sharing", sharingID)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("permission request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10)) // 64 KB cap
	if err != nil {
		return fmt.Errorf("read permission body: %w", err)
	}

	var cr uploadCheckResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return fmt.Errorf("parse permission response: %w", err)
	}
	if !cr.Success {
		return proxy.SynoErrorFromCode(cr.Error.Code)
	}
	return nil
}

// UploadFile streams a file from src to the Synology file upload endpoint.
// sid is the sharing_sid session cookie value.
// size and mtime should be the file's known size (bytes) and modification time.
func UploadFile(ctx context.Context, client *proxy.Client, sharingID, sid, uploaderName, filename string, size int64, mtime time.Time, src io.Reader) error {
	params := url.Values{}
	params.Set("api", "SYNO.FileStation.Upload")
	params.Set("method", "upload")
	params.Set("version", "2")
	params.Set("_sharing_id", synoQuote(sharingID))

	reqURL := client.BaseURL() + "/webapi/entry.cgi?" + params.Encode()

	// Use io.Pipe to stream the multipart body without buffering the file in memory.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		var closeErr error
		defer func() {
			mw.Close()
			pw.CloseWithError(closeErr)
		}()

		fields := map[string]string{
			"sharing_id":    sharingID,
			"uploader_name": uploaderName,
			"size":          strconv.FormatInt(size, 10),
			"mtime":         strconv.FormatInt(mtime.UnixMilli(), 10),
			"overwrite":     "true",
		}
		for k, v := range fields {
			if err := mw.WriteField(k, v); err != nil {
				closeErr = err
				return
			}
		}

		part, err := mw.CreateFormFile("file", filename)
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
		return fmt.Errorf("build upload request: %w", err)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+mw.Boundary())
	req.AddCookie(&http.Cookie{Name: "sharing_sid", Value: sid})
	req.Header.Set("X-Syno-Sharing", sharingID)

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("upload request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10)) // 64 KB cap
	if err != nil {
		return fmt.Errorf("read upload body: %w", err)
	}

	var ur uploadResponse
	if err := json.Unmarshal(raw, &ur); err != nil {
		return fmt.Errorf("parse upload response: %w", err)
	}
	if !ur.Success {
		return proxy.SynoErrorFromCode(ur.Error.Code)
	}
	return nil
}
