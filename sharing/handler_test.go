package sharing

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tharit/synologysharegate/proxy"
)

func TestFormatSize(t *testing.T) {
	cases := []struct {
		bytes int64
		want  string
	}{
		{0, "0 B"},
		{500, "500 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{3359239, "3.2 MB"},
		{1024 * 1024 * 1024, "1.0 GB"},
	}
	for _, tc := range cases {
		got := formatSize(tc.bytes)
		if got != tc.want {
			t.Errorf("formatSize(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}

func TestSharingErrorPage_KnownCodes(t *testing.T) {
	cases := []struct {
		code      int
		wantTitle string
	}{
		{114, "Share Link Expired"},
		{105, "Access Denied"},
		{408, "Invalid Share"},
		{999, "Share Error"},
	}
	for _, tc := range cases {
		ep := sharingErrorPage(proxy.SynoErrorFromCode(tc.code))
		if ep.Title != tc.wantTitle {
			t.Errorf("code %d: Title = %q, want %q", tc.code, ep.Title, tc.wantTitle)
		}
	}
}

func TestSharingErrorPage_HTTPError(t *testing.T) {
	cases := []struct {
		code      int
		wantTitle string
	}{
		{404, "Share Not Found"},
		{403, "Server Error"},
		{500, "Server Error"},
	}
	for _, tc := range cases {
		ep := sharingErrorPage(&proxy.HTTPError{StatusCode: tc.code})
		if ep.Title != tc.wantTitle {
			t.Errorf("HTTP %d: Title = %q, want %q", tc.code, ep.Title, tc.wantTitle)
		}
	}
}

func TestValidSharingID(t *testing.T) {
	valid := []string{"pKGFcZ6A4", "abc123", "a-b_c", "A"}
	for _, id := range valid {
		if !validSharingID(id) {
			t.Errorf("validSharingID(%q) = false, want true", id)
		}
	}
	invalid := []string{"", strings.Repeat("a", 65), "abc/def", "abc?x=1", `ab"cd`, "abc def"}
	for _, id := range invalid {
		if validSharingID(id) {
			t.Errorf("validSharingID(%q) = true, want false", id)
		}
	}
}

func TestWithinRoot(t *testing.T) {
	root := "/MyShare"
	cases := []struct {
		path string
		want bool
	}{
		{"/MyShare", true},
		{"/MyShare/", true},     // cleaned to /MyShare
		{"/MyShare/sub", true},
		{"/MyShare/a/b/c", true},
		{"/MyShare/../other", false},  // cleans to /other
		{"/MyShareExtra", false},
		{"/other", false},
		{"/", false},
	}
	for _, tc := range cases {
		got := withinRoot(tc.path, root)
		if got != tc.want {
			t.Errorf("withinRoot(%q, %q) = %v, want %v", tc.path, root, got, tc.want)
		}
	}
}

func TestSharingErrorPage_NetworkError(t *testing.T) {
	ep := sharingErrorPage(fmt.Errorf("connection refused"))
	if ep.Title != "Server Unavailable" {
		t.Errorf("Title = %q, want %q", ep.Title, "Server Unavailable")
	}
}
