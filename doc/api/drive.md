# Synology Drive Sharing API

Synology Drive sharing links are created by the Synology Drive app. Drive is a newer
package than FileStation/Photos and uses a noticeably different pattern: instead of a
JSON or JS-object session blob, the client bootstraps itself from **executable
JavaScript that assigns global functions** (`window.getDriveXxx = function(){return ...;}`),
and upload-request pages embed all their metadata **directly in the initial HTML**
with no follow-up call needed at all.

## URL Patterns

| Type | Browser URL | API Base |
|---|---|---|
| Browse (public/password) | `/drive/d/s/{permanent_link}/{sharing_link}` | `POST /drive/d/s/{permanent_link}/webapi/entry.cgi[/{API_NAME}]` |
| Browse (invite-only, DSM login) | `/drive/d/f/{permanent_link}` | `POST /drive/d/f/webapi/entry.cgi[/{API_NAME}]` |
| Upload request (file request) | `/drive/d/r/{file_request_id}/{sharing_link}` | `POST /drive/d/r/{file_request_id}/webapi/entry.cgi[/{API_NAME}]` |
| Bootstrap script (browse only) | — | `GET .../webapi/entry.cgi?api=SYNO.SynologyDrive.Shard&method=getjs&...` |
| Download (single file) | — | `GET .../webapi/entry.cgi/{filename}?api=SYNO.SynologyDrive.Files&method=download&...` |
| Download (folder or multi-item, ZIP) | — | async task, then `GET .../webapi/entry.cgi/SYNO.SynologyDrive.Files/{archive_name}.zip?...` |
| Upload (file request only) | — | `POST .../webapi/entry.cgi/SYNO.SynologyDrive.Files?api=SYNO.SynologyDrive.Files&method=upload&...` |

**API base rule:** the base for all `entry.cgi` calls is the share's browser URL with its
**last path segment removed** (the `{sharing_link}` part for `/d/s/` and `/d/r/` links; the
`{permanent_link}` itself for `/d/f/` links, since those URLs have only one segment).

## Cookies

Unlike FileStation (`sharing_sid`) and Photos (`sharing_sid`), Drive names its cookie
after the share itself, so multiple Drive shares can be open in the same browser at once
without clobbering each other's session:

| Share type | Cookie name | Value |
|---|---|---|
| Browse (`/d/s/`) | `drive-sharing-{sharing_link}` | opaque `sharing_token` |
| Upload request (`/d/r/`) | `drive-request-{sharing_link}` | opaque `sharing_token` |

The same `sharing_token` value is *also* sent as an explicit, JSON-quoted request
parameter (`sharing_token="{token}"`) on almost every API call. There is no
`X-Syno-Sharing`-style header as in FileStation/Photos — the cookie and the request
parameter are the only carriers of the token. The upload endpoint itself (see below) is
the one exception observed: its query string carries no `sharing_token` at all and it
appears to rely on the cookie alone.

For password-protected shares, the cookie is explicitly cleared on the initial page load
(`Set-Cookie: drive-sharing-{link}=; expires=1970...`) until authentication succeeds.

---

## Initial Bootstrap (Browse shares only)

### Step 1 — Load the landing page
```
GET /drive/d/s/{permanent_link}/{sharing_link}
```
or, for invite-only links:
```
GET /drive/d/f/{permanent_link}
```

This sets the `drive-sharing-{sharing_link}` cookie (empty/expired for password shares).
The HTML itself is a thin ExtJS shell; it contains a `<script src="...">` tag that must be
fetched to get any information about the share:

```html
<script type="text/javascript" src="webapi/entry.cgi?api=SYNO.SynologyDrive.Shard&version=1&method=getjs&permanent_link=%22{permanent_link}%22&sharing_type=%22public_sharing%22&sharing_link=%22{sharing_link}%22&v={timestamp}&_dc={timestamp}"></script>
```

`sharing_type` is `"public_sharing"` for `/d/s/` links and `"simple_sharing"` for `/d/f/`
links (which also omit the `sharing_link` parameter entirely, since there isn't one).

### Step 2 — Fetch the bootstrap script
```
GET .../webapi/entry.cgi?api=SYNO.SynologyDrive.Shard&version=1&method=getjs&permanent_link="{permanent_link}"&sharing_type="public_sharing"&sharing_link="{sharing_link}"&v={timestamp}

Cookie: drive-sharing-{sharing_link}={token}   (if already known; not required for this call)
```

**This is executable JavaScript, not JSON.** The response assigns a handful of global
functions, each a JSON value wrapped in a trivial function body:

```javascript
window.getDriveShareMode=function(){return 'public';}
window.getDriveErrCode=function(){return 0;}
window.getDriveAllowToShare=function(){return true;}
window.getDriveLink=function(){return "194HpbSI2AWtijAKdrv7Lcgze5Pi01Qx";}
window.getDriveSharingLink=function(){return "hca5SgpB8kZKhQr1Wy8mrfl36bjeSz3B-fLFAX_T0WQ0";}
window.getDriveTexts=function(){return {...localization strings, irrelevant...};}
window.getDriveFile=function(){return {...node object, see below...};}
window.getOfficeTexts=function(){return {};}
window.getSASTexts=function(){return {...localization strings, irrelevant...};}
```

Unlike Photos' `window.SYNO` (unquoted JS object literal), the payload inside each
`return ... ;` here is valid JSON — but still extract it with brace-matching from the
first `{`/`"`/literal to the matching close before the trailing `;}`, the same way
`photos.md` recommends for `FotoSharing`, rather than trying to `eval`/parse the whole
script. `getDriveFile`'s JSON can itself contain `;` inside string values in theory, so
don't just split on the literal string `;}`.

Key fields:

| Field | Meaning |
|---|---|
| `getDriveErrCode()` | `0` = OK; `1037` = password required; `1002` = requires DSM account login (invite-only, not supported) |
| `getDriveFile()` | Full node object for the shared file/folder (empty/absent pre-auth on password shares) |
| `getDriveLink()` | Echoes `permanent_link` |
| `getDriveSharingLink()` | Echoes `sharing_link` (empty for `/d/f/` links) |
| `getDriveShareMode()` | Observed as `'public'` in all cases tested, including password and invite-only links — **not a reliable signal**, use `getDriveErrCode()` instead |

When `getDriveErrCode()` is non-zero and it's `1037`, `getDriveFile()`'s body is empty
(literally `return ;`) — treat that as "no data yet, show the password prompt".

### The shared node object (`getDriveFile()` / list items)

The same object shape is used for the shared item itself and for every entry returned by
the folder listing API. Example (a plain-text file shared with view+download, no password):

```json
{
  "file_id": "962069303782322533",
  "permanent_link": "194HpbSI2AWtijAKdrv7Lcgze5Pi01Qx",
  "name": "test_2.txt",
  "path": "/test_2.txt",
  "display_path": "/shared-with-me/test_2.txt",
  "type": "file",
  "content_type": "document",
  "size": 14,
  "hash": "3b487cf6856af7e330bc4b1b7d977ef8",
  "modified_time": 1784126349,
  "created_time": 1784126392,
  "owner": { "display_name": "martin", "name": "martin", "uid": 1026 },
  "parent_id": "686134738643102457",
  "adv_shared": true,
  "adv_shared_info": { "created_time": 1784126403, "due_date": 0, "has_password": false },
  "disable_download": false,
  "capabilities": {
    "can_read": true, "can_preview": true, "can_download": true, "can_write": false,
    "can_comment": false, "can_share": false, "can_delete": false, "can_rename": false,
    "can_organize": false, "can_sync": true, "can_lock": false, "can_auto_lock": false,
    "can_encrypt": false
  }
}
```

- `type` (or `content_type: "dir"` vs anything else) distinguishes folder from file —
  `type` is the more direct signal: `"dir"` for folders, `"file"` for everything else.
- `adv_shared_info.has_password` reflects whether *this specific link* is password
  protected (confirmed `true` on the authenticated re-fetch of a password link).

**Permission tiers, as observed on three otherwise-identical test files:**

| Link setting | `can_read` | `can_preview` | `can_download` | `can_write` | `can_comment` |
|---|---|---|---|---|---|
| View only | `false` | `true` | `false` | `false` | `false` |
| View + download | `true` | `true` | `true` | `false` | `false` |
| View + download + edit | `true` | `true` | `true` | `true` | `true` |

Quirk: `can_read` is `false` even though `can_preview` is `true` on view-only links —
`can_preview` is the one that stays `true` across all tiers, `can_download` is the
reliable download-permission signal, and `can_write` is the edit signal (`can_write`
always implies `can_download` in the data observed, matching the "editing implies
downloading" behavior described by the DSM UI).

---

## Flow 1: Public Browse (no password)

1. `GET /drive/d/s/{permanent_link}/{sharing_link}` — get `drive-sharing-{link}` cookie
2. `GET .../webapi/entry.cgi?api=SYNO.SynologyDrive.Shard&method=getjs&...` — parse
   `getDriveErrCode` (expect `0`) and `getDriveFile` (root node + capabilities)
3. If `getDriveFile().type === "dir"`: list children (see **Folder Listing** below)
4. Download as needed

## Flow 2: Password-Protected Browse

1. `GET /drive/d/s/{permanent_link}/{sharing_link}` — cookie is cleared
   (`Set-Cookie: drive-sharing-{link}=; expires=1970...`)
2. `GET .../webapi/entry.cgi?api=SYNO.SynologyDrive.Shard&method=getjs&...` (no cookie
   yet) — `getDriveErrCode()` returns `1037`, `getDriveFile()` is empty
3. Authenticate:
   ```
   POST .../webapi/entry.cgi
   api=SYNO.SynologyDrive.AdvanceSharing.Public&method=auth&version=1
     &sharing_link="{sharing_link}"
     &password="{plaintext_password}"
   ```
   Success:
   ```json
   { "data": { "sharing_token": "..." }, "success": true }
   ```
   `Set-Cookie: drive-sharing-{sharing_link}={sharing_token}` is issued at the same time.

   Wrong password:
   ```json
   { "error": { "code": 1037, "errors": { "line": 48, "message": "protocol error, reason = 'sharing_link password error'" } }, "success": false }
   ```
   Note: unlike FileStation/Photos' `SYNO.Core.Sharing.Login`, **`password` is
   JSON-quoted here** (`password="gh78ut"`, not `password=gh78ut`).
4. Repeat step 2 with the cookie set — `getDriveErrCode()` is now `0` and `getDriveFile()`
   returns the full node (with `adv_shared_info.has_password: true`).
5. Continue as Flow 1 from step 3.

## Flow 3: Invite-Only (not supported)

`/drive/d/f/{permanent_link}` links require a DSM account. There is no HTTP redirect
(unlike Photos) — the page loads normally (HTTP 200) but the bootstrap script's
`getDriveErrCode()` returns **`1002`**. Detect this and refuse support; do not attempt to
follow a login flow.

---

## Folder Listing

```
POST .../webapi/entry.cgi
api=SYNO.SynologyDrive.Files&method=list&version=2
  &offset=0&limit=1000
  &filter={"include_transient":true}
  &sort_by="name"&sort_direction="asc"
  &path="id:{file_id}"
  &sharing_token="{token}"
```

- `path` uses the `id:{file_id}` scheme, not a filesystem path — `{file_id}` is the
  shared root's `file_id` for the top level, or a child's `file_id` (from a previous
  listing) to descend into a subfolder.
- `filter` is a JSON object, itself JSON-quoted as a whole parameter.

Response:
```json
{
  "data": {
    "items": [ { "...": "same node shape as getDriveFile() above" } ],
    "total": 3
  },
  "success": true
}
```

---

## Download

### Single file (or preview)
```
GET .../webapi/entry.cgi/{filename}
  ?api=SYNO.SynologyDrive.Files&method=download&version=2
  &files=["id:{file_id}"]
  &force_download={true|false}
  &is_preview={false|true}
  &sharing_token="{token}"
```

- Real download (attachment): `force_download=true&is_preview=false`. Response:
  `Content-Disposition: attachment; filename="..."`, streamed binary body.
- Inline preview (used by the viewer, e.g. for the text/image preview pane):
  `force_download=false&is_preview=true`.
- This streams directly — no async task involved — as long as exactly one, non-folder
  item is requested.

### Folder (or any multi-item) download — ZIP via async task

Requesting a folder (or presumably multiple items) goes through a 3-step async
compression job instead of streaming directly:

1. **Dry run** (feasibility check):
   ```
   POST .../webapi/entry.cgi
   api=SYNO.SynologyDrive.Files&method=download&version=2&dry_run=true
     &archive_name="{name}"&download_type="download"
     &files=["id:{file_id}"]&force_download=true&json_error=true
     &sharing_token="{token}"
   ```
   Response: `{"data":{"result":null},"success":true}`
2. **Start the job** (same params, no `dry_run`):
   ```json
   { "data": { "async_task_id": "task-5" }, "success": true }
   ```
3. **Poll for completion:**
   ```
   POST .../webapi/entry.cgi
   api=SYNO.SynologyDrive.Tasks&method=get&version=1&task_id="task-5"&sharing_token="{token}"
   ```
   ```json
   {
     "data": {
       "progress": 100,
       "status": "finished",
       "task_id": "task-5",
       "result": { "action": "download", "archive_name": "upload_req.zip", "total_size": 14, "processed_size": 17 }
     },
     "success": true
   }
   ```
4. **Download the finished archive:**
   ```
   GET .../webapi/entry.cgi/SYNO.SynologyDrive.Files/{archive_name}.zip
     ?api=SYNO.SynologyDrive.Files&method=download&version=2
     &task_id="task-5"&_dc={timestamp}&sharing_token="{token}"
   ```
   Note the extra `SYNO.SynologyDrive.Files/` path segment before the filename — absent
   in the single-file download URL.

---

## Flow 4: Upload Request (File Request)

URL: `/drive/d/r/{file_request_id}/{sharing_link}`. This is where Drive diverges most
from FileStation/Photos: **all metadata is embedded directly in the initial HTML as
plain JS variable assignments** — no bootstrap script call is needed at all.

### Step 1 — Load the landing page
```
GET /drive/d/r/{file_request_id}/{sharing_link}
```

Response HTML contains (before the actual SPA markup):
```javascript
getDriveLink = () => "194Hvk7eenZHK4qcuVqFJyxll0PLGpiz"        // target folder's permanent_link
getDriveSharingLink = () => "3A-xhFgd2MhkPPLGaraqE0u5x_cTLNCR-qLIAWFz2WQ0"
getDriveFileRequestState = () => "file_request_ok"              // or "file_request_password"
getDriveDSID = () => "dbf67fdb3d327908002332043ce6a758"
getDriveFileRequestCreator = () => "martin"
getDriveFileRequestTitle = () => "This is a test"
getDriveFileRequestDescription = () => "Description bla bla bla"
getDriveFileRequestIdentifier = () => "create_folder"
getDriveFileRequestExpire = () => 0
getDriveFileRequestId = () => "YdSLCQYI4nsz5juX"                 // echoes the URL's file_request_id
getDriveFileId = () => "962069652524020178"                      // target folder's file_id
getDriveFileRequestHasDueDateHour = () => false
```

These are plain `() => value` arrow functions assigning globals — parse with the same
kind of targeted regex/brace-matching as the bootstrap script, no separate request
required. `getDriveFileRequestState()` is the password signal: `"file_request_ok"` (no
password) vs `"file_request_password"`.

The `drive-request-{sharing_link}` cookie is set on this same response for public (no
password) requests; for password-protected requests it is unset until auth succeeds.

### Step 2 (password requests only) — Authenticate
```
POST .../webapi/entry.cgi
api=SYNO.SynologyDrive.FileRequest.Public&method=auth&version=1
  &password="{plaintext_password}"
  &encryption=["password"]
  &sharing_link="{sharing_link}"
```
On success this sets `Set-Cookie: drive-request-{sharing_link}={sharing_token}` (response
body observed empty — the cookie is the actual signal). `encryption` is a JSON array,
suggesting room for other auth methods Synology hasn't needed to document.

### Step 3 — Get request info (real client does this; likely skippable)
```
POST .../webapi/entry.cgi
api=SYNO.SynologyDrive.FileRequest.Public&method=get&version=1
  &sharing_link="{sharing_link}"
  &sharing_token="{token}"
```
```json
{
  "data": {
    "description": "Description bla bla bla",
    "due_date": 0,
    "file_id": "962069652524020178",
    "file_request_id": "YdSLCQYI4nsz5juX",
    "file_request_status": "active",
    "identifier": "create_folder",
    "title": "This is a test",
    "uid": 1026
  },
  "success": true
}
```

**This call is very likely unnecessary for our proxy.** Every field it returns is either
already present in the inline `getDriveFileRequestXxx` vars from the landing page HTML
(`description`, `file_id`, `file_request_id`, `identifier`, `title`, `due_date` ≈
`getDriveFileRequestExpire`), or obtainable from the next call's response (`uid` is the
owner's numeric id, `getDriveFileRequestCreator` already gives the display name). The one
field with no inline equivalent, `file_request_status`, would only matter as a live
"is this request still active" check — plausible if a tab is left open long enough for
the request to expire, but not confirmed as load-bearing.

Confirmed live: skipping this call entirely and going straight from the landing page's
inline vars (plus the `sharing_token` read directly from the `drive-sharing`/`drive-request`
cookie set on page load — no API call needed to obtain it either) to Step 4
(`Files.create`) worked without any error. We were not able to cleanly re-confirm the
following upload step in isolation — repeated attempts hit an unrelated `"blocked by
flock"` error (code `1054`) that reproduced even against a brand-new target folder and
filename, indicating a stuck server-side lock from earlier interrupted test uploads
rather than anything caused by skipping this call. Since the upload endpoint's only
observed auth dependency is the same cookie-derived `sharing_token` that already worked
for `Files.create`, there's no mechanism by which this call would be required for it
either — treat it as skippable, but be aware it hasn't been end-to-end verified.

### Step 4 — Create a per-uploader subfolder
The real client always creates a subfolder named after the uploader inside the target
folder before uploading into it:
```
POST .../webapi/entry.cgi
api=SYNO.SynologyDrive.Files&method=create&version=2
  &type="folder"
  &path="id:{target_folder_file_id}/{uploader_name}"
  &conflict_action="autorename"
  &sharing_type="file_request"
  &sharing_token="{token}"
```
Response: the new subfolder's node object (same shape as `getDriveFile()`), including its
`file_id` — used as the upload destination in the next step. `conflict_action=autorename`
means re-uploading under the same name creates `TestUser (1)`, etc., rather than erroring.

### Step 5 — Upload the file (slice-upload protocol)

This is a custom chunked-upload protocol, distinct from FileStation's single-POST
`SYNO.FileStation.Upload` and Photos' single-POST `SYNO.Foto.Upload.PhotoRequestItem`:

**First request — reserve the upload:**
```
POST .../webapi/entry.cgi/SYNO.SynologyDrive.Files
  ?api=SYNO.SynologyDrive.Files&method=upload&version=2
  &path="id:{uploader_folder_id}/{filename}"
  &conflict_action="autorename"&type="file"&json_error=true
  &reserved_size={total_file_size_bytes}

X-Type-Name: SLICEUPLOAD
X-File-Chunk-End: false
X-Tmp-File: {client-generated opaque id, e.g. a uuid}
Content-Type: multipart/form-data; boundary=...
```
No `sharing_token` query parameter is present on this endpoint — it relies on the
`drive-request-{sharing_link}` cookie alone. This call carries no file bytes (or an
empty file part); response:
```json
{ "data": { "tmp_newly_created": true, "uploaded_size": 0 }, "success": true }
```

**Final request — send the data:**
```
POST .../webapi/entry.cgi/SYNO.SynologyDrive.Files
  ?api=SYNO.SynologyDrive.Files&method=upload&version=2
  &path="id:{uploader_folder_id}/{filename}"
  &conflict_action="autorename"&type="file"&json_error=true

X-Type-Name: SLICEUPLOAD
X-File-Chunk-End: true
X-Tmp-File: {same id as the first request}
Content-Type: multipart/form-data; boundary=...
```
(no `reserved_size` this time). Multipart body carries the actual file content under a
`file` field. Response is the completed file's full node object (same shape as
`getDriveFile()`), `success: true`.

Both requests share the same `X-Tmp-File` value, which is how the server correlates
chunks belonging to one logical upload. For a single small test file the real client
still issues exactly these two requests (an empty reservation, then the full payload) —
it does not appear to split file bytes across more than one data-carrying request for
small files; presumably larger files would add more `X-File-Chunk-End: false` requests
with real byte ranges in between. **Not confirmed:** combining `reserved_size` and
`X-File-Chunk-End: true` into a single request was attempted and returned an HTTP 502
from the backend — treat the two-step sequence as required rather than trying to
collapse it.

### Step 6 — Notify the owner
```
POST .../webapi/entry.cgi
api=SYNO.SynologyDrive.FileRequest.Public&method=notify&version=1
  &uploader="{uploader_name}"
  &request_title="{title}"
  &upload_items=["{filename1}", "{filename2}", ...]
  &path="id:{uploader_folder_id}"
  &sharing_token="{token}"
```
```json
{ "success": true }
```
Sent once after all files in a batch have finished uploading (not per-file).

---

## Parameter Encoding Reference

Drive uses the same `synoQuote` convention documented in `sharing.md` (JSON-quote most
string parameters), but applies it more broadly:

```
synoQuote(s) = '"' + s.replace('\', '').replace('"', '') + '"'
```

**Quoted here (differs from FileStation/Photos):** `password` in both
`SYNO.SynologyDrive.AdvanceSharing.Public.auth` and `SYNO.SynologyDrive.FileRequest.Public.auth`
— FileStation/Photos' `SYNO.Core.Sharing.Login` leaves `password` unquoted.

**Also quoted:** `sharing_link`, `permanent_link`, `sharing_token`, `path`, `sort_by`,
`sort_direction`, `archive_name`, `download_type`, `task_id`, `uploader`, `request_title`,
`type`, `conflict_action`, `sharing_type`.

**JSON arrays, still quoted as a whole:** `files=["id:..."]`, `upload_items=["name1"]`,
`encryption=["password"]`, `filter={"include_transient":true}`.

**Not quoted:** the multipart form fields of the slice-upload request; the custom
`X-*` headers.

---

## Error Codes

| Code | Meaning |
|---|---|
| `0` | OK (`getDriveErrCode`) |
| `1002` | Requires DSM account login (invite-only share) — not supported |
| `1037` | Password required / wrong password |
| `1054` | "blocked by flock" — observed on `Files.upload`; looked like a stuck server-side lock from an earlier interrupted upload attempt rather than a real request error (reproduced against a brand-new target folder/filename, so it wasn't a per-path lock) |

---

## Minimum API Calls (per use case)

**Browse public file/folder:**
1. `GET /drive/d/s/{id}/{link}` — get cookie
2. `SYNO.SynologyDrive.Shard.getjs` — parse `getDriveErrCode`/`getDriveFile`
3. `SYNO.SynologyDrive.Files.list` — list children (folders only, repeat per subfolder)
4. Download endpoint on demand

**Browse password-protected file/folder:**
1. `GET /drive/d/s/{id}/{link}` — cookie cleared
2. `SYNO.SynologyDrive.Shard.getjs` (no auth) — detect `getDriveErrCode() === 1037`
3. `SYNO.SynologyDrive.AdvanceSharing.Public.auth` — authenticate, get `sharing_token` + cookie
4. `SYNO.SynologyDrive.Shard.getjs` (authenticated) — get full node info
5. `SYNO.SynologyDrive.Files.list` — list children (folders only)

**Download single file:**
- `SYNO.SynologyDrive.Files.download` (`force_download=true&is_preview=false`) — one GET, streams directly

**Download folder / multiple items as ZIP:**
1. `SYNO.SynologyDrive.Files.download` with `dry_run=true` — feasibility check
2. `SYNO.SynologyDrive.Files.download` — start async job, get `async_task_id`
3. `SYNO.SynologyDrive.Tasks.get` — poll until `status: "finished"`
4. `SYNO.SynologyDrive.Files/{archive}.zip` download endpoint — stream the result

**Upload request (public):**
1. `GET /drive/d/r/{id}/{link}` — parse inline `getDriveFileRequestState`/`getDriveFileId`/etc, get cookie (and the `sharing_token` it carries)
2. ~~`SYNO.SynologyDrive.FileRequest.Public.get`~~ — real client calls this, but everything
   it returns duplicates the inline vars from step 1; confirmed skippable for
   `Files.create` (step 3), very likely skippable for `Files.upload` too (see note above)
3. `SYNO.SynologyDrive.Files.create` (`type="folder"`) — create per-uploader subfolder
4. `SYNO.SynologyDrive.Files.upload` ×2 (reserve, then final chunk) — per file
5. `SYNO.SynologyDrive.FileRequest.Public.notify` — notify owner once per batch

**Upload request (password-protected):**
1. `GET /drive/d/r/{id}/{link}` — detect `getDriveFileRequestState() === "file_request_password"`
2. `SYNO.SynologyDrive.FileRequest.Public.auth` — authenticate, get cookie
3. Same as steps 2–5 above (`FileRequest.Public.get` likewise skippable)

---

## Key Differences from FileStation `/sharing/` and Photos `/photo/`

| | FileStation / Photos | Drive |
|---|---|---|
| Session bootstrap format | JSON-ish JS variable assignment / unquoted JS object | Executable JS assigning `window.getDriveXxx` functions, each returning valid JSON |
| Upload-request metadata | Requires an API call (`get_photo_request_info`) or session parse | Embedded directly in the initial HTML as plain `() => value` assignments — no call needed |
| Auth token transport | Cookie (`sharing_sid`) + `X-Syno-Sharing` header | Cookie (`drive-sharing-`/`drive-request-{link}`, name scoped per share) + `sharing_token` request parameter; no custom header |
| Password quoting | `password` plain/unquoted in `SYNO.Core.Sharing.Login` | `password` JSON-quoted in both Drive auth endpoints |
| Folder download | Direct streaming ZIP, single request | Async task: dry-run → start job → poll → download |
| Upload protocol | Single multipart POST | Custom "SLICEUPLOAD" chunked protocol (`X-Tmp-File`, `X-File-Chunk-End`, `reserved_size`) across ≥2 requests |
| Invite-only detection | Redirect (Photos) / `sharing_status=user` (FileStation) | No redirect; `getDriveErrCode() === 1002` on an HTTP 200 page |
| Permission model | Coarse (view/download flags on the share) | Full `capabilities` object (`can_read`, `can_preview`, `can_download`, `can_write`, ...) on every node |
