# Synology FileStation Sharing API

FileStation sharing links are created by DSM's file manager. They give access to a folder or file on the NAS without DSM credentials.

## URL Patterns

| Purpose | URL |
|---|---|
| Share landing page | `GET /sharing/{sharing_id}` |
| Session context API | `GET /sharing/webapi/entry.cgi?api=SYNO.Core.Sharing.Session&...` |
| All other API calls | `POST /sharing/webapi/entry.cgi` |
| Upload API (exception) | `POST /webapi/entry.cgi?api=SYNO.FileStation.Upload&...` |
| Download (file or folder) | `GET /fsdownload/webapi/file_download.cgi/{basename}?...` |

## Common Request Headers (authenticated API calls)

```
Content-Type: application/x-www-form-urlencoded
Cookie: sharing_sid={value}
X-Syno-Sharing: {sharing_id}
```

---

## Flow 1: Public Browse (no password)

### Step 1 — Load sharing page and get SID
```
GET /sharing/{sharing_id}
```

- Synology sets a `sharing_sid` cookie in the response (no auth needed for public shares)
- The HTML body contains a `<script src="...">` tag whose `src` URL embeds the `sharing_status` query parameter — this is the canonical source of truth for the share type
- `sharing_status` values: `none` (public), `password` (locked), `user` (requires DSM login — not supported)

Extract `sharing_status` by finding the script tag containing `SYNO.Core.Sharing.Session` and parsing its `src` URL:
```html
<script src="/sharing/webapi/entry.cgi?api=SYNO.Core.Sharing.Session&...&sharing_status=%22none%22&...">
```

### Step 2 — Get sharing context
```
GET /sharing/webapi/entry.cgi
  ?api=SYNO.Core.Sharing.Session
  &version=1
  &method=get
  &sharing_id="{sharing_id}"
  &sharing_status={status}
  &v={unix_timestamp}

Cookie: sharing_sid={value}
X-Syno-Sharing: {sharing_id}
```

**Response format — JavaScript variables, not JSON:**
```javascript
SYNO.SDS.Session = {
  "sharing_id": "pKGFcZ6A4",
  "sharing_status": "none",
  "hostname": "giristation"
}
;SYNO.SDS.ExtraSession = {
  "filename": "02.10.2009 - Tisch Evaluation",
  "is_folder": true,
  "is_sharing_upload": false,
  "status": 0
}
;
```

Parse by finding `VARNAME = ` and JSON-decoding the object that follows.

Key fields:

| Field | Source | Meaning |
|---|---|---|
| `SharingSession.sharing_status` | `SYNO.SDS.Session` | `"none"` / `"password"` / `"user"` |
| `ExtraSession.is_sharing_upload` | `SYNO.SDS.ExtraSession` | `true` → upload request share |
| `ExtraSession.filename` | `SYNO.SDS.ExtraSession` | Root folder name (browse shares) |
| `ExtraSession.is_folder` | `SYNO.SDS.ExtraSession` | `false` → single-file share |
| `ExtraSession.status` | `SYNO.SDS.ExtraSession` | Non-zero → share error (expired, etc.) |
| `ExtraSession.request_name` | `SYNO.SDS.ExtraSession` | Upload share display name |
| `ExtraSession.request_info` | `SYNO.SDS.ExtraSession` | Upload share description |

`ExtraSession.filename` sets the root folder path for listing: prefix with `/` → `/02.10.2009 - Tisch Evaluation`.

### Step 3 — List files
```
POST /sharing/webapi/entry.cgi

api=SYNO.FolderSharing.List
  &method=list
  &version=2
  &offset=0
  &limit=1000
  &sort_by="name"
  &sort_direction="ASC"
  &action="enum"
  &additional=["size","time","type"]
  &filetype="all"
  &folder_path="{/path/to/folder}"
  &_sharing_id="{sharing_id}"
```

Response:
```json
{
  "data": {
    "files": [
      {
        "name": "DSC_0997.JPG",
        "path": "/02.10.2009 - Tisch Evaluation/DSC_0997.JPG",
        "isdir": false,
        "additional": {
          "size": 3359239,
          "time": { "mtime": 1254476266 },
          "type": "JPG"
        }
      }
    ],
    "offset": 0,
    "total": 64
  },
  "success": true
}
```

For single-file shares, `SYNO.FolderSharing.List` does not apply. Treat the share as a synthetic single-entry listing using the share's own `filename` field.

---

## Flow 2: Password-Protected Browse

### Step 1 — Load sharing page
```
GET /sharing/{sharing_id}
```

For password-protected shares Synology does **not** set a `sharing_sid` cookie here. Parse `sharing_status=password` from the HTML script tag src.

### Step 2 — Get pre-auth context (no cookies)

Call `SYNO.Core.Sharing.Session` **without** the `Cookie` or `X-Syno-Sharing` headers. Synology loads this as a `<script>` tag on the locked page; the response still returns `ExtraSession` metadata (including `is_sharing_upload`, `request_name`, `request_info`) before authentication.

```
GET /sharing/webapi/entry.cgi
  ?api=SYNO.Core.Sharing.Session
  &version=1
  &method=get
  &sharing_id="{sharing_id}"
  &sharing_status=password
  &v={unix_timestamp}
```
(No `Cookie` or `X-Syno-Sharing` header)

Use the result only to determine `is_sharing_upload` so the correct UI can be shown before the password prompt.

### Step 3 — Authenticate
```
POST /sharing/webapi/entry.cgi/SYNO.Core.Sharing.Login

api=SYNO.Core.Sharing.Login
  &method=login
  &version=1
  &sharing_id={sharing_id}
  &password={plaintext_password}
```

> **Note:** `sharing_id` and `password` are **plain values** here — no JSON-quoting. This endpoint has no `Cookie` or `X-Syno-Sharing` headers.

Response:
```json
{
  "data": { "sharing_sid": "61XFf4kxM6fdwCj50whyl9n2rnzaU1CW" },
  "success": true
}
```

The `sharing_sid` value is returned both in the JSON body and as a `Set-Cookie` response header (which also carries TTL attributes `MaxAge`/`Expires`).

Error code `1001` = wrong password.

### Step 4 — Get authenticated context
Repeat the `SYNO.Core.Sharing.Session` call, now with the `Cookie: sharing_sid=...` and `X-Syno-Sharing` headers set.

**Quirk:** For password-protected browse shares, Synology omits `ExtraSession.filename` from the Session response even after authentication. Fetch it separately:

```
POST /sharing/webapi/entry.cgi

api=SYNO.Core.Sharing.Initdata
  &method=get
  &version=1
```

Response:
```json
{
  "data": {
    "Private": { "filename": "FolderName" }
  },
  "success": true
}
```

### Step 5 — List files
Same as Flow 1 Step 3, now with the authenticated `sharing_sid` cookie.

---

## Flow 3: User-Authenticated Shares (not supported)

When `sharing_status == "user"`, the share requires a full DSM account login. This is not supported; return an error to the user.

---

## Flow 4: Upload Request

When `ExtraSession.is_sharing_upload == true` the share is a drop-box. Users upload files; they cannot browse existing content.

### Step 1 — Check upload permission
```
POST /sharing/webapi/entry.cgi/SYNO.FileStation.CheckPermission

api=SYNO.FileStation.CheckPermission
  &method=write
  &version=3
  &sharing_id="{sharing_id}"
  &uploader_name="{name}"
  &size={bytes}
  &filename="{filename}"
  &overwrite=true
```

Response:
```json
{ "data": {}, "success": true }
```

Validates that the uploader can write before sending the file body. Also lets Synology enforce quota limits early.

### Step 2 — Upload file
```
POST /webapi/entry.cgi
  ?api=SYNO.FileStation.Upload
  &method=upload
  &version=2
  &_sharing_id="{sharing_id}"

Content-Type: multipart/form-data
Cookie: sharing_sid={value}
X-Syno-Sharing: {sharing_id}
```

> **Note:** This uses `/webapi/entry.cgi`, not `/sharing/webapi/entry.cgi`. The `api`, `method`, `version` appear in the URL query string; `_sharing_id` is JSON-quoted in the query string.

Multipart fields:

| Field | Value | Notes |
|---|---|---|
| `sharing_id` | `{sharing_id}` | Plain (not JSON-quoted) |
| `uploader_name` | `{name}` | Plain |
| `size` | `{bytes}` | Decimal integer |
| `mtime` | `{milliseconds}` | Unix epoch in milliseconds |
| `overwrite` | `true` | |
| `file` | binary | field name `file`, filename attribute set |

Response:
```json
{ "success": true }
```

---

## Download

Both files and folders use the same endpoint. Folders are returned as a streaming ZIP.

```
GET /fsdownload/webapi/file_download.cgi/{basename}
  ?dlink="{hex_encoded_path}"
  &noCache={unix_timestamp_ms}
  &_sharing_id="{sharing_id}"
  &api=SYNO.FolderSharing.Download
  &version=2
  &method=download
  &mode=download
  &stdhtml=true

Cookie: sharing_sid={value}
X-Syno-Sharing: {sharing_id}
```

- `{basename}` — filename for the file, or `{foldername}.zip` for a folder (browser hint only)
- `dlink` — see encoding below
- `noCache` — current time in milliseconds (prevents caching)
- Returns binary file data or streaming ZIP

### dlink encoding

```
dlink = '"' + hex(utf8(path)) + '"'
```

Example:
```
path:  /02.10.2009 - Tisch Evaluation/DSC_0997.JPG
utf-8: 2f 30 32 2e 31 30 2e 32 30 30 39 ...
dlink: "2f30322e31302e323030392e2e2e"
```

The result is wrapped in literal double quotes that become part of the encoded query parameter value.

---

## Parameter Encoding Reference

### synoQuote — JSON-quoted string parameters

Most string parameters in POST bodies are wrapped in literal double quotes. Before wrapping, strip any backslashes and double-quotes from the value to prevent injection:

```
synoQuote(s) = '"' + s.replace('\', '').replace('"', '') + '"'
```

Examples:
```
sharingID "pKGFcZ6A4"  →  "pKGFcZ6A4"
path "/My Folder"      →  "/My Folder"
filename 'file"1.jpg'  →  "file1.jpg"
```

**Used for:** `sharing_id`, `folder_path`, `_sharing_id`, `filename`, `uploader_name` in API calls.  
**Not used for:** `password` and `sharing_id` in `SYNO.Core.Sharing.Login`; multipart upload fields.

---

## Error Codes

| Code | Meaning |
|---|---|
| `105` | Permission denied |
| `114` | Share link expired or invalid |
| `408` | Invalid parameter |
| `1001` | Wrong password (login only) |

Non-zero `ExtraSession.status` maps to the same error codes.

---

## Minimum API Calls (per use case)

**Browse public folder:**
1. `GET /sharing/{id}` — get `sharing_sid` cookie, parse `sharing_status` from HTML
2. `SYNO.Core.Sharing.Session.get` — get root folder name, detect single-file share
3. `SYNO.FolderSharing.List.list` — list files (repeat per subfolder as needed)
4. `SYNO.FolderSharing.Download` — stream file or folder ZIP on demand

**Browse password-protected folder:**
1. `GET /sharing/{id}` — detect `sharing_status=password` (no SID cookie issued)
2. `SYNO.Core.Sharing.Session.get` (no auth) — detect `is_sharing_upload` before the password prompt
3. `SYNO.Core.Sharing.Login.login` — authenticate with password, get `sharing_sid`
4. `SYNO.Core.Sharing.Session.get` (authenticated) — get full share context
5. `SYNO.Core.Sharing.Initdata.get` — get root folder name (password shares only; Session omits it)
6. `SYNO.FolderSharing.List.list` — list files

**Upload request:**
1. `GET /sharing/{id}` — get `sharing_sid` cookie
2. `SYNO.Core.Sharing.Session.get` — confirm `is_sharing_upload=true`, get `request_name` / `request_info`
3. `SYNO.FileStation.CheckPermission.write` — validate permission before each file upload
4. `SYNO.FileStation.Upload.upload` — upload file (stream via multipart)

**Download single file:**
- `SYNO.FolderSharing.Download` — one GET, streams file directly (no separate API calls needed beyond the browse flow that provided the path)

**Download folder as ZIP:**
- `SYNO.FolderSharing.Download` — one GET with folder path, Synology streams ZIP on the fly

---

## Key Quirks Summary

| Quirk | Detail |
|---|---|
| Session response format | JavaScript variable assignments, not JSON — parse `VARNAME = {...}` |
| Pre-auth Session call | No `Cookie`/`X-Syno-Sharing` headers; Synology serves it as a `<script>` tag |
| Password-protected folder name | Not in Session response after auth; need a separate `Initdata` call |
| Login parameter encoding | `sharing_id` and `password` are **plain**, not JSON-quoted |
| Upload base URL | `/webapi/entry.cgi` (not `/sharing/webapi/entry.cgi`) |
| Upload API params | In query string as JSON-quoted; form fields are plain values |
| `sharing_status` source | HTML script tag src URL, not the Session API response (which just echoes the parameter) |
