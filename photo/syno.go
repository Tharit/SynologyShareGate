package photo

import (
	"context"
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

// synoQuote wraps s in the literal double quotes Synology's API expects and
// strips embedded double quotes and backslashes to prevent malformed parameters.
func synoQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, "")
	s = strings.ReplaceAll(s, `"`, "")
	return `"` + s + `"`
}

// PhotoItem represents a single photo/video returned by SYNO.Foto.Browse.Item.list.
type PhotoItem struct {
	ID            int64
	Filename      string
	FileSize      int64
	Time          int64 // unix seconds
	IsVideo       bool
	Width         int
	Height        int
	ThumbCacheKey string
	ThumbUnitID   int64
}

// albumInfoResponse mirrors the Synology JSON for SYNO.Foto.Browse.Album.get.
type albumInfoResponse struct {
	Data struct {
		List []struct {
			ID        int64  `json:"id"`
			Name      string `json:"name"`
			ItemCount int    `json:"item_count"`
		} `json:"list"`
	} `json:"data"`
	Error struct {
		Code int `json:"code"`
	} `json:"error"`
	Success bool `json:"success"`
}

// GetAlbumInfo fetches the album name and item count for the shared album.
// sid must be a session established via FetchLanding (or LoginWithPassword).
func GetAlbumInfo(ctx context.Context, client *proxy.Client, passphrase, sid string) (name string, itemCount int, err error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	body := url.Values{}
	body.Set("api", "SYNO.Foto.Browse.Album")
	body.Set("method", "get")
	body.Set("version", "4")
	body.Set("passphrase", synoQuote(passphrase))
	body.Set("additional", `["sharing_info","flex_section","provider_count","thumbnail"]`)

	reqURL := client.BaseURL() + basePathSharing + "/webapi/entry.cgi/SYNO.Foto.Browse.Album"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(body.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("build album info request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "sharing_sid", Value: sid})
	req.Header.Set("X-Syno-Sharing", passphrase)

	resp, err := client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("album info request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB cap
	if err != nil {
		return "", 0, fmt.Errorf("read album info body: %w", err)
	}

	var ar albumInfoResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return "", 0, fmt.Errorf("parse album info response: %w", err)
	}
	if !ar.Success {
		return "", 0, proxy.SynoErrorFromCode(ar.Error.Code)
	}
	if len(ar.Data.List) == 0 {
		return "", 0, fmt.Errorf("album info response contained no album")
	}
	return ar.Data.List[0].Name, ar.Data.List[0].ItemCount, nil
}

// itemListResponse mirrors the Synology JSON for SYNO.Foto.Browse.Item.list.
type itemListResponse struct {
	Data struct {
		List []struct {
			ID         int64  `json:"id"`
			Filename   string `json:"filename"`
			FileSize   int64  `json:"filesize"`
			Time       int64  `json:"time"`
			Type       string `json:"type"`
			Additional struct {
				Resolution struct {
					Width  int `json:"width"`
					Height int `json:"height"`
				} `json:"resolution"`
				Thumbnail struct {
					CacheKey string `json:"cache_key"`
					UnitID   int64  `json:"unit_id"`
				} `json:"thumbnail"`
			} `json:"additional"`
		} `json:"list"`
	} `json:"data"`
	Error struct {
		Code int `json:"code"`
	} `json:"error"`
	Success bool `json:"success"`
}

// ListItems fetches one page of photos/videos in the shared album, ordered by
// capture time ascending. The caller detects the end of the album by receiving
// fewer than limit items back.
func ListItems(ctx context.Context, client *proxy.Client, passphrase, sid string, offset, limit int) ([]PhotoItem, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	body := url.Values{}
	body.Set("api", "SYNO.Foto.Browse.Item")
	body.Set("method", "list")
	body.Set("version", "4")
	body.Set("passphrase", synoQuote(passphrase))
	body.Set("offset", strconv.Itoa(offset))
	body.Set("limit", strconv.Itoa(limit))
	body.Set("sort_by", `"takentime"`)
	body.Set("sort_direction", `"asc"`)
	body.Set("additional", `["thumbnail","resolution","orientation","video_convert","video_meta","provider_user_id"]`)

	reqURL := client.BaseURL() + basePathSharing + "/webapi/entry.cgi/SYNO.Foto.Browse.Item"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(body.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build item list request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "sharing_sid", Value: sid})
	req.Header.Set("X-Syno-Sharing", passphrase)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("item list request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20)) // 8 MB cap
	if err != nil {
		return nil, fmt.Errorf("read item list body: %w", err)
	}

	var lr itemListResponse
	if err := json.Unmarshal(raw, &lr); err != nil {
		return nil, fmt.Errorf("parse item list response: %w", err)
	}
	if !lr.Success {
		return nil, proxy.SynoErrorFromCode(lr.Error.Code)
	}

	items := make([]PhotoItem, 0, len(lr.Data.List))
	for _, it := range lr.Data.List {
		items = append(items, PhotoItem{
			ID:            it.ID,
			Filename:      it.Filename,
			FileSize:      it.FileSize,
			Time:          it.Time,
			IsVideo:       it.Type == "video",
			Width:         it.Additional.Resolution.Width,
			Height:        it.Additional.Resolution.Height,
			ThumbCacheKey: it.Additional.Thumbnail.CacheKey,
			ThumbUnitID:   it.Additional.Thumbnail.UnitID,
		})
	}
	return items, nil
}

// FetchThumbnail streams a thumbnail image from Synology. size must be "sm", "m", or "xl".
// sid is the sharing_sid session cookie value — despite doc/api/photos.md noting this
// endpoint doesn't require it, Synology's own web client always sends it, and in
// practice the request is rejected (error 101) without it.
// The caller must close the returned response body.
func FetchThumbnail(ctx context.Context, client *proxy.Client, passphrase, sid string, unitID int64, cacheKey, size string) (*http.Response, error) {
	params := url.Values{}
	params.Set("id", strconv.FormatInt(unitID, 10))
	params.Set("cache_key", synoQuote(cacheKey))
	params.Set("type", `"unit"`)
	params.Set("size", synoQuote(size))
	params.Set("passphrase", synoQuote(passphrase))
	params.Set("_sharing_id", synoQuote(passphrase))

	reqURL := client.BaseURL() + "/photo/synofoto/api/v2/p/Thumbnail/get?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build thumbnail request: %w", err)
	}
	req.AddCookie(&http.Cookie{Name: "sharing_sid", Value: sid})

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("thumbnail request: %w", err)
	}
	return resp, nil
}

// DownloadItems initiates a streaming download of one or more photos/videos by item ID.
// A single item returns the original file; multiple items return a ZIP.
// sid is the sharing_sid session cookie value. rangeHeader, if non-empty, is forwarded
// as-is as the request's Range header — this is how the original frontend streams
// video playback (single-item requests only; ranges on a multi-item ZIP response
// aren't meaningful). A <video> element can only ever issue a GET for its src, never
// a POST, so a non-empty rangeHeader switches this request to GET-with-query-params
// (matching how the original frontend embeds this download URL directly as a
// <video src>); explicit downloads keep using POST-with-form-body as before.
// The caller must close the returned response body.
func DownloadItems(ctx context.Context, client *proxy.Client, passphrase, sid string, itemIDs []int64, rangeHeader string) (*http.Response, error) {
	idJSON, err := json.Marshal(itemIDs)
	if err != nil {
		return nil, fmt.Errorf("encode item ids: %w", err)
	}

	params := url.Values{}
	params.Set("api", "SYNO.Foto.Download")
	params.Set("method", "download")
	params.Set("version", "2")
	params.Set("force_download", "true")
	params.Set("item_id", string(idJSON))
	params.Set("passphrase", synoQuote(passphrase))
	params.Set("download_type", "source")
	params.Set("_sharing_id", synoQuote(passphrase))

	base := client.BaseURL() + basePathSharing + "/webapi/entry.cgi"

	var req *http.Request
	if rangeHeader != "" {
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, base+"?"+params.Encode(), nil)
	} else {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, base, strings.NewReader(params.Encode()))
	}
	if err != nil {
		return nil, fmt.Errorf("build download request: %w", err)
	}
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	} else {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.AddCookie(&http.Cookie{Name: "sharing_sid", Value: sid})
	req.Header.Set("X-Syno-Sharing", passphrase)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download request: %w", err)
	}
	return resp, nil
}

// DownloadAlbum initiates a streaming ZIP download of the entire shared album.
// sid is the sharing_sid session cookie value. The caller must close the returned
// response body.
func DownloadAlbum(ctx context.Context, client *proxy.Client, passphrase, sid, albumName string) (*http.Response, error) {
	body := url.Values{}
	body.Set("passphrase", synoQuote(passphrase))
	body.Set("download_type", "source")
	body.Set("api", "SYNO.Foto.Browse.Album")
	body.Set("method", "download")
	body.Set("version", "2")
	body.Set("_sharing_id", synoQuote(passphrase))

	basename := albumName + ".zip"
	reqURL := client.BaseURL() + basePathSharing + "/webapi/entry.cgi/" + url.PathEscape(basename)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(body.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build album download request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// No X-Syno-Sharing header for album download — auth is via the sharing_sid cookie only.
	req.AddCookie(&http.Cookie{Name: "sharing_sid", Value: sid})

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("album download request: %w", err)
	}
	return resp, nil
}

// requestInfoResponse mirrors the Synology JSON for
// SYNO.Foto.Sharing.Passphrase.get_photo_request_info.
type requestInfoResponse struct {
	Data struct {
		Subject       string `json:"subject"`
		Description   string `json:"description"`
		FilesizeLimit int64  `json:"filesize_limit"`
	} `json:"data"`
	Error struct {
		Code int `json:"code"`
	} `json:"error"`
	Success bool `json:"success"`
}

// GetPhotoRequestInfo fetches the subject/description/size-limit metadata for an
// upload request share. filesizeLimit of 0 means no limit. sid is the sharing_sid
// session cookie value captured from the request landing page — like every other
// Photos API call except Login, this endpoint rejects requests missing it (error 101).
func GetPhotoRequestInfo(ctx context.Context, client *proxy.Client, passphrase, sid string) (subject, description string, filesizeLimit int64, err error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	body := url.Values{}
	body.Set("api", "SYNO.Foto.Sharing.Passphrase")
	body.Set("method", "get_photo_request_info")
	body.Set("version", "1")
	body.Set("passphrase", synoQuote(passphrase))

	reqURL := client.BaseURL() + basePathRequest + "/webapi/entry.cgi/SYNO.Foto.Sharing.Passphrase"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(body.Encode()))
	if err != nil {
		return "", "", 0, fmt.Errorf("build request info request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "sharing_sid", Value: sid})
	req.Header.Set("X-Syno-Sharing", passphrase)

	resp, err := client.Do(req)
	if err != nil {
		return "", "", 0, fmt.Errorf("request info request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10)) // 64 KB cap
	if err != nil {
		return "", "", 0, fmt.Errorf("read request info body: %w", err)
	}

	var rr requestInfoResponse
	if err := json.Unmarshal(raw, &rr); err != nil {
		return "", "", 0, fmt.Errorf("parse request info response: %w", err)
	}
	if !rr.Success {
		return "", "", 0, proxy.SynoErrorFromCode(rr.Error.Code)
	}
	return rr.Data.Subject, rr.Data.Description, rr.Data.FilesizeLimit, nil
}

// uploadResponse mirrors the Synology JSON for SYNO.Foto.Upload.PhotoRequestItem.
type uploadResponse struct {
	Error struct {
		Code int `json:"code"`
	} `json:"error"`
	Success bool `json:"success"`
}

// UploadPhotoRequestItem streams a file from src to the Synology photo upload-request
// endpoint. sid is the sharing_sid session cookie value captured from the request
// landing page. No client-generated thumbnails are sent — the server is assumed to
// generate them itself (per doc/api/photos.md, those fields are likely optional).
func UploadPhotoRequestItem(ctx context.Context, client *proxy.Client, passphrase, sid, guestName, filename string, src io.Reader) error {
	params := url.Values{}
	params.Set("api", "SYNO.Foto.Upload.PhotoRequestItem")
	params.Set("method", "upload")
	params.Set("version", "1")

	reqURL := client.BaseURL() + basePathRequest + "/webapi/entry.cgi?" + params.Encode()

	// Use io.Pipe to stream the multipart body without buffering the file in memory.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)

	go func() {
		var closeErr error
		defer func() {
			mw.Close()
			pw.CloseWithError(closeErr)
		}()

		// passphrase/guest_name/name are sent JSON-quoted (literal wrapping quotes),
		// matching the exact wire format documented in doc/api/photos.md.
		fields := map[string]string{
			"passphrase": synoQuote(passphrase),
			"guest_name": synoQuote(guestName),
			"name":       synoQuote(filename),
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
	req.Header.Set("X-Syno-Sharing", passphrase)

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
