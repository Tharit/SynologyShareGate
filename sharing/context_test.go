package sharing

import (
	"testing"
)

// browseContextBody is the raw JS response from doc/api/sharing_id_context.md (browse share).
var browseContextBody = []byte(`SYNO.SDS.Session = {
   "configured" : true,
   "custom_login_title" : "",
   "diskless" : false,
   "enable_syno_token" : "no",
   "fullversion" : "1781661654",
   "hostname" : "nas_hostname",
   "isLogined" : false,
   "lang" : "enu",
   "login_background_color" : "#FFFFFF",
   "login_background_enable" : false,
   "login_background_ext" : ".jpg",
   "login_background_pos" : "center",
   "login_background_seq" : 0,
   "login_background_type" : "fromDS",
   "login_enable_fp" : 0,
   "login_footer_enable_html" : false,
   "login_footer_msg" : "",
   "login_logo_enable" : false,
   "login_logo_ext" : ".jpg",
   "login_logo_pos" : "center",
   "login_logo_seq" : 0,
   "login_only_bgcolor" : false,
   "login_style" : "tpl1",
   "login_version_logo" : false,
   "protect_title" : "",
   "sharing" : true,
   "sharing_id" : "pKGFcZ6A4",
   "sharing_status" : "none",
   "sharing_theme" : {},
   "version" : "1781661654"
}
;SYNO.SDS.ExtraSession = {
   "background_color" : "",
   "background_path" : "../webman/fbsharing_login_background?v=1783625515",
   "background_position" : "",
   "enable_background" : false,
   "enable_custom_setting" : false,
   "enable_footer_html" : false,
   "enable_logo" : false,
   "filename" : "02.10.2009 - Tisch Evaluation",
   "footer_msg" : "",
   "is_folder" : true,
   "logo_color" : "",
   "logo_path" : "../webman/fbsharing_login_logo?v=1783625515",
   "logo_position" : "",
   "status" : 0
}
;`)

// uploadContextBody is the raw JS response from doc/api/sharing_id_context.md (upload share).
var uploadContextBody = []byte(`SYNO.SDS.Session = {
   "configured" : true,
   "custom_login_title" : "",
   "diskless" : false,
   "enable_syno_token" : "no",
   "fullversion" : "1781661654",
   "hostname" : "nas_hostname",
   "isLogined" : false,
   "lang" : "enu",
   "login_background_color" : "#FFFFFF",
   "login_background_enable" : false,
   "login_background_ext" : ".jpg",
   "login_background_pos" : "center",
   "login_background_seq" : 0,
   "login_background_type" : "fromDS",
   "login_enable_fp" : 0,
   "login_footer_enable_html" : false,
   "login_footer_msg" : "",
   "login_logo_enable" : false,
   "login_logo_ext" : ".jpg",
   "login_logo_pos" : "center",
   "login_logo_seq" : 0,
   "login_only_bgcolor" : false,
   "login_style" : "tpl1",
   "login_version_logo" : false,
   "protect_title" : {
      "key" : "file_request_title",
      "section" : "sharing"
   },
   "sharing" : true,
   "sharing_id" : "2kzQERkrf",
   "sharing_status" : "none",
   "sharing_theme" : {},
   "version" : "1781661654"
}
;SYNO.SDS.ExtraSession = {
   "is_sharing_upload" : true,
   "request_info" : "Hello, my friend! Please upload files here.",
   "request_name" : "username",
   "status" : 0
}
;`)

func TestParseJSContext_Browse(t *testing.T) {
	session, extra, err := parseJSContext(browseContextBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if session.SharingID != "pKGFcZ6A4" {
		t.Errorf("SharingID = %q, want %q", session.SharingID, "pKGFcZ6A4")
	}
	if session.Hostname != "nas_hostname" {
		t.Errorf("Hostname = %q, want %q", session.Hostname, "nas_hostname")
	}
	if extra.IsSharingUpload {
		t.Error("IsSharingUpload should be false for browse share")
	}
	if extra.Filename != "02.10.2009 - Tisch Evaluation" {
		t.Errorf("Filename = %q, want %q", extra.Filename, "02.10.2009 - Tisch Evaluation")
	}
	if !extra.IsFolder {
		t.Error("IsFolder should be true")
	}
	if extra.Status != 0 {
		t.Errorf("Status = %d, want 0", extra.Status)
	}
}

func TestParseJSContext_Upload(t *testing.T) {
	session, extra, err := parseJSContext(uploadContextBody)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if session.SharingID != "2kzQERkrf" {
		t.Errorf("SharingID = %q, want %q", session.SharingID, "2kzQERkrf")
	}
	if !extra.IsSharingUpload {
		t.Error("IsSharingUpload should be true for upload share")
	}
	if extra.RequestName != "username" {
		t.Errorf("RequestName = %q, want %q", extra.RequestName, "username")
	}
	if extra.RequestInfo != "Hello, my friend! Please upload files here." {
		t.Errorf("RequestInfo = %q", extra.RequestInfo)
	}
}

func TestParseSharingStatusFromHTML(t *testing.T) {
	// The real Synology HTML wraps enum values in JSON-style double quotes encoded
	// as &quot; in the HTML attribute, e.g. sharing_status=&quot;password&quot;.
	cases := []struct {
		name string
		html string
		want string
	}{
		{
			name: "password share",
			html: `<script type="text/javascript" src="webapi/entry.cgi?api=SYNO.Core.Sharing.Session&amp;version=1&amp;method=get&amp;sharing_id=&quot;RPom3rorP&quot;&amp;sharing_status=&quot;password&quot;&v=1763722670"></script>`,
			want: "password",
		},
		{
			name: "public share",
			html: `<script type="text/javascript" src="webapi/entry.cgi?api=SYNO.Core.Sharing.Session&amp;version=1&amp;method=get&amp;sharing_id=&quot;pKGFcZ6A4&quot;&amp;sharing_status=&quot;none&quot;&v=1234567890"></script>`,
			want: "none",
		},
		{
			name: "user share",
			html: `<script type="text/javascript" src="webapi/entry.cgi?api=SYNO.Core.Sharing.Session&amp;sharing_status=&quot;user&quot;&v=1"></script>`,
			want: "user",
		},
		{
			name: "marker absent",
			html: `<html><body>nothing here</body></html>`,
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSharingStatusFromHTML([]byte(tc.html))
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEncodeDLink(t *testing.T) {
	// From the API doc: /02.10.2009 - Tisch/IMG_0095.JPG → hex of UTF-8 bytes.
	p := "/02.10.2009 - Tisch/IMG_0095.JPG"
	got := encodeDLink(p)
	// Must be wrapped in literal double quotes.
	if got[0] != '"' || got[len(got)-1] != '"' {
		t.Errorf("encodeDLink result not double-quoted: %q", got)
	}
	// The inner hex string must be valid hex of the UTF-8 path bytes.
	inner := got[1 : len(got)-1]
	if len(inner) != len(p)*2 {
		t.Errorf("hex length mismatch: got %d chars for %d-byte path", len(inner), len(p))
	}
}
