package photo

import (
	"context"
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

// errInviteOnly is returned when a photo share requires a full Synology account
// login (invite-only), which this proxy does not support.
var errInviteOnly = errors.New("photo share requires a Synology account (invite-only)")

const (
	basePathSharing = "/photo/mo/sharing"
	basePathRequest = "/photo/mo/request"
)

// sidResult carries the sharing_sid value together with any TTL attributes
// Synology set on its cookie.
type sidResult struct {
	Value   string
	MaxAge  int
	Expires time.Time
}

// fotoSharing mirrors the fields we need from the FotoSharing object embedded in
// window.SYNO.
type fotoSharing struct {
	EnablePassword bool
	Passphrase     string
	PrivacyType    string // "public-view" | "public-download"
}

// landing is the parsed result of fetching a photo sharing/request landing page.
type landing struct {
	SID                 string
	SIDMaxAge           int
	SIDExpires          time.Time
	IsPasswordProtected bool
	PrivacyType         string // "public-view" | "public-download"; empty for request pages
}

var (
	passphraseRe  = regexp.MustCompile(`\bpassphrase\s*:\s*"([^"]*)"`)
	privacyTypeRe = regexp.MustCompile(`\bprivacy_type\s*:\s*"([^"]*)"`)
	enablePassRe  = regexp.MustCompile(`\benable_password\s*:\s*true\b`)
)

// parseFotoSharing extracts the FotoSharing sub-object embedded in the
// `window.SYNO = {...}` block on the landing page HTML.
//
// That block is NOT valid JSON — Synology emits it as a literal JavaScript object
// with unquoted keys, trailing commas, and (in the SDS.Session sibling object)
// inline function literals like `doLayout: function () {}`. Rather than writing a
// full JS object parser, this isolates just the FotoSharing block via brace
// matching and then regex-extracts its three known scalar fields directly.
func parseFotoSharing(body []byte) (fotoSharing, error) {
	s := string(body)

	synoIdx := strings.Index(s, "window.SYNO")
	if synoIdx < 0 {
		return fotoSharing{}, fmt.Errorf("window.SYNO not found in response")
	}
	s = s[synoIdx:]

	const marker = "FotoSharing"
	idx := strings.Index(s, marker)
	if idx < 0 {
		return fotoSharing{}, fmt.Errorf("FotoSharing not found in window.SYNO")
	}
	rest := s[idx+len(marker):]

	start := strings.IndexByte(rest, '{')
	if start < 0 {
		return fotoSharing{}, fmt.Errorf("no opening brace after FotoSharing")
	}

	block, err := extractBraceBlock(rest[start:])
	if err != nil {
		return fotoSharing{}, fmt.Errorf("extract FotoSharing block: %w", err)
	}

	fs := fotoSharing{EnablePassword: enablePassRe.MatchString(block)}
	if m := passphraseRe.FindStringSubmatch(block); m != nil {
		fs.Passphrase = m[1]
	}
	if m := privacyTypeRe.FindStringSubmatch(block); m != nil {
		fs.PrivacyType = m[1]
	}
	return fs, nil
}

// extractBraceBlock returns the substring of s (which must start with '{') up to
// and including its matching closing brace, honoring quoted string literals so
// that braces inside string values don't confuse the depth count.
func extractBraceBlock(s string) (string, error) {
	if len(s) == 0 || s[0] != '{' {
		return "", fmt.Errorf("input does not start with '{'")
	}
	depth := 0
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' {
				i++ // skip the escaped character
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
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[:i+1], nil
			}
		}
	}
	return "", fmt.Errorf("unterminated brace block")
}

// FetchLanding hits the Synology Photos landing page (browse or request) for the given
// passphrase and parses the sharing_sid cookie and the window.SYNO object.
// basePath is either basePathSharing or basePathRequest.
func FetchLanding(ctx context.Context, client *proxy.Client, basePath, passphrase string) (landing, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	reqURL := client.BaseURL() + basePath + "/" + passphrase
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return landing{}, fmt.Errorf("build landing request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return landing{}, fmt.Errorf("landing request: %w", err)
	}
	defer resp.Body.Close()

	// Invite-only shares redirect to the DSM login/launch-app URL. The underlying
	// http.Client follows redirects by default, so inspect the final request URL
	// rather than the (already-200) status code.
	if resp.Request != nil && resp.Request.URL != nil &&
		strings.Contains(resp.Request.URL.RawQuery, "launchApp=SYNO.Foto.Sharing.AppInstance") {
		return landing{}, errInviteOnly
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB cap
	if err != nil {
		return landing{}, fmt.Errorf("read landing page: %w", err)
	}

	if resp.StatusCode >= 400 {
		return landing{}, &proxy.HTTPError{StatusCode: resp.StatusCode}
	}

	l := landing{}
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "sharing_sid" {
			l.SID = cookie.Value
			l.SIDMaxAge = cookie.MaxAge
			l.SIDExpires = cookie.Expires
			break
		}
	}

	fs, err := parseFotoSharing(body)
	if err != nil {
		return landing{}, fmt.Errorf("parse window.SYNO: %w", err)
	}

	l.IsPasswordProtected = fs.EnablePassword
	l.PrivacyType = fs.PrivacyType
	return l, nil
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

// LoginWithPassword authenticates against a password-protected photo share and
// returns the resulting sharing_sid together with any TTL attributes from the
// response cookie. A non-nil error is returned for wrong passwords (SynoError
// code 1001) as well as network/parse failures.
func LoginWithPassword(ctx context.Context, client *proxy.Client, passphrase, password string) (sidResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	params := url.Values{}
	params.Set("api", "SYNO.Core.Sharing.Login")
	params.Set("method", "login")
	params.Set("version", "1")
	params.Set("sharing_id", passphrase)
	params.Set("password", password)

	reqURL := client.BaseURL() + basePathSharing + "/webapi/entry.cgi/SYNO.Core.Sharing.Login"
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
