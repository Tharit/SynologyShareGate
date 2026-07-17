# synologysharegate

A secure, lightweight Go proxy that exposes Synology NAS file shares to the public internet without exposing DSM itself.

## Security Model

Synology's native sharing portal (`/sharing/`, `/webman/`) cannot be cleanly isolated from DSM's admin surface when exposed directly to the internet. This proxy solves that by:

- **Zero stored credentials.** Synology's sharing API uses sharing IDs as capability tokens. Knowing a valid sharing ID is sufficient to list and download files — no admin credentials, no session cookies. The proxy holds nothing.
- **Zero trust by design.** The proxy can only perform actions Synology has already explicitly permitted via a share link.
- **Minimal attack surface.** The only outbound destination is the configured Synology host. No other outbound calls are made. The container runs in a read-only filesystem as a non-root user.

## Supported Services

| URL prefix | Status |
|---|---|
| `/sharing/{id}` | ✅ Implemented (browse + upload) |
| `/api/sharing/*` | ✅ Implemented (list, download, upload JSON API) |
| `/photo/mo/sharing/{id}` | ✅ Implemented (thumbnail grid + fullscreen viewer) |
| `/photo/mo/request/{id}` | ✅ Implemented (photo/video upload requests) |
| `/api/photo/*` | ✅ Implemented (list, thumbnail, download, upload JSON API) |
| `/drive/d/s/{permanent_link}/{sharing_link}` | ✅ Implemented (browse, public or password-protected) |
| `/drive/d/f/{permanent_link}` | ✅ Detected and rejected (invite-only shares require a Synology account login; not supported) |
| `/drive/d/r/{file_request_id}/{sharing_link}` | ✅ Implemented (file upload requests) |
| `/api/drive/browse/*` | ✅ Implemented (unlock, list, download, download-zip JSON/streaming API) |
| `/api/drive/upload/*` | ✅ Implemented (unlock, init, file, notify JSON/streaming API) |

## Deployment

### Docker Compose (recommended)

```yaml
services:
  syno-proxy:
    image: syno-proxy:latest
    build: .
    read_only: true
    ports:
      - "8080:8080"
    environment:
      SYNO_HOST: "192.168.1.10:5001"
    restart: unless-stopped
```

```bash
docker compose up -d
```

### Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `SYNO_HOST` | ✅ | — | Synology hostname/IP and port, e.g. `192.168.1.10:5001` |
| `SYNO_HTTPS` | | `true` | Use HTTPS for Synology calls |
| `SYNO_SKIP_VERIFY` | | `false` | Skip TLS cert verification (warns loudly; for self-signed certs only) |
| `LISTEN_ADDR` | | `:8080` | Address and port the proxy listens on |
| `RATE_LIMIT_RPM` | | `60` | Requests per minute per IP (sliding window) |
| `MAX_TRACKED_IPS` | | `1000` | Max distinct client IPs held in the rate-limit window |
| `MAX_UPLOAD_BYTES` | | `104857600` | Maximum request body size for uploads (bytes; default 100 MB) |
| `MAX_CONCURRENT` | | `5` | Server-wide cap on concurrent in-flight requests (returns 503 when exceeded) |
| `LOG_LEVEL` | | `info` | `info` or `debug` (debug logs include request details) |
| `DEV_MODE` | | `false` | Disables HSTS and makes session cookies non-Secure (local development only) |
| `TRUSTED_PROXIES` | | `` | Comma-separated CIDRs to trust for `X-Forwarded-For`, e.g. `10.0.0.0/8` |

### Running Locally

```bash
SYNO_HOST=192.168.1.10:5001 go run .
# Visit http://localhost:8080/sharing/<your-sharing-id>
```

### TLS

TLS termination is expected to be handled by a reverse proxy in front of syno-proxy (Caddy, Nginx, Traefik, etc.). The proxy itself speaks plain HTTP on the configured `LISTEN_ADDR`.

## Adding a New Service Backend (Extension Pattern)

New service backends follow the same structure as the `sharing`, `photo`, and `drive` packages:

1. **Create a package** under the service name, e.g. `newsvc/`.
2. **Implement a `Handler`** struct with the proxy client injected via `NewHandler`.
3. **Write `syno.go`** for the service-specific Synology API calls, and a `context.go` for session/bootstrap parsing if the service needs it (following the `sharing`/`photo`/`drive` package pattern).
4. **Add templates** under `<service>/templates/` and embed them with `//go:embed`.
5. **Register routes** in `main.go` — page routes under `/<service>/...`, JSON API routes under `/api/<service>/`:
   ```go
   nh := newsvc.NewHandler(client, logger, cfg.MaxUploadBytes, cfg.DevMode)
   mux.Handle("GET /newsvc/{id}", wrap(nh.Browse))
   mux.Handle("GET /api/newsvc/list", wrap(nh.APIList))
   mux.HandleFunc("GET /api/newsvc/download", nh.APIDownload)  // no timeout — streams
   ```
   For services keyed by more than one identifier (e.g. Drive's `permanent_link`/`sharing_link` pairs), use multiple `{...}` wildcards in the route pattern — see `mux.Handle("GET /drive/d/s/{permanentLink}/{sharingLink}", ...)` in `main.go` for a real example.

Key invariants to maintain in any new backend:
- Never log sharing IDs, file paths, or user-supplied values at INFO level
- Never buffer file contents in memory — use `io.Copy` for downloads, `io.Pipe` for uploads
- All outbound calls go through `proxy.Client`; add no new HTTP clients
- Enforce permission checks (view-only, download-disabled, etc.) server-side before proxying to Synology — never rely on the frontend alone to hide an action, since a client can always call the API directly

## Architecture

```
Request
  └─ middleware chain (Logger → SecurityHeaders → RateLimit → GlobalConcurrency)
       └─ ServeMux
            ├─ GET  /sharing/{id}          → sharing.Handler.Browse   (sets sid cookie)
            ├─ GET  /api/sharing/list      → sharing.Handler.APIList   (reads sid cookie)
            ├─ GET  /api/sharing/download  → sharing.Handler.APIDownload (streams; no timeout)
            ├─ POST /api/sharing/upload    → sharing.Handler.APIUpload   (streams; no timeout)
            ├─ GET  /photo/mo/sharing/{id} → photo.Handler.BrowsePage    (sets sid cookie)
            ├─ GET  /photo/mo/request/{id} → photo.Handler.RequestPage  (sets sid cookie)
            ├─ GET  /api/photo/list        → photo.Handler.APIList        (reads sid cookie)
            ├─ GET  /api/photo/thumbnail   → photo.Handler.APIThumbnail   (no cookie needed)
            ├─ GET  /api/photo/download    → photo.Handler.APIDownload    (streams; no timeout)
            ├─ GET  /api/photo/download-album → photo.Handler.APIDownloadAlbum (streams; no timeout)
            ├─ POST /api/photo/upload      → photo.Handler.APIUpload      (streams; no timeout)
            ├─ GET  /drive/d/s/{permanentLink}/{sharingLink} → drive.Handler.BrowseByLink (sets sid cookie, path-scoped to /api/drive/browse/)
            ├─ GET  /drive/d/f/{permanentLink}               → drive.Handler.BrowseInviteOnly (detects + rejects; no session)
            ├─ GET  /drive/d/r/{fileRequestID}/{sharingLink} → drive.Handler.RequestPage (sets sid cookie, path-scoped to /api/drive/upload/)
            ├─ POST /api/drive/browse/unlock  → drive.Handler.APIUnlockBrowse
            ├─ GET  /api/drive/browse/list    → drive.Handler.APIList        (reads sid cookie)
            ├─ GET  /api/drive/browse/download    → drive.Handler.APIDownload    (streams; no timeout)
            ├─ GET  /api/drive/browse/download-zip → drive.Handler.APIDownloadZip (streams; no timeout)
            ├─ POST /api/drive/upload/unlock  → drive.Handler.APIUnlockRequest
            ├─ POST /api/drive/upload/init    → drive.Handler.APIUploadInit  (creates the per-uploader subfolder)
            ├─ POST /api/drive/upload/file    → drive.Handler.APIUploadFile  (streams; no timeout)
            └─ POST /api/drive/upload/notify  → drive.Handler.APIUploadNotify
                                                │
                                                ▼
                                          proxy.Client
                                                │
                                                ▼
                                       Synology NAS (internal network only)
```

`/sharing/{id}` fetches the sharing context from Synology and stores the resulting `sharing_sid` as a session cookie. Subsequent API calls (`/api/sharing/*`) read that cookie — no credentials are ever passed through the browser or stored on the proxy.

`/photo/mo/sharing/{id}` and `/photo/mo/request/{id}` mirror Synology's own two Photos URL namespaces (browse vs. upload request) and work the same way: the landing page establishes a `sharing_sid` session cookie, and `/api/photo/*` reads it on every call. Unlike FileStation shares, Photos API calls are scoped entirely by passphrase (not by folder path), so there's no path-traversal handling needed in this backend.

`/drive/d/s/{permanent_link}/{sharing_link}` and `/drive/d/r/{file_request_id}/{sharing_link}` mirror Synology's own Drive URL namespaces (browse vs. upload request); `/drive/d/f/{permanent_link}` (invite-only shares) is detected and rejected, since it requires a Synology account login. As with the other backends, the landing page establishes a session cookie and the corresponding `/api/drive/browse/*` / `/api/drive/upload/*` calls read it — kept as two separate cookies so a browse session and an upload-request session can coexist in the same browser.
