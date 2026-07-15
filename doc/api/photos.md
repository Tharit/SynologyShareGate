# Synology Photos Sharing API

Photos sharing links are created by the Synology Photos app. They differ significantly from `/sharing/XXXX` (FileStation) links.

## URL Patterns

| Type | Browser URL | API Base |
|---|---|---|
| Browse / view | `/photo/mo/sharing/{passphrase}` | `POST /photo/mo/sharing/webapi/entry.cgi/{API_NAME}` |
| Upload request | `/photo/mo/request/{passphrase}` | `POST /photo/mo/request/webapi/entry.cgi/{API_NAME}` |
| Thumbnails | — | `GET /photo/synofoto/api/v2/p/Thumbnail/get` |
| Download (per item) | — | `POST /photo/mo/sharing/webapi/entry.cgi` (`SYNO.Foto.Download`) |
| Download (full album) | — | `POST /photo/mo/sharing/webapi/entry.cgi/{filename}.zip` (`SYNO.Foto.Browse.Album`) |

## Common Request Headers (authenticated API calls)

```
Content-Type: application/x-www-form-urlencoded
Cookie: sharing_sid={value}
X-Syno-Sharing: {passphrase}
```

---

## Initial Page Parse

All browse flows start with a `GET` to the share landing page. The HTML response embeds a `window.SYNO` object that carries all the context needed before making any API calls:

For Browsing:
```
GET /photo/mo/sharing/{passphrase}
```
Or for Upload Requests:
```
GET /photo/mo/request/{passphrase}
```

Embedded JavaScript (look for `window.SYNO = {`):
```javascript
window.SYNO = {
  SDS: {
    Session: {
      sharing: true,
      sharing_status: "none",   // "none" | "password"
      sharing_id: "EWvNhI0J0",  // equals the passphrase
    },
  },
  FotoSharing: {
    enable_password: false,
    passphrase: "EWvNhI0J0",
    privacy_type: "public-download",  // "public-view" | "public-download"
  },
};
```

Key fields:

| Field | Meaning |
|---|---|
| `FotoSharing.enable_password` | `true` → show password form before listing photos |
| `FotoSharing.privacy_type` | `"public-download"` → offer download buttons; `"public-view"` → view only |
| `SDS.Session.sharing_id` | The passphrase value (use for all subsequent API calls) |

**Invite-only detection:** The landing page issues an HTTP redirect to the DSM login URL (`{nas}:5001/?launchApp=SYNO.Foto.Sharing.AppInstance&...`) instead of returning HTML with `window.SYNO`. Detect by observing a redirect response.

Both upload and browse pages include `window.SYNO`, but the upload page's `FotoSharing` object only has `passphrase` — no `enable_password` or `privacy_type`. 

**Cookie:** For public browse shares and upload requests, `Set-Cookie: sharing_sid=...` is included in this response. For password-protected browse shares, no cookie is set here — it comes from Login.

---

## Flow 1: Public Browse (no password)

### Step 1 — Load landing page
```
GET /photo/mo/sharing/{passphrase}
```

Parse `window.SYNO` from the HTML. Confirm `enable_password: false` and note `privacy_type`. The `sharing_sid` cookie is set in this response.

### Step 2 — Get album info
```
POST /photo/mo/sharing/webapi/entry.cgi/SYNO.Foto.Browse.Album

api=SYNO.Foto.Browse.Album&method=get&version=4
  &passphrase="LESyyu3kf"
  &additional=["sharing_info","flex_section","provider_count","thumbnail"]
```

Response (key fields):
```json
{
  "data": {
    "list": [{
      "id": 38,
      "name": "2026-07-14",
      "item_count": 3
    }]
  },
  "success": true
}
```

Provides album name and total item count for pagination.

### Step 3 — List photos
```
POST /photo/mo/sharing/webapi/entry.cgi/SYNO.Foto.Browse.Item

api=SYNO.Foto.Browse.Item&method=list&version=4
  &passphrase="LESyyu3kf"
  &offset=0&limit=100
  &sort_by="takentime"&sort_direction="asc"
  &additional=["thumbnail","resolution","orientation","video_convert","video_meta","provider_user_id"]
```

Response:
```json
{
  "success": true,
  "data": {
    "list": [{
      "id": 476423,
      "filename": "DSC_0997.JPG",
      "filesize": 3359239,
      "time": 1254483464,
      "type": "photo",
      "folder_id": 9175,
      "owner_user_id": 2,
      "additional": {
        "resolution": { "width": 3872, "height": 2592 },
        "orientation": 1,
        "thumbnail": {
          "cache_key": "448643_1254476266",
          "unit_id": 448643,
          "sm": "ready", "m": "ready", "xl": "ready", "preview": "broken"
        }
      }
    }]
  }
}
```

---

## Flow 2: Password-Protected Browse

### Step 1 — Load landing page
```
GET /photo/mo/sharing/{passphrase}
```

Parse `window.SYNO`. `FotoSharing.enable_password: true` → show the password form. No `sharing_sid` cookie is set here.

### Step 2 — Authenticate
```
POST /photo/mo/sharing/webapi/entry.cgi/SYNO.Core.Sharing.Login

api=SYNO.Core.Sharing.Login&method=login&version=1
  &sharing_id=YXiEpO1Xy
  &password=gh78ut
```

> **Note:** `sharing_id` and `password` are plain values — not JSON-quoted. (Same quirk as FileStation.)

Respons on success / correct password:
```json
{
  "data": { "sharing_sid": "61XFf4kxM6fdwCj50whyl9n2rnzaU1CW" },
  "success": true
}
```
`Set-Cookie: sharing_sid=...` is also included in successful response, identical to the value in the body.


Respons on error / wrong password:
```
{
    "error": {
        "code": 1001,
        "errors": "Execute Error: wrong protect passwd"
    },
    "success": false
}
```

success: true/false is the primary signal; error code could be used to customize the error message for the frontend (everything beyond 1001 would be unknown error).

Store the `sharing_sid` and include it as a cookie on all subsequent requests. 


### Step 3 — Get album info & list photos
Same as Flow 1 Steps 2–3, now with the `sharing_sid` cookie set.

---

## Flow 3: Invite-Only (not supported)

The landing page `GET /photo/mo/sharing/{passphrase}` responds with an HTTP redirect to:
```
{nas}:5001/?launchApp=SYNO.Foto.Sharing.AppInstance&passphrase={id}&photos_action=login
```

This requires a full DSM account login and is **not supported**. Detect by following the redirect and checking the destination URL, or by treating any non-200 / missing `window.SYNO` response as unsupported.

---

## Flow 4: Upload Request

URL pattern: `/photo/mo/request/{passphrase}` (note: `/request/` not `/sharing/`).  
API base: `POST /photo/mo/request/webapi/entry.cgi/{API_NAME}`.

Upload requests are **always public** — no password or invite-only protection.

### Step 1 — Load landing page
```
GET /photo/mo/request/{passphrase}
```

The HTML body contains `PhotoRequestPage.init` (a JavaScript call), which is the signal that this is an upload request share rather than a browse share. `window.SYNO` is present but `FotoSharing` only contains `passphrase` — no `enable_password` or `privacy_type`.

### Step 2 — Get request info
```
POST /photo/mo/request/webapi/entry.cgi/SYNO.Foto.Sharing.Passphrase

api=SYNO.Foto.Sharing.Passphrase&method=get_photo_request_info&version=1
  &passphrase="P4MBAbRsi"
```

Response:
```json
{
  "data": {
    "subject": "Photo Request on 2026-07-14",
    "description": "",
    "filesize_limit": 0
  },
  "success": true
}
```

`filesize_limit: 0` means no limit.

### Step 3 — Upload photo
```
POST /photo/mo/request/webapi/entry.cgi/SYNO.Foto.Upload.PhotoRequestItem
  ?api=SYNO.Foto.Upload.PhotoRequestItem&method=upload&version=1

Content-Type: multipart/form-data
X-Syno-Sharing: P4MBAbRsi
```

Multipart fields:

| Field | Value |
|---|---|
| `api` | `SYNO.Foto.Upload.PhotoRequestItem` |
| `method` | `upload` |
| `version` | `1` |
| `passphrase` | `"P4MBAbRsi"` (JSON-quoted) |
| `guest_name` | `"TestUser"` (JSON-quoted) |
| `name` | `"upload_test.png"` (JSON-quoted filename) |
| `file` | binary image data |
| `thumb_xl` | client-generated JPEG thumbnail (large) |
| `thumb_sm` | client-generated JPEG thumbnail (small) |
| `thumb_m` | client-generated JPEG thumbnail (medium) |

The client generates the three thumbnails locally before uploading.
Whether the server generates them if omitted is not confirmed, but assumed - the fields are likely optional.

Response:
```json
{
  "data": { "action": "new", "id": 476433, "unit_id": 448653 },
  "success": true
}
```

---

## Thumbnail URL

```
GET /photo/synofoto/api/v2/p/Thumbnail/get
  ?id={unit_id}
  &cache_key="{cache_key}"
  &type="unit"
  &size="{size}"
  &passphrase="{passphrase}"
  &_sharing_id="{passphrase}"
```

- `size`: `"sm"` | `"m"` | `"xl"`
- No session cookie required — passphrase params are sufficient
- Returns: `image/jpeg`

Example:
```
GET /photo/synofoto/api/v2/p/Thumbnail/get?id=448643&cache_key="448643_1254476266"&type="unit"&size="sm"&passphrase="LESyyu3kf"&_sharing_id="LESyyu3kf"
```

Parameters (e.g., cache_key, etc) are supplied by data from SYNO.Foto.Browse.Item endpoint.
The xl thumbnail is used by the original frontend for the fullscreen image viewer.

Exemplary sizes (varies depending on image aspect ration):
- XL: 1912 x 1280
- M: 478 x 320
- SM: 359 x 240

---

## Download URL (Per Item)

Downloads one or more specific photos by item ID. Single item → original file. Multiple items → ZIP containing just those files.

```
POST /photo/mo/sharing/webapi/entry.cgi

api=SYNO.Foto.Download&method=download&version=2
  &force_download=true
  &item_id=[476428,476429]
  &passphrase="{passphrase}"
  &download_type=source
  &_sharing_id="{passphrase}"
```

- `item_id` — JSON array of integer photo IDs from `Browse.Item.list`
- `download_type=source` → original file(s); `download_type=convert` → compressed JPEG
- `force_download=true` — sets `Content-Disposition: attachment`
- Single item: returns the original image file directly
- Multiple items: returns `application/zip`
- Auth: `X-Syno-Sharing: {passphrase}` header; `sharing_sid` cookie required for password-protected albums

---

## Download URL (Full Album)

Downloads the full album as a ZIP. The `.zip` suffix in the URL is a browser filename hint.

```
POST /photo/mo/sharing/webapi/entry.cgi/{album_name}.zip

passphrase="{passphrase}"
  &download_type={type}
  &api=SYNO.Foto.Browse.Album
  &method=download
  &version=2
  &_sharing_id="{passphrase}"
```

- `download_type=source` → original files
- `download_type=convert` → compressed JPEG
- No `X-Syno-Sharing` header; auth is via the `sharing_sid` cookie and/or `_SSID` session
- Returns: `application/zip` with `Content-Disposition: attachment; filename="{album_name}.zip"`

---

## Key Differences from FileStation `/sharing/` API

| | FileStation `/sharing/` | Photos `/photo/mo/sharing/` |
|---|---|---|
| URL namespace | `/sharing/webapi/entry.cgi/` | `/photo/mo/sharing/webapi/entry.cgi/` |
| API family | `SYNO.FolderSharing.*`, `SYNO.FileStation.*` | `SYNO.Foto.*` |
| Upload requests | same URL as browse | separate `/photo/mo/request/` path |
| Password auth | `SYNO.Core.Sharing.Login` | `SYNO.Core.Sharing.Login` (same) |
| Thumbnails | not applicable | separate endpoint at `/photo/synofoto/api/v2/p/` |
| Download | single file or folder as ZIP via `SYNO.FileStation.Download` | per-item (1–N photos) via `SYNO.Foto.Download`; full album ZIP via `SYNO.Foto.Browse.Album` |
| Invite-only detection | `SYNO.Core.Sharing.Session` `sharing_status` field | Redirect from landing page HTML |

---

## Minimum API Calls (per use case)

**Browse public album:**
1. `GET /photo/mo/sharing/{passphrase}` — parse `window.SYNO`; get `sharing_sid` cookie; detect invite-only (redirect) and `privacy_type`
2. `SYNO.Foto.Browse.Album.get` — get album name and item count
3. `SYNO.Foto.Browse.Item.list` — list photos (paginate with offset/limit)
4. `Thumbnail/get` — fetch thumbnails (direct GET with passphrase params, no cookie needed)

**Browse password-protected album:**
1. `GET /photo/mo/sharing/{passphrase}` — parse `window.SYNO`; detect `enable_password: true`
2. `SYNO.Core.Sharing.Login.login` — authenticate, get `sharing_sid` cookie
3. `SYNO.Foto.Browse.Album.get` — get album name and item count
4. `SYNO.Foto.Browse.Item.list` — list photos (cookie required)

**Download single photo (from viewer):**
- `SYNO.Foto.Download.download` — POST with `item_id=[{id}]`; streams original file

**Download selected photos:**
- `SYNO.Foto.Download.download` — POST with `item_id=[id1,id2,...]`; returns ZIP of selected files

**Download full album:**
- `SYNO.Foto.Browse.Album.download` — POST to `/{album_name}.zip`; streams all photos as ZIP

**Upload request:**
1. `GET /photo/mo/request/{passphrase}` - get `sharing_sid` cookie
2. `SYNO.Foto.Sharing.Passphrase.get_photo_request_info` — get subject/description
3. `SYNO.Foto.Upload.PhotoRequestItem.upload` — upload each file (with thumbnails)
