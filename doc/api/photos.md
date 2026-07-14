# Synology Photos Sharing API

Photos sharing links are created by the Synology Photos app. They differ significantly from `/sharing/XXXX` (FileStation) links.

## URL Patterns

| Type | Browser URL | API Base |
|---|---|---|
| Browse / view | `/photo/mo/sharing/{passphrase}` | `POST /photo/mo/sharing/webapi/entry.cgi/{API_NAME}` |
| Upload request | `/photo/mo/request/{passphrase}` | `POST /photo/mo/request/webapi/entry.cgi/{API_NAME}` |
| Thumbnails | — | `GET /photo/synofoto/api/v2/p/Thumbnail/get` |
| Download | — | `POST /photo/mo/sharing/webapi/entry.cgi/{filename}.zip` |

## Common Request Headers (all entry.cgi API calls)

```
Content-Type: application/x-www-form-urlencoded
X-Syno-Sharing: {passphrase}
```

---

## Flow 1: Public Browse (no password)

### Step 1 — Get permissions
```
POST /photo/mo/sharing/webapi/entry.cgi/SYNO.Foto.Sharing.Passphrase

api=SYNO.Foto.Sharing.Passphrase&method=get_permission&version=1
  &passphrase="LESyyu3kf"
  &exclude_public=false
```

Response:
```json
{
  "data": {
    "permission": {
      "download": false,
      "manage": false,
      "own": false,
      "upload": false,
      "view": true
    },
    "user_id": -1,
    "username": ""
  },
  "success": true
}
```

- `download: true` → album was shared with download permission
- `error.code === 123` → invite-only link; requires DSM login; **not supported**

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
      "item_count": 3,
      "start_time": 1254483464,
      "end_time": 1254483560,
      "additional": {
        "sharing_info": {
          "enable_password": false,
          "privacy_type": "public-view",
          "passphrase": "LESyyu3kf",
          "owner": { "id": 2, "name": "martin" }
        },
        "thumbnail": {
          "cache_key": "448643_1254476266",
          "unit_id": 448643,
          "sm": "ready", "m": "ready", "xl": "ready"
        }
      }
    }]
  },
  "success": true
}
```

Key fields:
- `enable_password: true` → show password form; call Login before proceeding
- `privacy_type`: `"public-view"` | `"public-download"` (correlates with `permission.download`)

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

Steps 1–2 (get_permission + Browse.Album) work **without** auth and reveal `enable_password: true`. Then:

### Step 2.5 — Authenticate
```
POST /photo/mo/sharing/webapi/entry.cgi/SYNO.Core.Sharing.Login

api=SYNO.Core.Sharing.Login&method=login&version=1
  &sharing_id="YXiEpO1Xy"
  &password="gh78ut"
```

Response:
```json
{
  "data": { "sharing_sid": "61XFf4kxM6fdwCj50whyl9n2rnzaU1CW" },
  "success": true
}
```

The `sharing_sid` is set as a browser cookie. **All subsequent requests for this album must include the `sharing_sid` cookie** — Browse.Item returns `error.code 101` without it.

Note: this is the same API as FileStation password-protected shares (`SYNO.Core.Sharing.Login`).

---

## Flow 3: Invite-Only (not supported)

Calling `SYNO.Foto.Sharing.Passphrase.get_permission` returns:
```json
{ "error": { "code": 123 }, "success": false }
```

The web app redirects to `{nas}:5001/?launchApp=SYNO.Foto.Sharing.AppInstance&passphrase={id}&photos_action=login`. This requires full DSM login and is **not supported**.

---

## Flow 4: Upload Request

URL pattern: `/photo/mo/request/{passphrase}` (note: `/request/` not `/sharing/`).  
API base: `POST /photo/mo/request/webapi/entry.cgi/{API_NAME}`.

Upload requests are **always public** — no password or invite-only protection.

### Step 1 — Get request info
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

### Step 2 — Upload photo
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

The client generates the three thumbnails locally before uploading. Whether the server generates them if omitted is not confirmed.

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

---

## Download URL (Album)

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

> **Note:** Download requires a valid session. For public links, the `sharing_sid` cookie appears to be set automatically on page load (mechanism: likely `Set-Cookie` from the initial HTML response). For password-protected links it comes from `SYNO.Core.Sharing.Login`. A CLI implementation needs to capture the session cookie from the initial page request before calling the download endpoint.

---

## Key Differences from FileStation `/sharing/` API

| | FileStation `/sharing/` | Photos `/photo/mo/sharing/` |
|---|---|---|
| URL namespace | `/sharing/webapi/entry.cgi/` | `/photo/mo/sharing/webapi/entry.cgi/` |
| API family | `SYNO.FolderSharing.*`, `SYNO.FileStation.*` | `SYNO.Foto.*` |
| Upload requests | same URL as browse | separate `/photo/mo/request/` path |
| Password auth | `SYNO.Core.Sharing.Login` | `SYNO.Core.Sharing.Login` (same) |
| Thumbnails | not applicable | separate endpoint at `/photo/synofoto/api/v2/p/` |
| Download | single file or folder as ZIP via `SYNO.FileStation.Download` | album as ZIP via `SYNO.Foto.Browse.Album&method=download` |
| Invite-only detection | `SYNO.Core.Sharing.Session` `sharing_status` field | `SYNO.Foto.Sharing.Passphrase` error code 123 |

---

## Minimum API Calls (per use case)

**Browse public album:**
1. `SYNO.Foto.Sharing.Passphrase.get_permission` — check permissions & detect invite-only
2. `SYNO.Foto.Browse.Album.get` — get album name, item count, detect password gate
3. `SYNO.Foto.Browse.Item.list` — list photos (paginate with offset/limit)
4. Thumbnail/get — fetch thumbnails (no API call needed, direct GET with passphrase params)

**Browse password-protected album:**
1. `SYNO.Foto.Browse.Album.get` — detect `enable_password: true`
2. `SYNO.Core.Sharing.Login.login` — authenticate, get `sharing_sid` cookie
3. `SYNO.Foto.Browse.Item.list` — list photos (cookie required)

**Upload request:**
1. `SYNO.Foto.Sharing.Passphrase.get_photo_request_info` — get subject/description
2. `SYNO.Foto.Upload.PhotoRequestItem.upload` — upload each file (with thumbnails)
