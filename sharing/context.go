package sharing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tharit/synologysharegate/middleware"
	"github.com/tharit/synologysharegate/proxy"
)

// errUserAuthRequired is returned by GetContext when a share requires Synology
// account credentials (sharing_status == "user"), which this proxy does not support.
var errUserAuthRequired = errors.New("share requires Synology user credentials")

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

// sidResult carries the sharing_sid value, any TTL attributes from the NAS,
// and the sharing_status parsed from the sharing HTML page.
type sidResult struct {
	Value         string
	MaxAge        int
	Expires       time.Time
	SharingStatus string // parsed from the <script src> in the sharing HTML page
}

// FetchSID hits the Synology sharing HTML page for the given sharing ID and returns
// the sharing_sid session cookie together with any TTL attributes set by the NAS,
// and the sharing_status parsed from the embedded script tag src URL.
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
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB cap
	resp.Body.Close()
	if readErr != nil {
		return sidResult{}, fmt.Errorf("read sharing page: %w", readErr)
	}

	if resp.StatusCode >= 400 {
		return sidResult{}, &proxy.HTTPError{StatusCode: resp.StatusCode}
	}

	sr := sidResult{SharingStatus: parseSharingStatusFromHTML(body)}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "sharing_sid" {
			sr.Value = cookie.Value
			sr.MaxAge = cookie.MaxAge
			sr.Expires = cookie.Expires
			break
		}
	}
	// No sharing_sid cookie is normal for password-protected shares, which only
	// set the cookie after a successful SYNO.Core.Sharing.Login call.
	return sr, nil
}

// parseSharingStatusFromHTML finds the <script src="...SYNO.Core.Sharing.Session...">
// tag in the Synology sharing HTML page and extracts the sharing_status query parameter
// from its src URL.  Synology embeds the real status in this URL; the API itself just
// echoes back whatever value is passed as a GET parameter.
func parseSharingStatusFromHTML(body []byte) string {
	s := string(body)
	const apiMarker = "SYNO.Core.Sharing.Session"
	idx := strings.Index(s, apiMarker)
	if idx < 0 {
		return ""
	}
	// Walk backward from the marker to find the opening src=" of this tag.
	srcIdx := strings.LastIndex(s[:idx], `src="`)
	if srcIdx < 0 {
		return ""
	}
	rest := s[srcIdx+len(`src="`):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		return ""
	}
	// Unescape HTML-attribute entities before parsing as a URL.
	// &amp; → & (query parameter separator).
	// &quot; → %22 so the embedded JSON-style quotes don't split query parameters.
	rawSrc := rest[:end]
	rawSrc = strings.ReplaceAll(rawSrc, "&amp;", "&")
	rawSrc = strings.ReplaceAll(rawSrc, "&quot;", "%22")
	u, err := url.Parse(rawSrc)
	if err != nil {
		return ""
	}
	// Synology wraps enum values in JSON-style double quotes, e.g. "password".
	// Strip them after URL-decoding.
	return strings.Trim(u.Query().Get("sharing_status"), `"`)
}

// GetContext fetches and parses the sharing context for the given sharing ID.
// If sid is non-empty it is used directly and FetchSID is skipped. If sid is
// empty, a fresh SID is fetched first. The SID used is always included in the
// returned SharingContext.
//
// When the share is password-protected the returned SharingContext will have
// SharingStatus == "password" and an empty RootPath; the caller must prompt
// for a password and call LoginWithPassword before accessing share content.
// IsUpload is always set correctly even for locked password shares.
func GetContext(ctx context.Context, client *proxy.Client, logger *middleware.Logger, sharingID, sid string) (*SharingContext, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var sidMaxAge int
	var sidExpires time.Time
	sharingStatus := ""

	if sid == "" {
		sr, err := FetchSID(ctx, client, sharingID)
		if err != nil {
			return nil, fmt.Errorf("fetch session: %w", err)
		}
		sid = sr.Value
		sidMaxAge, sidExpires = sr.MaxAge, sr.Expires
		sharingStatus = sr.SharingStatus

		logger.Debug("sharing status", middleware.F("status", sharingStatus))

		// If the NAS did not issue a SID the share is locked.
		if sid == "" {
			switch sharingStatus {
			case "user":
				return nil, errUserAuthRequired
			case "password":
				// Fall through: call the Session API with an empty SID to learn
				// is_sharing_upload (available before authentication — Synology
				// loads this endpoint as a <script> tag on initial page load).
			default:
				return nil, fmt.Errorf("share did not establish a session (sharing_status: %q)", sharingStatus)
			}
		}
	}

	// We have a SID — call the Session API to get the full share context.
	params := url.Values{}
	params.Set("api", "SYNO.Core.Sharing.Session")
	params.Set("version", "1")
	params.Set("method", "get")
	params.Set("sharing_id", synoQuote(sharingID))
	params.Set("sharing_status", sharingStatus) // plain value; API echoes it back
	params.Set("v", fmt.Sprintf("%d", time.Now().Unix()))

	reqURL := client.BaseURL() + "/sharing/webapi/entry.cgi?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build context request: %w", err)
	}
	// Replicate the unauthenticated <script> tag request for pre-auth shares:
	// send no cookie and no X-Syno-Sharing header so Synology returns the JS
	// context format rather than a JSON auth error.
	if sid != "" {
		req.AddCookie(&http.Cookie{Name: "sharing_sid", Value: sid})
		req.Header.Set("X-Syno-Sharing", sharingID)
	}

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
		if sid == "" {
			// Pre-auth call returned something unexpected (e.g. Synology returned
			// a JSON error instead of the JS context). Fall back to a plain
			// password prompt; the reload-after-unlock path in browse.html handles
			// the upload case if IsUpload can't be determined here.
			logger.Debug("pre-auth session parse failed", middleware.F("err", err.Error()))
			return &SharingContext{
				SharingID:     sharingID,
				SharingStatus: "password",
				SIDMaxAge:     sidMaxAge,
				SIDExpires:    sidExpires,
			}, nil
		}
		return nil, fmt.Errorf("parse context: %w", err)
	}

	// Fallback user check in case HTML parsing failed to detect it.
	if session.SharingStatus == "user" {
		return nil, errUserAuthRequired
	}

	// Password-locked: return early with IsUpload/Extra info from the Session API.
	// The Session API exposes is_sharing_upload (and upload request metadata) even
	// before authentication, so the correct template can be chosen for the prompt.
	if sid == "" {
		return &SharingContext{
			SharingID:     sharingID,
			SharingStatus: "password",
			IsUpload:      extra.IsSharingUpload,
			Extra:         extra,
			SIDMaxAge:     sidMaxAge,
			SIDExpires:    sidExpires,
		}, nil
	}

	if extra.Status != 0 {
		return nil, proxy.SynoErrorFromCode(extra.Status)
	}

	// Synology omits filename from the Session API for password-protected browse
	// shares even after authentication — fetch it from Initdata instead.
	if !extra.IsSharingUpload && extra.Filename == "" {
		filename, err := FetchInitdata(ctx, client, sharingID, sid)
		if err != nil {
			return nil, fmt.Errorf("fetch share name: %w", err)
		}
		if filename == "" {
			return nil, fmt.Errorf("sharing context has no root folder name")
		}
		extra.Filename = filename
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

// initdataResponse mirrors the Synology JSON for SYNO.Core.Sharing.Initdata.
type initdataResponse struct {
	Data struct {
		Private struct {
			Filename string `json:"filename"`
		} `json:"Private"`
	} `json:"data"`
	Error struct {
		Code int `json:"code"`
	} `json:"error"`
	Success bool `json:"success"`
}

// FetchInitdata retrieves the root folder name for an authenticated
// password-protected share. Synology only exposes the filename through this
// endpoint — the Session API never returns it for password-protected shares.
func FetchInitdata(ctx context.Context, client *proxy.Client, sharingID, sid string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	body := url.Values{}
	body.Set("api", "SYNO.Core.Sharing.Initdata")
	body.Set("method", "get")
	body.Set("version", "1")

	reqURL := client.BaseURL() + "/sharing/webapi/entry.cgi"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL,
		strings.NewReader(body.Encode()))
	if err != nil {
		return "", fmt.Errorf("build initdata request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: "sharing_sid", Value: sid})
	req.Header.Set("X-Syno-Sharing", sharingID)

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("initdata request: %w", err)
	}
	defer resp.Body.Close()

	var ir initdataResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&ir); err != nil {
		return "", fmt.Errorf("parse initdata response: %w", err)
	}

	if !ir.Success {
		return "", proxy.SynoErrorFromCode(ir.Error.Code)
	}

	return ir.Data.Private.Filename, nil
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
