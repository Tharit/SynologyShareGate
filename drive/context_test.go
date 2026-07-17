package drive

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tharit/synologysharegate/middleware"
	"github.com/tharit/synologysharegate/proxy"
)

// realWorldPublicFileBootstrap is a trimmed version of the getjs response
// captured from a live NAS for a public, view+download file share. The
// surrounding getDriveTexts/getSASTexts blobs are omitted since extractGetter
// only anchors on the specific function name it's looking for.
var realWorldPublicFileBootstrap = []byte(`window.getDriveShareMode=function(){return 'public';}
window.getDriveErrCode=function(){return 0;}
window.getDriveAllowToShare=function(){return true;}
window.getDriveLink=function(){return "194HpbSI2AWtijAKdrv7Lcgze5Pi01Qx";}
window.getDriveSharingLink=function(){return "hca5SgpB8kZKhQr1Wy8mrfl36bjeSz3B-fLFAX_T0WQ0";}
window.getDriveFile=function(){return {"file_id":"962069303782322533","permanent_link":"194HpbSI2AWtijAKdrv7Lcgze5Pi01Qx","name":"test_2.txt","type":"file","content_type":"document","size":14,"modified_time":1784126349,"owner":{"display_name":"martin"},"adv_shared_info":{"has_password":false},"capabilities":{"can_read":true,"can_preview":true,"can_download":true,"can_write":false}};}
window.getOfficeTexts=function(){return {};}
`)

// realWorldPasswordBootstrap mirrors a locked share pre-auth: errCode 1037 and
// an empty getDriveFile body (literally `return ;`).
var realWorldPasswordBootstrap = []byte(`window.getDriveShareMode=function(){return 'public';}
window.getDriveErrCode=function(){return 1037;}
window.getDriveFile=function(){return ;}
`)

// realWorldInviteOnlyBootstrap mirrors an invite-only share: errCode 1002.
var realWorldInviteOnlyBootstrap = []byte(`window.getDriveErrCode=function(){return 1002;}
window.getDriveFile=function(){return ;}
`)

func TestParseBootstrap_PublicFile(t *testing.T) {
	b, err := parseBootstrap(realWorldPublicFileBootstrap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.ErrCode != 0 {
		t.Errorf("ErrCode = %d, want 0", b.ErrCode)
	}
	if b.File == nil {
		t.Fatal("File should not be nil")
	}
	if b.File.FileID != "962069303782322533" {
		t.Errorf("FileID = %q, want %q", b.File.FileID, "962069303782322533")
	}
	if b.File.Name != "test_2.txt" {
		t.Errorf("Name = %q, want %q", b.File.Name, "test_2.txt")
	}
	if b.File.IsDir() {
		t.Error("IsDir() should be false for a file")
	}
	if !b.File.Capabilities.CanDownload {
		t.Error("CanDownload should be true")
	}
	if b.File.Capabilities.CanWrite {
		t.Error("CanWrite should be false")
	}
}

func TestParseBootstrap_Password(t *testing.T) {
	b, err := parseBootstrap(realWorldPasswordBootstrap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.ErrCode != 1037 {
		t.Errorf("ErrCode = %d, want 1037", b.ErrCode)
	}
	if b.File != nil {
		t.Error("File should be nil when getDriveFile's return is empty")
	}
}

func TestParseBootstrap_InviteOnly(t *testing.T) {
	b, err := parseBootstrap(realWorldInviteOnlyBootstrap)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if b.ErrCode != 1002 {
		t.Errorf("ErrCode = %d, want 1002", b.ErrCode)
	}
}

func TestParseBootstrap_MissingErrCode(t *testing.T) {
	_, err := parseBootstrap([]byte(`window.getDriveFile=function(){return ;}`))
	if err == nil {
		t.Fatal("expected an error when getDriveErrCode is absent")
	}
}

func TestExtractReturnValue(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", " ;}", ""},
		{"number", "0;}", "0"},
		{"double-quoted string", `"abc";}`, `"abc"`},
		{"single-quoted string", `'public';}`, `'public'`},
		{"object with semicolon-like content", `{"a":"x;y","b":{"c":1}};}`, `{"a":"x;y","b":{"c":1}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractReturnValue(tt.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("extractReturnValue(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// realWorldRequestLandingOK is a trimmed version of the inline vars captured
// from a live NAS for a public (no password) upload-request landing page.
var realWorldRequestLandingOK = []byte(`<script nonce="QMZNjJ4rvq">
getDriveLink = () => "194Hvk7eenZHK4qcuVqFJyxll0PLGpiz"
getDriveSharingLink = () => "3A-xhFgd2MhkPPLGaraqE0u5x_cTLNCR-qLIAWFz2WQ0"
getDriveFileRequestState = () => "file_request_ok"
getDriveDSID = () => "dbf67fdb3d327908002332043ce6a758"
getDriveFileRequestCreator = () => "martin"
getDriveFileRequestTitle = () => "This is a test"
getDriveFileRequestDescription = () => "Description bla bla bla"
getDriveFileRequestIdentifier = () => "create_folder"
getDriveFileRequestExpire = () => 0
getDriveFileRequestId = () => "YdSLCQYI4nsz5juX"
getDriveFileId = () => "962069652524020178"
getDriveFileRequestHasDueDateHour = () => false
`)

// realWorldRequestLandingPassword mirrors the same page for a password-protected request.
var realWorldRequestLandingPassword = []byte(`<script nonce="abc">
getDriveLink = () => "194II31nZxoaVHOhYlungWTriUp55au4"
getDriveSharingLink = () => "_ykMMhjApKt0bWYSZ1r4uQD4t0lObLzq-xrLA4XL2WQ0"
getDriveFileRequestState = () => "file_request_password"
getDriveFileRequestTitle = () => "PW protected title"
getDriveFileRequestDescription = () => "Another description lalala"
getDriveFileRequestId = () => "BRdW6d8uD4izSabA"
getDriveFileId = () => "962070919449195185"
`)

func TestParseRequestLanding_OK(t *testing.T) {
	rl, err := parseRequestLanding(realWorldRequestLandingOK)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rl.State != "file_request_ok" {
		t.Errorf("State = %q, want %q", rl.State, "file_request_ok")
	}
	if rl.Title != "This is a test" {
		t.Errorf("Title = %q, want %q", rl.Title, "This is a test")
	}
	if rl.FileRequestID != "YdSLCQYI4nsz5juX" {
		t.Errorf("FileRequestID = %q, want %q", rl.FileRequestID, "YdSLCQYI4nsz5juX")
	}
	if rl.FileID != "962069652524020178" {
		t.Errorf("FileID = %q, want %q", rl.FileID, "962069652524020178")
	}
}

func TestParseRequestLanding_Password(t *testing.T) {
	rl, err := parseRequestLanding(realWorldRequestLandingPassword)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rl.State != "file_request_password" {
		t.Errorf("State = %q, want %q", rl.State, "file_request_password")
	}
	if rl.Title != "PW protected title" {
		t.Errorf("Title = %q, want %q", rl.Title, "PW protected title")
	}
}

func TestParseRequestLanding_NotFound(t *testing.T) {
	_, err := parseRequestLanding([]byte(`<html><body>nothing here</body></html>`))
	if err == nil {
		t.Fatal("expected an error when getDriveFileRequestState is absent")
	}
}

func TestBrowseAPIBase(t *testing.T) {
	if got := browseAPIBase("perm1", "link1"); got != "/drive/d/s/perm1" {
		t.Errorf("browseAPIBase(with link) = %q, want %q", got, "/drive/d/s/perm1")
	}
	if got := browseAPIBase("perm1", ""); got != "/drive/d/f" {
		t.Errorf("browseAPIBase(invite-only) = %q, want %q", got, "/drive/d/f")
	}
}

func TestBrowseCookieName(t *testing.T) {
	if got := browseCookieName("link1"); got != "drive-sharing-link1" {
		t.Errorf("browseCookieName() = %q, want %q", got, "drive-sharing-link1")
	}
}

func TestRequestAPIBase(t *testing.T) {
	if got := requestAPIBase("req1"); got != "/drive/d/r/req1" {
		t.Errorf("requestAPIBase() = %q, want %q", got, "/drive/d/r/req1")
	}
}

func TestFetchRequestLanding(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "drive-request-3A-xhFgd2MhkPPLGaraqE0u5x_cTLNCR-qLIAWFz2WQ0", Value: "tok123"})
		w.Write(realWorldRequestLandingOK) //nolint:errcheck
	}))
	defer ts.Close()

	client := newTestClient(t, ts.URL)

	rl, tr, err := FetchRequestLanding(context.Background(), client, "YdSLCQYI4nsz5juX", "3A-xhFgd2MhkPPLGaraqE0u5x_cTLNCR-qLIAWFz2WQ0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.Value != "tok123" {
		t.Errorf("token = %q, want %q", tr.Value, "tok123")
	}
	if rl.State != "file_request_ok" {
		t.Errorf("State = %q, want %q", rl.State, "file_request_ok")
	}
}

// newTestClient builds a proxy.Client pointed at a local httptest.Server.
func newTestClient(t *testing.T, serverURL string) *proxy.Client {
	t.Helper()
	logger := middleware.NewLogger("debug")
	return proxy.NewClient(strings.TrimPrefix(serverURL, "http://"), false, false, logger)
}
