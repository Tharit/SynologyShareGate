package proxy

import (
	"crypto/tls"
	"fmt"
	"net/http"

	"github.com/tharit/synologysharegate/middleware"
)

// HTTPError is returned when Synology responds with an unexpected HTTP status code.
type HTTPError struct {
	StatusCode int
}

func (e *HTTPError) Error() string { return fmt.Sprintf("synology HTTP %d", e.StatusCode) }

// SynoError represents a Synology API error with a numeric code.
type SynoError struct {
	Code int
	Msg  string
}

func (e *SynoError) Error() string { return fmt.Sprintf("synology error %d: %s", e.Code, e.Msg) }

var knownErrors = map[int]string{
	105: "permission denied",
	114: "share link is expired or invalid",
	408: "invalid parameter",
}

// SynoErrorFromCode returns a SynoError for the given Synology error code.
func SynoErrorFromCode(code int) *SynoError {
	msg, ok := knownErrors[code]
	if !ok {
		msg = "unknown error"
	}
	return &SynoError{Code: code, Msg: msg}
}

// Client is the shared Synology HTTP client.
type Client struct {
	// api is used for short API calls; no timeout since callers set context deadlines.
	api     *http.Client
	baseURL string
	logger  *middleware.Logger
}

// NewClient creates a Client from the given configuration values.
func NewClient(synoHost string, useHTTPS, skipVerify bool, logger *middleware.Logger) *Client {
	scheme := "https"
	if !useHTTPS {
		scheme = "http"
		logger.Warn("SYNO_HTTPS is false — all Synology traffic is unencrypted")
	}

	transport := &http.Transport{}
	if skipVerify {
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
		logger.Warn("SYNO_SKIP_VERIFY is true — TLS certificate verification disabled; do not use in production")
	}

	return &Client{
		// No client-level timeout: API callers use context deadlines; downloads stream indefinitely.
		api:     &http.Client{Transport: transport},
		baseURL: scheme + "://" + synoHost,
		logger:  logger,
	}
}

// Do executes a raw request against the Synology backend.
// The caller is responsible for closing the response body.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	return c.api.Do(req)
}

// BaseURL returns the configured Synology base URL.
func (c *Client) BaseURL() string { return c.baseURL }
