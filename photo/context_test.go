package photo

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tharit/synologysharegate/middleware"
	"github.com/tharit/synologysharegate/proxy"
)

// realWorldLandingBody is the exact window.SYNO block observed from a live NAS for a
// public, view-only browse share (reported against this implementation). It is NOT
// valid JSON: keys are unquoted, there are trailing commas, and SDS carries sibling
// fields with inline function literals (e.g. Desktop.doLayout).
var realWorldLandingBody = []byte(`<html><body>
  <script>
    window.SYNO = {
      SDS: {
        Session: {
          sharing: true,
          sharing_status: "none",
          sharing_id: "ZaAqvUn0f",
        },
        Desktop: {
          doLayout: function () { },
        },
        Utils: {
          Logout: {},
        },
      },
      FotoSharing: {
        enable_password: false,
        passphrase: "ZaAqvUn0f",
        privacy_type: "public-view",
      },
    };
  </script>
</body></html>`)

// passwordLandingBody mirrors the same real-world JS-literal style for a
// password-protected, download-enabled browse share.
var passwordLandingBody = []byte(`<html><body>
  <script>
    window.SYNO = {
      SDS: {
        Session: {
          sharing: true,
          sharing_status: "password",
          sharing_id: "YXiEpO1Xy",
        },
      },
      FotoSharing: {
        enable_password: true,
        passphrase: "YXiEpO1Xy",
        privacy_type: "public-download",
      },
    };
  </script>
</body></html>`)

// uploadRequestLandingBody mirrors the doc's note that the upload request page's
// window.SYNO.FotoSharing only carries passphrase — no enable_password/privacy_type.
var uploadRequestLandingBody = []byte(`<html><body>
  <script>
    window.SYNO = {
      SDS: {
        Session: {
          sharing: true,
          sharing_status: "none",
          sharing_id: "P4MBAbRsi",
        },
      },
      FotoSharing: {
        passphrase: "P4MBAbRsi",
      },
    };
  </script>
</body></html>`)

func TestParseFotoSharing_RealWorld(t *testing.T) {
	fs, err := parseFotoSharing(realWorldLandingBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fs.EnablePassword {
		t.Error("EnablePassword should be false for a public share")
	}
	if fs.Passphrase != "ZaAqvUn0f" {
		t.Errorf("Passphrase = %q, want %q", fs.Passphrase, "ZaAqvUn0f")
	}
	if fs.PrivacyType != "public-view" {
		t.Errorf("PrivacyType = %q, want %q", fs.PrivacyType, "public-view")
	}
}

func TestParseFotoSharing_Password(t *testing.T) {
	fs, err := parseFotoSharing(passwordLandingBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !fs.EnablePassword {
		t.Error("EnablePassword should be true for a locked share")
	}
	if fs.PrivacyType != "public-download" {
		t.Errorf("PrivacyType = %q, want %q", fs.PrivacyType, "public-download")
	}
}

func TestParseFotoSharing_UploadRequest(t *testing.T) {
	fs, err := parseFotoSharing(uploadRequestLandingBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fs.Passphrase != "P4MBAbRsi" {
		t.Errorf("Passphrase = %q, want %q", fs.Passphrase, "P4MBAbRsi")
	}
	if fs.EnablePassword {
		t.Error("EnablePassword should be false (zero value) when absent from the response")
	}
}

func TestParseFotoSharing_NotFound(t *testing.T) {
	_, err := parseFotoSharing([]byte(`<html><body>nothing here</body></html>`))
	if err == nil {
		t.Fatal("expected an error when window.SYNO is absent")
	}
}

func TestExtractBraceBlock(t *testing.T) {
	in := `{ a: { b: 1 }, c: "text with } brace" },trailing`
	got, err := extractBraceBlock(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := `{ a: { b: 1 }, c: "text with } brace" }`
	if got != want {
		t.Errorf("extractBraceBlock() = %q, want %q", got, want)
	}
}

func TestFetchLanding_Public(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "sharing_sid", Value: "abc123"})
		w.Write(realWorldLandingBody) //nolint:errcheck
	}))
	defer ts.Close()

	client := newTestClient(t, ts.URL)

	l, err := FetchLanding(context.Background(), client, basePathSharing, "ZaAqvUn0f")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if l.SID != "abc123" {
		t.Errorf("SID = %q, want %q", l.SID, "abc123")
	}
	if l.IsPasswordProtected {
		t.Error("IsPasswordProtected should be false")
	}
	if l.PrivacyType != "public-view" {
		t.Errorf("PrivacyType = %q, want %q", l.PrivacyType, "public-view")
	}
}

func TestFetchLanding_InviteOnly(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/photo/mo/sharing/") {
			http.Redirect(w, r, "/?launchApp=SYNO.Foto.Sharing.AppInstance&passphrase=abc&photos_action=login", http.StatusFound)
			return
		}
		w.Write([]byte("<html>DSM login</html>")) //nolint:errcheck
	}))
	defer ts.Close()

	client := newTestClient(t, ts.URL)

	_, err := FetchLanding(context.Background(), client, basePathSharing, "abc")
	if !errors.Is(err, errInviteOnly) {
		t.Fatalf("got error %v, want errInviteOnly", err)
	}
}

// newTestClient builds a proxy.Client pointed at a local httptest.Server.
func newTestClient(t *testing.T, serverURL string) *proxy.Client {
	t.Helper()
	logger := middleware.NewLogger("debug")
	return proxy.NewClient(strings.TrimPrefix(serverURL, "http://"), false, false, logger)
}
