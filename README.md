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
| `/photo/*` | 🔜 Planned (v2) |
| `/drive/*` | 🔜 Planned (v2) |

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

New service backends follow the same structure as the `sharing` package:

1. **Create a package** under the service name, e.g. `photo/`.
2. **Implement a `Handler`** struct with the proxy client injected via `NewHandler`.
3. **Write `syno.go`** for the service-specific Synology API calls (following the `sharing/syno.go` pattern).
4. **Add templates** under `<service>/templates/` and embed them with `//go:embed`.
5. **Register routes** in `main.go` — page routes under `/<service>/{id}`, JSON API routes under `/api/<service>/`:
   ```go
   ph := photo.NewHandler(client, logger)
   mux.Handle("GET /photo/{id}", timeout(ph.Browse))
   mux.Handle("GET /api/photo/list", timeout(ph.APIList))
   mux.HandleFunc("GET /api/photo/download", ph.APIDownload)  // no timeout — streams
   ```
6. **Remove the stubs** from `photo/handler.go`.

Key invariants to maintain in any new backend:
- Never log sharing IDs, file paths, or user-supplied values at INFO level
- Never buffer file contents in memory — use `io.Copy` for downloads, `io.Pipe` for uploads
- All outbound calls go through `proxy.Client`; add no new HTTP clients

## Architecture

```
Request
  └─ middleware chain (Logger → SecurityHeaders → RateLimit → GlobalConcurrency)
       └─ ServeMux
            ├─ GET  /sharing/{id}          → sharing.Handler.Browse   (sets sid cookie)
            ├─ GET  /api/sharing/list      → sharing.Handler.APIList   (reads sid cookie)
            ├─ GET  /api/sharing/download  → sharing.Handler.APIDownload (streams; no timeout)
            ├─ POST /api/sharing/upload    → sharing.Handler.APIUpload   (streams; no timeout)
            ├─ GET  /photo/{id}            → 501 stub
            ├─ *    /api/photo/*           → 501 stub
            ├─ GET  /drive/{id}            → 501 stub
            └─ *    /api/drive/*           → 501 stub
                                                │
                                                ▼
                                          proxy.Client
                                                │
                                                ▼
                                       Synology NAS (internal network only)
```

`/sharing/{id}` fetches the sharing context from Synology and stores the resulting `sharing_sid` as a session cookie. Subsequent API calls (`/api/sharing/*`) read that cookie — no credentials are ever passed through the browser or stored on the proxy.
