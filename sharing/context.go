package sharing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tharit/synologysharegate/middleware"
	"github.com/tharit/synologysharegate/proxy"
)

// SharingSession is the parsed SYNO.SDS.Session object.
type SharingSession struct {
	SharingID     string `json:"sharing_id"`
	SharingStatus string `json:"sharing_status"`
	Hostname      string `json:"hostname"`
}

// ExtraSession is the parsed SYNO.SDS.ExtraSession object.
type ExtraSession struct {
	IsSharingUpload bool   `json:"is_sharing_upload"`
	RequestInfo     string `json:"request_info"`
	RequestName     string `json:"request_name"`
	Filename        string `json:"filename"` // root folder name (browse shares)
	IsFolder        bool   `json:"is_folder"`
	Status          int    `json:"status"`
}

// SharingContext holds the decoded sharing context for a given sharing ID.
type SharingContext struct {
	SharingID string
	Hostname  string
	Extra     ExtraSession
	// SharingStatus mirrors SYNO.SDS.Session.sharing_status. It is "password"
	// when the share requires a password before content can be accessed.
	SharingStatus string
	// IsUpload is true when this share is a file-request (upload) share.
	IsUpload bool
	// RootPath is the NAS folder path to list for browse shares.
	RootPath string
	// SID is the sharing_sid session cookie value, required for all Synology API calls.
	SID string
	// SIDMaxAge and SIDExpires mirror the TTL attributes Synology set on its
	// sharing_sid cookie. Both are zero if Synology did not set a TTL.
	SIDMaxAge  int
	SIDExpires time.Time
}

// sidResult carries the sharing_sid value and any TTL attributes from the NAS.
type sidResult struct {
	Value   string
	MaxAge  int
	Expires time.Time
}

// FetchSID hits the Synology sharing HTML page for the given sharing ID and returns
// the sharing_sid session cookie together with any TTL attributes set by the NAS.
// The TTL fields are zero if Synology did not set them.
func FetchSID(ctx context.Context, client *proxy.Client, sharingID string) (sidResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	reqURL := client.BaseURL() + "/sharing/" + sharingID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return sidResult{}, fmt.Errorf("build sid request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return sidResult{}, fmt.Errorf("sid request: %w", err)
	}
	// Discard body — we only need the Set-Cookie header.
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()

	if resp.StatusCode >= 400 {
		return sidResult{}, &proxy.HTTPError{StatusCode: resp.StatusCode}
	}

	for _, cookie := range resp.Cookies() {
		if cookie.Name == "sharing_sid" {
			return sidResult{
				Value:   cookie.Value,
				MaxAge:  cookie.MaxAge,
				Expires: cookie.Expires,
			}, nil
		}
	}
	return sidResult{}, fmt.Errorf("sharing_sid cookie not found in response from %s", reqURL)
}

// GetContext fetches and parses the sharing context for the given sharing ID.
// If sid is non-empty it is used directly and FetchSID is skipped. If sid is
// empty, a fresh SID is fetched first. The SID used is always included in the
// returned SharingContext.
//
// When the share is password-protected the returned SharingContext will have
// SharingStatus == "password" and an empty RootPath; the caller must prompt
// for a password and call LoginWithPassword before accessing share content.
func GetContext(ctx context.Context, client *proxy.Client, logger *middleware.Logger, sharingID, sid string) (*SharingContext, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var sidMaxAge int
	var sidExpires time.Time
	if sid == "" {
		sr, err := FetchSID(ctx, client, sharingID)
		if err != nil {
			return nil, fmt.Errorf("fetch session: %w", err)
		}
		sid, sidMaxAge, sidExpires = sr.Value, sr.MaxAge, sr.Expires
	}

	params := url.Values{}
	params.Set("api", "SYNO.Core.Sharing.Session")
	params.Set("version", "1")
	params.Set("method", "get")
	params.Set("sharing_id", synoQuote(sharingID))
	params.Set("sharing_status", `"none"`)
	params.Set("v", fmt.Sprintf("%d", time.Now().Unix()))

	reqURL := client.BaseURL() + "/sharing/webapi/entry.cgi?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build context request: %w", err)
	}
	req.AddCookie(&http.Cookie{Name: "sharing_sid", Value: sid})
	req.Header.Set("X-Syno-Sharing", sharingID)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("context request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB cap
	if err != nil {
		return nil, fmt.Errorf("read context body: %w", err)
	}

	session, extra, err := parseJSContext(body)
	if err != nil {
		return nil, fmt.Errorf("parse context: %w", err)
	}

	// Password-protected shares must be unlocked via LoginWithPassword before
	// any share content is available; return early so the caller can prompt.
	if session.SharingStatus == "password" {
		return &SharingContext{
			SharingID:     session.SharingID,
			Hostname:      session.Hostname,
			SharingStatus: "password",
			SID:           sid,
			SIDMaxAge:     sidMaxAge,
			SIDExpires:    sidExpires,
		}, nil
	}

	if extra.Status != 0 {
		return nil, proxy.SynoErrorFromCode(extra.Status)
	}

	if !extra.IsSharingUpload && extra.Filename == "" {
		return nil, fmt.Errorf("sharing context has no root folder name")
	}

	sc := &SharingContext{
		SharingID:     session.SharingID,
		Hostname:      session.Hostname,
		SharingStatus: session.SharingStatus,
		Extra:         extra,
		IsUpload:      extra.IsSharingUpload,
		SID:           sid,
		SIDMaxAge:     sidMaxAge,
		SIDExpires:    sidExpires,
	}
	if !extra.IsSharingUpload {
		sc.RootPath = "/" + extra.Filename
	}
	return sc, nil
}

// loginResponse mirrors the Synology JSON for SYNO.Core.Sharing.Login.
type loginResponse struct {
	Data struct {
		SharingSID string `json:"sharing_sid"`
	} `json:"data"`
	Error struct {
		Code   int    `json:"code"`
		Errors string `json:"errors"`
	} `json:"error"`
	Success bool `json:"success"`
}

// LoginWithPassword authenticates against a password-protected share and returns
// the resulting sharing_sid together with any TTL attributes from the response cookie.
// A non-nil error is returned for wrong passwords (SynoError code 1001) as well as
// network/parse failures.
func LoginWithPassword(ctx context.Context, client *proxy.Client, sharingID, password string) (sidResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	params := url.Values{}
	params.Set("api", "SYNO.Core.Sharing.Login")
	params.Set("method", "login")
	params.Set("version", "1")
	params.Set("sharing_id", sharingID)
	params.Set("password", password)

	reqURL := client.BaseURL() + "/sharing/webapi/entry.cgi/SYNO.Core.Sharing.Login"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL,
		strings.NewReader(params.Encode()))
	if err != nil {
		return sidResult{}, fmt.Errorf("build login request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return sidResult{}, fmt.Errorf("login request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10)) // 64 KB cap
	if err != nil {
		return sidResult{}, fmt.Errorf("read login response: %w", err)
	}

	var lr loginResponse
	if err := json.Unmarshal(raw, &lr); err != nil {
		return sidResult{}, fmt.Errorf("parse login response: %w", err)
	}

	if !lr.Success {
		return sidResult{}, &proxy.SynoError{Code: lr.Error.Code, Msg: lr.Error.Errors}
	}

	if lr.Data.SharingSID == "" {
		return sidResult{}, fmt.Errorf("login succeeded but response contained no sharing_sid")
	}

	// The SID value comes from the JSON body; TTL attributes come from the cookie
	// that Synology sets on the same response.
	sr := sidResult{Value: lr.Data.SharingSID}
	for _, c := range resp.Cookies() {
		if c.Name == "sharing_sid" {
			sr.MaxAge = c.MaxAge
			sr.Expires = c.Expires
			break
		}
	}
	return sr, nil
}

// parseJSContext extracts the two JSON objects from Synology's JS-formatted response.
//
// The response looks like:
//
//	SYNO.SDS.Session = { ... }
//	;SYNO.SDS.ExtraSession = { ... }
//	;
func parseJSContext(body []byte) (SharingSession, ExtraSession, error) {
	s := string(body)

	var session SharingSession
	if err := decodeJSVar(s, "SYNO.SDS.Session", &session); err != nil {
		return SharingSession{}, ExtraSession{}, fmt.Errorf("decode Session: %w", err)
	}

	var extra ExtraSession
	if err := decodeJSVar(s, "SYNO.SDS.ExtraSession", &extra); err != nil {
		return SharingSession{}, ExtraSession{}, fmt.Errorf("decode ExtraSession: %w", err)
	}

	return session, extra, nil
}

// decodeJSVar finds `varName = { ... }` in src and JSON-decodes the object into dst.
// json.Decoder handles all brace matching and string escaping — no hand-written parser.
func decodeJSVar(src, varName string, dst any) error {
	marker := varName + " = "
	idx := strings.Index(src, marker)
	if idx < 0 {
		return fmt.Errorf("%q not found in response", varName)
	}

	// Advance past the marker to the opening brace.
	start := idx + len(marker)
	for start < len(src) && src[start] != '{' {
		start++
	}
	if start >= len(src) {
		return fmt.Errorf("no opening brace after %q", varName)
	}

	dec := json.NewDecoder(strings.NewReader(src[start:]))
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("JSON decode for %q: %w", varName, err)
	}
	return nil
}
