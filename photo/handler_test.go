package photo

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

func TestValidID(t *testing.T) {
	valid := []string{"EWvNhI0J0", "abc123", "a-b_c", "A"}
	for _, id := range valid {
		if !validID(id) {
			t.Errorf("validID(%q) = false, want true", id)
		}
	}
	invalid := []string{"", strings.Repeat("a", 65), "abc/def", "abc?x=1", `ab"cd`, "abc def"}
	for _, id := range invalid {
		if validID(id) {
			t.Errorf("validID(%q) = true, want false", id)
		}
	}
}

func TestPhotoErrorPage_InviteOnly(t *testing.T) {
	ep := photoErrorPage(errInviteOnly)
	if ep.Title != "Unsupported Share" {
		t.Errorf("Title = %q, want %q", ep.Title, "Unsupported Share")
	}
}

func TestPhotoErrorPage_KnownCodes(t *testing.T) {
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
		ep := photoErrorPage(proxy.SynoErrorFromCode(tc.code))
		if ep.Title != tc.wantTitle {
			t.Errorf("code %d: Title = %q, want %q", tc.code, ep.Title, tc.wantTitle)
		}
	}
}

func TestPhotoErrorPage_HTTPError(t *testing.T) {
	cases := []struct {
		code      int
		wantTitle string
	}{
		{404, "Share Not Found"},
		{403, "Server Error"},
		{500, "Server Error"},
	}
	for _, tc := range cases {
		ep := photoErrorPage(&proxy.HTTPError{StatusCode: tc.code})
		if ep.Title != tc.wantTitle {
			t.Errorf("HTTP %d: Title = %q, want %q", tc.code, ep.Title, tc.wantTitle)
		}
	}
}

func TestPhotoErrorPage_NetworkError(t *testing.T) {
	ep := photoErrorPage(fmt.Errorf("connection refused"))
	if ep.Title != "Server Unavailable" {
		t.Errorf("Title = %q, want %q", ep.Title, "Server Unavailable")
	}
}

func TestIsBidiOrInvisible(t *testing.T) {
	invisible := []rune{0x200B, 0x202E, 0x2066, 0xFEFF}
	for _, r := range invisible {
		if !isBidiOrInvisible(r) {
			t.Errorf("isBidiOrInvisible(%U) = false, want true", r)
		}
	}
	visible := []rune{'a', 'Z', '0', ' ', '.'}
	for _, r := range visible {
		if isBidiOrInvisible(r) {
			t.Errorf("isBidiOrInvisible(%U) = true, want false", r)
		}
	}
}

func TestRFC5987Encode(t *testing.T) {
	got := rfc5987Encode("café.jpg")
	want := "caf%C3%A9.jpg"
	if got != want {
		t.Errorf("rfc5987Encode = %q, want %q", got, want)
	}
}
