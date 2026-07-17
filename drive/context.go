package drive

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/tharit/synologysharegate/proxy"
)

// errUnsupportedShare is returned when a Drive browse share requires a full
// Synology account login (getDriveErrCode() == 1002), which this proxy does not support.
var errUnsupportedShare = errors.New("drive share requires a Synology account login")

// synoQuote wraps s in the literal double quotes Synology's API expects and
// strips embedded double quotes and backslashes to prevent malformed parameters.
func synoQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, "")
	s = strings.ReplaceAll(s, `"`, "")
	return `"` + s + `"`
}

// folderTokenKey signs the per-uploader subfolder id handed back to the client
// after APIUploadInit (see signFolderToken). It's generated fresh per process
// start and never persisted: this proxy keeps no server-side state, so a client
// restarting its upload batch after a process restart is the same "session gone,
// start over" behavior a Synology session-token expiry would already produce.
var folderTokenKey = func() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("drive: failed to generate folder token key: " + err.Error())
	}
	return key
}()

// signFolderToken authenticates folderID (the per-uploader subfolder this proxy
// itself created in APIUploadInit) by binding it to the file-request share that
// created it, so a later APIUploadFile/APIUploadNotify call can't substitute an
// arbitrary folder id — one belonging to a different share, or one never actually
// created by this proxy — for the real one.
func signFolderToken(fileRequestID, sharingLink, folderID string) string {
	mac := hmac.New(sha256.New, folderTokenKey)
	mac.Write([]byte(fileRequestID + "|" + sharingLink + "|" + folderID))
	return folderID + "." + hex.EncodeToString(mac.Sum(nil))
}

// verifyFolderToken extracts and authenticates the folder id from a token
// produced by signFolderToken, scoped to the same fileRequestID/sharingLink. The
// digits before the "." must never be trusted on their own — only a value that
// carries a valid signature for this exact share may reach a Synology API call.
func verifyFolderToken(fileRequestID, sharingLink, token string) (string, error) {
	folderID, sig, ok := strings.Cut(token, ".")
	if !ok {
		return "", fmt.Errorf("malformed folder token")
	}
	want := hmac.New(sha256.New, folderTokenKey)
	want.Write([]byte(fileRequestID + "|" + sharingLink + "|" + folderID))
	got, err := hex.DecodeString(sig)
	if err != nil || !hmac.Equal(got, want.Sum(nil)) {
		return "", fmt.Errorf("invalid folder token")
	}
	return folderID, nil
}

// DriveCapabilities mirrors the "capabilities" object present on every Drive node.
type DriveCapabilities struct {
	CanRead     bool `json:"can_read"`
	CanPreview  bool `json:"can_preview"`
	CanDownload bool `json:"can_download"`
	CanWrite    bool `json:"can_write"`
}

// DriveAdvSharedInfo mirrors the "adv_shared_info" sub-object.
type DriveAdvSharedInfo struct {
	HasPassword bool `json:"has_password"`
}

// DriveOwner mirrors the "owner" sub-object.
type DriveOwner struct {
	DisplayName string `json:"display_name"`
}

// DriveNode is the shared node object returned by getDriveFile() and the folder
// listing API — same shape for the shared item itself and every child entry.
type DriveNode struct {
	FileID        string             `json:"file_id"`
	PermanentLink string             `json:"permanent_link"`
	Name          string             `json:"name"`
	Type          string             `json:"type"` // "dir" or "file"
	ContentType   string             `json:"content_type"`
	Size          int64              `json:"size"`
	ModifiedTime  int64              `json:"modified_time"`
	Owner         DriveOwner         `json:"owner"`
	AdvSharedInfo DriveAdvSharedInfo `json:"adv_shared_info"`
	Capabilities  DriveCapabilities  `json:"capabilities"`
}

// IsDir reports whether the node is a folder.
func (n DriveNode) IsDir() bool { return n.Type == "dir" }

// bootstrap is the parsed result of the SYNO.SynologyDrive.Shard.getjs script.
type bootstrap struct {
	ErrCode int
	File    *DriveNode
}

// extractReturnValue scans s (which must start right after the `return` keyword
// of a `function(){return ...;}` body) and returns the raw JSON-ish substring up
// to the terminating top-level `;`, honoring quoted strings and brace/bracket
// nesting so that `;` inside string values or nested objects doesn't cause an
// early cutoff. An empty return (`return ;`) yields an empty string, not an error.
func extractReturnValue(s string) (string, error) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
		i++
	}
	start := i
	depth := 0
	var quote byte
	for ; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case '{', '[':
			depth++
		case '}', ']':
			depth--
		case ';':
			if depth <= 0 {
				return strings.TrimSpace(s[start:i]), nil
			}
		}
	}
	return "", fmt.Errorf("unterminated return value")
}

// extractGetter finds `name=function(){return ...;}` in s and returns the raw
// value between `return` and the terminating `;`.
func extractGetter(s, name string) (string, bool) {
	marker := name + "=function(){return"
	idx := strings.Index(s, marker)
	if idx < 0 {
		return "", false
	}
	val, err := extractReturnValue(s[idx+len(marker):])
	if err != nil {
		return "", false
	}
	return val, true
}

// parseBootstrap parses the executable-JS response of SYNO.SynologyDrive.Shard.getjs.
func parseBootstrap(body []byte) (bootstrap, error) {
	s := string(body)

	errCodeStr, ok := extractGetter(s, "getDriveErrCode")
	if !ok {
		return bootstrap{}, fmt.Errorf("getDriveErrCode not found in bootstrap script")
	}
	var errCode int
	if err := json.Unmarshal([]byte(errCodeStr), &errCode); err != nil {
		return bootstrap{}, fmt.Errorf("parse getDriveErrCode: %w", err)
	}

	var node *DriveNode
	if fileStr, ok := extractGetter(s, "getDriveFile"); ok && strings.TrimSpace(fileStr) != "" {
		var n DriveNode
		if err := json.Unmarshal([]byte(fileStr), &n); err != nil {
			return bootstrap{}, fmt.Errorf("parse getDriveFile: %w", err)
		}
		node = &n
	}

	return bootstrap{ErrCode: errCode, File: node}, nil
}

// browseAPIBase returns the entry.cgi base path for a browse share. Per
// doc/api/drive.md, the base is the browser URL with its last path segment
// removed: the {sharing_link} segment for /d/s/ links, or the whole path for
// /d/f/ links (which have no sharing_link segment at all).
func browseAPIBase(permanentLink, sharingLink string) string {
	if sharingLink == "" {
		return "/drive/d/f"
	}
	return "/drive/d/s/" + permanentLink
}

// browseCookieName returns the Synology cookie name for a browse share.
func browseCookieName(sharingLink string) string {
	return "drive-sharing-" + sharingLink
}

// browsePageURL returns the browser-facing landing page path for a browse share.
func browsePageURL(permanentLink, sharingLink string) string {
	if sharingLink == "" {
		return "/drive/d/f/" + permanentLink
	}
	return "/drive/d/s/" + permanentLink + "/" + sharingLink
}

// browseSharingType returns the sharing_type bootstrap parameter for a browse share.
func browseSharingType(sharingLink string) string {
	if sharingLink == "" {
		return "simple_sharing"
	}
	return "public_sharing"
}

// fetchBootstrap calls SYNO.SynologyDrive.Shard.getjs and parses the result.
func fetchBootstrap(ctx context.Context, client *proxy.Client, base, permanentLink, sharingLink, token string) (bootstrap, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	params := url.Values{}
	params.Set("api", "SYNO.SynologyDrive.Shard")
	params.Set("version", "1")
	params.Set("method", "getjs")
	params.Set("permanent_link", synoQuote(permanentLink))
	params.Set("sharing_type", synoQuote(browseSharingType(sharingLink)))
	if sharingLink != "" {
		params.Set("sharing_link", synoQuote(sharingLink))
	}
	params.Set("v", fmt.Sprintf("%d", time.Now().Unix()))

	reqURL := client.BaseURL() + base + "/webapi/entry.cgi?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return bootstrap{}, fmt.Errorf("build bootstrap request: %w", err)
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: browseCookieName(sharingLink), Value: token})
	}

	resp, err := client.Do(req)
	if err != nil {
		return bootstrap{}, fmt.Errorf("bootstrap request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return bootstrap{}, &proxy.HTTPError{StatusCode: resp.StatusCode}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4 MB cap
	if err != nil {
		return bootstrap{}, fmt.Errorf("read bootstrap script: %w", err)
	}

	return parseBootstrap(body)
}

// tokenResult carries a sharing_token cookie value together with its TTL attributes.
type tokenResult struct {
	Value   string
	MaxAge  int
	Expires time.Time
}

// fetchBrowseLandingCookie loads the browse landing page and returns the
// drive-sharing-{link} cookie Synology set on it (empty for password shares).
func fetchBrowseLandingCookie(ctx context.Context, client *proxy.Client, permanentLink, sharingLink string) (tokenResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	reqURL := client.BaseURL() + browsePageURL(permanentLink, sharingLink)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return tokenResult{}, fmt.Errorf("build landing request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return tokenResult{}, fmt.Errorf("landing request: %w", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)) //nolint:errcheck

	if resp.StatusCode >= 400 {
		return tokenResult{}, &proxy.HTTPError{StatusCode: resp.StatusCode}
	}

	cookieName := browseCookieName(sharingLink)
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			return tokenResult{Value: c.Value, MaxAge: c.MaxAge, Expires: c.Expires}, nil
		}
	}
	// No cookie is normal for password-protected shares.
	return tokenResult{}, nil
}

// BrowseContext holds the result of bootstrapping a Drive browse share.
type BrowseContext struct {
	Token        string
	TokenMaxAge  int
	TokenExpires time.Time
	ErrCode      int
	Root         *DriveNode // nil for password-locked or invite-only shares
}

// GetBrowseContext bootstraps a Drive browse share. If token is empty, the
// landing page is fetched first to obtain a fresh session cookie. The caller
// must branch on ErrCode: 0 = OK, 1037 = password required, 1002 = requires a
// Synology account login (unsupported).
func GetBrowseContext(ctx context.Context, client *proxy.Client, permanentLink, sharingLink, token string) (*BrowseContext, error) {
	var maxAge int
	var expires time.Time

	if token == "" {
		tr, err := fetchBrowseLandingCookie(ctx, client, permanentLink, sharingLink)
		if err != nil {
			return nil, fmt.Errorf("fetch landing cookie: %w", err)
		}
		token, maxAge, expires = tr.Value, tr.MaxAge, tr.Expires
	}

	boot, err := fetchBootstrap(ctx, client, browseAPIBase(permanentLink, sharingLink), permanentLink, sharingLink, token)
	if err != nil {
		return nil, fmt.Errorf("fetch bootstrap: %w", err)
	}

	if boot.ErrCode == 1002 {
		return nil, errUnsupportedShare
	}

	return &BrowseContext{
		Token:        token,
		TokenMaxAge:  maxAge,
		TokenExpires: expires,
		ErrCode:      boot.ErrCode,
		Root:         boot.File,
	}, nil
}

// authResponse mirrors the Synology JSON for the Drive auth endpoints
// (SYNO.SynologyDrive.AdvanceSharing.Public.auth and
// SYNO.SynologyDrive.FileRequest.Public.auth).
type authResponse struct {
	Data struct {
		SharingToken string `json:"sharing_token"`
	} `json:"data"`
	Error struct {
		Code   int `json:"code"`
		Errors struct {
			Message string `json:"message"`
		} `json:"errors"`
	} `json:"error"`
	Success bool `json:"success"`
}

// AuthBrowsePassword authenticates against a password-protected browse share.
func AuthBrowsePassword(ctx context.Context, client *proxy.Client, permanentLink, sharingLink, password string) (tokenResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	body := url.Values{}
	body.Set("api", "SYNO.SynologyDrive.AdvanceSharing.Public")
	body.Set("method", "auth")
	body.Set("version", "1")
	body.Set("sharing_link", synoQuote(sharingLink))
	body.Set("password", synoQuote(password))

	reqURL := client.BaseURL() + browseAPIBase(permanentLink, sharingLink) + "/webapi/entry.cgi"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(body.Encode()))
	if err != nil {
		return tokenResult{}, fmt.Errorf("build auth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return tokenResult{}, fmt.Errorf("auth request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10)) // 64 KB cap
	if err != nil {
		return tokenResult{}, fmt.Errorf("read auth response: %w", err)
	}

	var ar authResponse
	if err := json.Unmarshal(raw, &ar); err != nil {
		return tokenResult{}, fmt.Errorf("parse auth response: %w", err)
	}
	if !ar.Success {
		return tokenResult{}, &proxy.SynoError{Code: ar.Error.Code, Msg: ar.Error.Errors.Message}
	}
	if ar.Data.SharingToken == "" {
		return tokenResult{}, fmt.Errorf("auth succeeded but response contained no sharing_token")
	}

	tr := tokenResult{Value: ar.Data.SharingToken}
	cookieName := browseCookieName(sharingLink)
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			tr.MaxAge, tr.Expires = c.MaxAge, c.Expires
			break
		}
	}
	return tr, nil
}

// requestAPIBase returns the entry.cgi base path for an upload-request share.
func requestAPIBase(fileRequestID string) string {
	return "/drive/d/r/" + fileRequestID
}

// requestCookieName returns the Synology cookie name for an upload-request share.
func requestCookieName(sharingLink string) string {
	return "drive-request-" + sharingLink
}

// requestPageURL returns the browser-facing landing page path for an upload-request share.
func requestPageURL(fileRequestID, sharingLink string) string {
	return "/drive/d/r/" + fileRequestID + "/" + sharingLink
}

// RequestLanding is the parsed result of the inline getDriveFileRequestXxx vars
// embedded in the /d/r/ landing page HTML.
type RequestLanding struct {
	State         string // "file_request_ok" or "file_request_password"
	Title         string
	Description   string
	FileRequestID string
	FileID        string // target folder's file_id
}

var requestVarRe = regexp.MustCompile(`(?m)^\s*(getDrive\w+)\s*=\s*\(\)\s*=>\s*(.+?)\s*$`)

// parseRequestLanding extracts the plain `name = () => value` arrow-function
// assignments embedded in the /d/r/ landing page HTML.
func parseRequestLanding(body []byte) (RequestLanding, error) {
	matches := requestVarRe.FindAllStringSubmatch(string(body), -1)
	if matches == nil {
		return RequestLanding{}, fmt.Errorf("no getDriveFileRequestXxx vars found in landing page")
	}

	vals := make(map[string]string, len(matches))
	for _, m := range matches {
		vals[m[1]] = m[2]
	}

	get := func(name string) string {
		raw, ok := vals[name]
		if !ok {
			return ""
		}
		var s string
		if json.Unmarshal([]byte(raw), &s) == nil {
			return s
		}
		return strings.Trim(raw, `"`)
	}

	state := get("getDriveFileRequestState")
	if state == "" {
		return RequestLanding{}, fmt.Errorf("getDriveFileRequestState not found in landing page")
	}

	return RequestLanding{
		State:         state,
		Title:         get("getDriveFileRequestTitle"),
		Description:   get("getDriveFileRequestDescription"),
		FileRequestID: get("getDriveFileRequestId"),
		FileID:        get("getDriveFileId"),
	}, nil
}

// FetchRequestLanding loads the upload-request landing page, parsing its inline
// metadata and returning the drive-request-{link} cookie (empty for password
// requests, until AuthRequestPassword succeeds).
func FetchRequestLanding(ctx context.Context, client *proxy.Client, fileRequestID, sharingLink string) (RequestLanding, tokenResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	reqURL := client.BaseURL() + requestPageURL(fileRequestID, sharingLink)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return RequestLanding{}, tokenResult{}, fmt.Errorf("build request-landing request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return RequestLanding{}, tokenResult{}, fmt.Errorf("request-landing request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return RequestLanding{}, tokenResult{}, &proxy.HTTPError{StatusCode: resp.StatusCode}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB cap
	if err != nil {
		return RequestLanding{}, tokenResult{}, fmt.Errorf("read request-landing page: %w", err)
	}

	rl, err := parseRequestLanding(body)
	if err != nil {
		return RequestLanding{}, tokenResult{}, fmt.Errorf("parse request-landing page: %w", err)
	}

	var tr tokenResult
	cookieName := requestCookieName(sharingLink)
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			tr = tokenResult{Value: c.Value, MaxAge: c.MaxAge, Expires: c.Expires}
			break
		}
	}
	return rl, tr, nil
}

// AuthRequestPassword authenticates against a password-protected upload-request share.
func AuthRequestPassword(ctx context.Context, client *proxy.Client, fileRequestID, sharingLink, password string) (tokenResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	body := url.Values{}
	body.Set("api", "SYNO.SynologyDrive.FileRequest.Public")
	body.Set("method", "auth")
	body.Set("version", "1")
	body.Set("password", synoQuote(password))
	body.Set("encryption", `["password"]`)
	body.Set("sharing_link", synoQuote(sharingLink))

	reqURL := client.BaseURL() + requestAPIBase(fileRequestID) + "/webapi/entry.cgi"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, strings.NewReader(body.Encode()))
	if err != nil {
		return tokenResult{}, fmt.Errorf("build auth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := client.Do(req)
	if err != nil {
		return tokenResult{}, fmt.Errorf("auth request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10)) // 64 KB cap
	if err != nil {
		return tokenResult{}, fmt.Errorf("read auth response: %w", err)
	}

	cookieName := requestCookieName(sharingLink)
	for _, c := range resp.Cookies() {
		if c.Name == cookieName && c.Value != "" {
			return tokenResult{Value: c.Value, MaxAge: c.MaxAge, Expires: c.Expires}, nil
		}
	}

	// No cookie was set — the response body, if non-empty, should contain the error.
	if len(raw) > 0 {
		var ar authResponse
		if err := json.Unmarshal(raw, &ar); err == nil && !ar.Success {
			return tokenResult{}, &proxy.SynoError{Code: ar.Error.Code, Msg: ar.Error.Errors.Message}
		}
	}
	return tokenResult{}, fmt.Errorf("auth did not set a session cookie")
}
