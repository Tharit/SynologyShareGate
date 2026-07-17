# Specification

This document specifies what SynologyShareGate is, the invariants it must never violate,
and the architecture and behavior needed to (re)build it. It is written for coding
agents: dense, imperative, and organized around decisions that matter rather than
exhaustive detail. Wire-level Synology API detail (exact request/response parameters,
JSON-quoting rules, response-parsing mechanics) is intentionally **not** duplicated here
— see `doc/api/*.md` for that.
Accepted security trade-offs with rationale live in `doc/security-decisions.md`; do not
re-litigate them without reading that file first.

## 1. Purpose

A small, single-binary fully stateless Go proxy that exposes specific, narrow features
of a Synology NAS's public sharing portal to the public internet, without exposing DSM
or `webman` itself.

**Services in scope.** One independent backend per Synology sharing application, each
under its own URL prefix and each following the same contract (§5):

| Service | Synology feature | URL prefix |
|---|---|---|
| `sharing` | FileStation shares | `/sharing/...` |
| `photo` | Photos shares | `/photo/...` |
| `drive` | Drive shares | `/drive/...` |

Supporting an additional Synology sharing application later means adding a fourth
backend to this list, not changing how the existing ones work.

**Why this exists:** DSM's own sharing portal cannot be cleanly isolated from DSM's admin
surface when placed directly on the internet. This proxy re-implements just the sharing
UX as an independent app that only ever calls the narrow set of Synology APIs a public
share link already grants access to.

## 2. Non-Goals

- Not a general-purpose file server or NAS admin tool.
- Not multi-tenant and not a DSM account/authentication proxy — shares that require a
  DSM user login (`sharing_status: user`, Drive `errCode 1002`, Photos invite-only
  redirect) are detected and explicitly rejected with a clear error, never followed.
- No in-browser previewing or editing
- No thumbnail generation beyond what Synology backend does automatically
- No server-side persistence of any kind — no database, no session store, no on-disk
  cache. Every request either forwards to Synology or is served from data embedded in
  the current request/response.
- No config file, no runtime reconfiguration. All configuration is environment
  variables read once at startup, covering: the target NAS host, the listen address,
  resource limits (rate limit, concurrency cap, upload size cap), log verbosity, and a
  development-mode flag that relaxes cookie/HSTS requirements for local HTTP testing
  without weakening production defaults. Exact variable names are a reference-level
  detail (README), not a spec concern.

## 3. Security Invariants

These are non-negotiable. A change that violates one of these needs a documented,
deliberate exception in `doc/security-decisions.md`, not a silent workaround.

1. **Zero stored credentials.** The proxy never authenticates to Synology with an
   account. It only ever forwards the capability a public share link already carries
   (a sharing ID / passphrase / permanent link, optionally + password). If the proxy
   process is compromised, the blast radius is "whatever the public share links already
   allowed" — nothing more.
2. **Stateless.** No server-side caching, no session table. "Session" = the token Synology
   issued (e.g. `sharing_sid`, Drive's `sharing_token`), held only in an `HttpOnly`,
   `SameSite=Strict` cookie scoped to the relevant `/api/<service>/...` path prefix, and
   forwarded to Synology on each call. Restarting the process invalidates nothing.
3. **Zero trust of the client.** Never trust a client-supplied value for anything that
   gates access. Every permission/capability check that matters (download-allowed,
   password-gate, path containment) must be re-verified against Synology (or against
   data fetched from Synology in the same request) before the action is performed —
   hiding a button in the UI is UX, not enforcement. See §5's "capability re-check"
   requirement.
4. **Minimal outbound surface.** The only outbound network destination for the whole
   process is the single configured NAS host, called only through one shared HTTP
   client. No other HTTP clients, no telemetry, no third-party CDN scripts/fonts
   (the CSP is `default-src 'self'`; everything must be self-hosted or inlined).
5. **No secrets or PII in logs.** Never log sharing IDs, passwords, tokens, cookie
   values, file paths, or user-supplied strings (uploader names, filenames) at INFO
   level. DEBUG may log error context but never raw secrets.
6. **Never buffer whole files in memory.** Downloads stream via `io.Copy` directly from
   the Synology response to the client response. Uploads stream via `io.Pipe` from the
   client request into the outbound multipart body. A file exceeding the configured
   upload size cap must be rejected via `http.MaxBytesReader`, not read fully and then
   checked.
7. **Bounded resource usage under attack.** Per-IP sliding-window rate limiting, a
   global concurrency cap (reject with 503 past the cap, never queue unboundedly), and a
   hard body-size cap on uploads are all required, not optional hardening.
8. **Defense-in-depth response headers on every response:** strict CSP, `X-Frame-Options:
   DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy`, a locked-down
   `Permissions-Policy`, `Cache-Control: no-store`, and HSTS in production (disabled only
   in development mode, for local HTTP testing).
9. **TLS termination is external.** The proxy speaks plain HTTP on its configured listen
   address and assumes a generic reverse proxy in front handles TLS. Do not add native
   TLS support to "simplify" deployment — that trade-off has already been made (see
   `doc/security-decisions.md`, SD-4).
10. **Sanitize before it becomes a header or a shell for another API call.** Any
    user-supplied string that ends up in `Content-Disposition`, an HTML attribute, or a
    Synology API parameter must be stripped of quotes/backslashes (parameter injection)
    and bidirectional/invisible Unicode control characters (display spoofing) first.

## 4. Architecture

- **Single Go binary**, standard library only for HTTP (`net/http`'s `ServeMux` with
  Go 1.22+ method+wildcard route patterns) — no web framework, no ORM, no external router, 
  generally no external dependencies.
- **One shared `proxy.Client`** (`proxy/proxy.go`) wrapping a single `http.Client`
  pointed at the configured NAS host. Every backend must funnel every Synology call
  through this one client. It also defines the two shared error types (`HTTPError`,
  `SynoError`) other packages wrap with `errors.As`.
- **One Go package per exposed service** (`sharing/`, `photo/`, `drive/`), each fully
  self-contained:
  - `handler.go` — the HTTP layer only: route handlers, request validation and input
    sanitization, template rendering, JSON response shaping, and mapping errors to
    user-facing text. Owns a `Handler` struct — holding the shared `proxy.Client`, a logger, and
    the relevant configuration (upload size cap, dev-mode flag)
  - `context.go` (where the service needs session/bootstrap handling) — fetches the
    landing page, parses whatever bespoke non-JSON format Synology serves it in (JS
    object literal / executable JS / inline arrow-function assignments), extracts the
    session token and share metadata, and implements password authentication.
  - `syno.go` — the actual per-feature Synology API calls (list, download, upload,
    archive, notify, ...). Each function takes already-resolved inputs (token, ids) and
    returns parsed data or a streaming `*http.Response` for the handler to copy through.
  - `templates/*.html` — embedded via `//go:embed`, one shell per page type (browse,
    upload). Rendered server-side once with everything already known at that point
    (name, folder-vs-file, permissions); all further interaction happens through that
    service's own `/api/<service>/*` endpoints via plain JS — no build step, no
    framework, no bundler.
  - **Small duplication across packages is intentional and preferred** over a shared
    "common" package. For example, RFC 5987 filename encoding,
    bidi/invisible-character stripping, and the `errorPage{Title, Detail}` pattern are
    each re-implemented per package. Do not factor these into a shared internal package
    just to remove duplication — each service package must stay independently readable
    and independently modifiable without touching the others.
- **Middleware chain** (outermost → innermost, applied once to every request): request
  logging → security headers → rate limiting → a global concurrency cap → the router.
- **Two response-handling regimes**, chosen per route at registration time in `main.go`:
  - Non-streaming routes (page renders, JSON list/unlock/init/notify calls) are wrapped
    in `http.TimeoutHandler` with a fixed deadline.
  - Streaming routes (download, download-zip/folder/album, upload) are registered
    **without** a timeout wrapper, because they can legitimately run for a long time.
    This is deliberate — do not "fix" a missing timeout on a streaming route without
    understanding this split; instead bound it via context deadlines on the *upstream*
    calls that can hang (e.g. an archive-job poll loop), not the whole HTTP response.
- **Configuration is 100% environment variables**, parsed and validated once at startup;
  invalid configuration fails fast (the process exits immediately) rather than falling
  back silently to an insecure default for a required value.
- **Structured JSON logs to stdout only**, one line per HTTP request plus explicit
  `Info`/`Debug`/`Warn`/`Error` calls in handlers. No external log sinks.

## 5. Backend Contract (generic — applies to every service)

Every `/<service>/...` backend must implement this shape. §6 lists what actually differs
per existing service.

**URL namespace.** Two independent flows, each with its own URL prefix:
- **Browse**: `GET /<service>/{...ids}` — view/download existing content.
- **Upload request**: a second URL prefix — a drop-box for uploading files, no browsing
  of existing content. Some services encode both under one ID shape (`sharing`,
  `photo`), others need multiple path segments (Drive's `permanent_link`/`sharing_link`
  or `file_request_id`/`sharing_link` pairs) — use multiple `{...}` wildcards in the
  route pattern for those.
- An unsupported share type (requires a DSM account) must render a clear "Unsupported
  Share" error page immediately, with no attempt to follow a login flow.

**Session.** The landing page fetch establishes a session token from Synology. That
token becomes an `HttpOnly`, `SameSite=Strict` cookie named consistently within the
package (commonly `sid`), scoped via `Path` to that service's own `/api/<service>/...`
prefix (or a more specific sub-path, e.g. per browse-vs-upload) so unrelated services or
flows in the same browser can't collide. A locked (password-protected) share must
**never** get a valid session cookie before successful authentication — pre-auth,
render the password prompt using only whatever metadata Synology exposes without a
session (title/description of an upload request, whether it's an upload share at all),
and keep the real content areas hidden.

**Password flow.** Wrong password → inline error message next to the field, field stays
focused/selected, no navigation. Correct password → the frontend transitions in place
using a small JSON payload from the unlock endpoint (name, folder-vs-file, permission
flags) rather than reloading the page, *except* where the service genuinely cannot know
enough without a fresh server render (in which case a client-side reload of the same URL
is acceptable and must be commented as such).

**Browse UI.** Two mutually exclusive layouts, chosen by whether the shared item is a
folder or a single file — never render a one-row table for a single-file share:
- **Single item:** a centered card — icon, name, (size/modified date if cheaply known
  without an extra round trip), and a Download button. No breadcrumb, no table, no
  network call beyond what already produced this page.
- **Folder:** breadcrumb navigation + a file table (name / size / modified columns),
  a "download entire share" action, and, where the service supports multi-select, a
  checkbox per row plus a selection bar (count, "download selected", "clear") that
  appears once ≥1 item is selected. Exactly one selected non-folder item downloads
  directly; anything else (a folder, or >1 item) goes through the archive/ZIP path.
- If the share's permission tier disables downloads entirely, show a persistent,
  non-alarming notice banner explaining that up front (not just a missing button —
  a missing button reads as a bug), and don't render download links/checkboxes/buttons
  that would only ever fail.

**Download semantics.** A single file streams directly with a forced
`Content-Disposition: attachment` and a sanitized filename (ASCII fallback +
RFC 5987 `filename*=` for non-ASCII). Multi-item/folder downloads produce a ZIP —
either streamed directly or, if the underlying API requires it, through an async
job (start → poll → download). Before proxying **any** download, the handler must
independently re-verify the share still permits it (§3.3) — never rely on a value
computed earlier in the session or supplied by the client, and never let a
permission-denied download fall through to Synology only to forward a confusing
upstream error; reject locally with a clear status instead.

**Upload UI.** Drop zone (click or drag-and-drop) + required uploader-name field + a
per-file list showing pending/uploading/done/error status + sequential (not parallel)
upload of the batch + a "done" banner once all succeed. If the service's upload
protocol involves multiple discrete API calls (e.g. create a destination, then upload,
then notify the owner), the frontend must issue them in the correct order and any
one-time setup step (like creating a per-uploader destination) must happen exactly once
per batch, not once per file.

**Error handling.** Landing-page-level failures render a full error page
(`errorPage{Title, Detail}`-shaped data passed to the same template used for the happy
path) mapping known Synology error codes to short, specific, user-facing text, with a
generic fallback for anything unrecognized — never show a raw Synology error code or
internal message to the user. JSON API errors are always
`{"success": false, "error": "<text>"}` (uploads also carry `"retryable": bool` so the
frontend knows whether to let the user retry that item) with an appropriate HTTP status;
never leak stack traces or raw upstream bodies.

**Minimal calls.** Every browse/upload page must render everything already knowable
(name, folder-vs-file, permissions, upload-request title/description) directly into the
server-rendered HTML/JS at first load. Only call `/api/<service>/*` for things that are
genuinely dynamic: listing a specific folder, streaming a download, authenticating a
password, or performing a mutation (upload). Do not add a round trip for data that was
already available at render time.

## 6. Existing Backends — Deltas

The three implemented backends share everything in §5; this table is what actually
differs between them. Read `doc/api/<service>.md` for the exact wire format behind each
row.

| Aspect | `sharing` (FileStation) | `photo` (Photos) | `drive` (Drive) |
|---|---|---|---|
| Item identity | filesystem path | passphrase + numeric item id | opaque `id:{file_id}` node scheme |
| Permission granularity | share-wide; download assumed allowed | share-wide `privacy_type` (view-only vs download) | per-node `capabilities` (read/preview/download/write) |
| Folder / multi-item download | direct streaming ZIP | direct streaming ZIP (whole album) | async job: dry-run → start → poll → download |
| Multi-select UI | not implemented (share is one file or one folder tree) | yes | yes (can include folders in the selection) |
| Upload protocol | single multipart POST | single multipart POST | 2-step "slice upload" (empty reservation, then data) + separate destination-create + owner-notify calls |
| Session cookie scope | one `sid`, path `/api/sharing/` | one `sid`, path `/api/photo/` | two `sid` cookies, paths `/api/drive/browse/` and `/api/drive/upload/` (browse and upload-request sessions must coexist) |
| Rejected share type | DSM-account share (`sharing_status: user`) | invite-only (HTTP redirect to DSM login) | invite-only (`errCode 1002`, no redirect — must be detected without following one) |

## 7. Frontend Conventions

- Vanilla HTML/CSS/JS per template, no build step, no framework, no external font/script
  CDNs (must satisfy `default-src 'self'`).
- Consistent visual language across all services: same header (small icon + title), same
  accent color and card/table styling, same password-prompt component, same
  drop-zone/upload-list component, same breadcrumb+table component, same selection-bar
  component. A new backend should look and behave like the existing ones, not invent a
  new visual style.
- Every value interpolated into an inline `<script>` block from Go's `html/template`
  must go through the `| js` pipeline (e.g. `{{.ShareName | js}}`) — never interpolate a
  server value into a `<script>` block unescaped.
- Every user-supplied or Synology-supplied display string must be HTML-escaped
  client-side before insertion into the DOM.

## 8. Adding a New Backend

1. New package under the service name; a `Handler` struct plus a `NewHandler`
   constructor taking the shared client, a logger, and the relevant configuration
   (§4).
2. `context.go` for session/bootstrap parsing if needed, `syno.go` for the Synology API
   calls, `templates/*.html` embedded with `//go:embed` — follow §4's package shape.
3. Register routes in `main.go`: page routes under `/<service>/...` (wrapped with the
   shared timeout handler), JSON routes under `/api/<service>/...` (also wrapped),
   streaming routes under `/api/<service>/...` registered **without** the timeout
   wrapper (§4).
4. Implement the full §5 contract before calling it done: both flows if applicable,
   password gating, single-item vs folder layouts, permission re-verification before
   any download, minimal-calls page loads.
5. Add unit tests for any hand-rolled parsing logic (bootstrap/session format parsing)
   using real fixtures captured from the actual NAS response, not synthetic minimal
   examples — this is the highest-risk, least-typed-checked code in each backend.
6. Update `doc/api/<service>.md` (wire format) and the README's service table and
   architecture diagram.

## 9. Non-Functional Expectations

- **Efficient:** minimal Synology round trips per user action (§5); no full-file
  buffering (§3.6); background async work (e.g. archive-job polling) bounded by its own
  context deadline, not left to run indefinitely.
- **Resilient:** a slow or failing Synology call must fail the single request it's part
  of, not the process; no unbounded goroutines, no unclosed `io.Pipe`/response bodies
  (always `defer resp.Body.Close()`, always `pw.CloseWithError(...)` in the upload
  goroutine).
- **Observable enough, not more:** one structured log line per request plus targeted
  `Debug`/`Warn`/`Error` calls at failure points — no per-field verbose tracing, no logs
  that could leak the invariants in §3.5.

## 10. Verification Workflow

Before considering a change to any backend done:
1. `go build ./... && go vet ./... && go test ./...` must be clean.
2. Run the app locally against a real (or safely reachable) NAS with development mode
   enabled and exercise the actual change end-to-end (page load, password flow if
   touched, download, upload) — via a browser (Playwright) or `curl`, not just unit
   tests. Treat "the code compiles and unit tests pass" as necessary, not sufficient,
   for anything touching an HTTP flow.
3. Confirm both the happy path and the relevant negative path (wrong password, download
   disabled, invalid id, oversized upload) produce the specific behavior §5 requires —
   not just "doesn't crash."
