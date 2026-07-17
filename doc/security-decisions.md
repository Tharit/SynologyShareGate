# Security Decisions

This file documents security findings that were reviewed and deliberately accepted,
along with the rationale. It exists so future reviewers understand these are known,
considered trade-offs — not oversights.

Findings describe the actual security-relevant mechanism (a header directive, a cookie's
provenance, a class of request) where that mechanism *is* the subject of the decision —
that detail is what a future reviewer needs to re-verify the finding still holds. They
deliberately avoid internal implementation trivia that can drift without changing the
finding itself (exact environment-variable names, current numeric defaults, function
names) — see `doc/spec.md` for the current architecture and `README.md` for current
configuration values.

---

## SD-1 — CSP allows `unsafe-inline` scripts

**Finding:** `Content-Security-Policy: script-src 'unsafe-inline'` weakens XSS protection
because injected inline scripts would execute.

**Decision:** Accepted.

**Rationale:** All data is escaped before being rendered into HTML or scripts, both
server-side and client-side. More importantly, there are no credentials to steal via
XSS: the session cookie is `HttpOnly` (JS cannot read it), and the only APIs exposed are
scoped to a public share link the attacker must already possess. The XSS risk surface
does not justify the refactor required to eliminate `unsafe-inline`.

---

## SD-2 — Rate limiter tracks a bounded number of IPs, with no eviction of active ones

**Finding:** The rate limiter's per-IP tracking table has a fixed maximum size. Once
full, requests from any new IP are rejected until stale entries age out on the next
periodic cleanup. An attacker with enough distinct IPs could fill the table and deny
service to legitimate users in the meantime.

**Decision:** Accepted. The table size is configurable; the default is sized for
expected traffic.

**Rationale:** Evicting the least-recently-seen IP to make room (LRU) is not a
meaningful improvement — it would remove that IP's rate-limit history, letting an
attacker cycling through IPs bypass rate limiting entirely for evicted addresses. The
correct mitigation for a table-exhaustion attack at this scale is upstream (firewall,
reverse-proxy connection limits), not in application code.

---

## SD-3 — Global concurrency limit can be saturated by slow downloads

**Finding:** The global concurrency limit (a small, fixed pool of request slots) is
shared across all endpoints. A handful of parallel long-running downloads can hold every
slot and block other users, including those making fast API calls.

**Decision:** Accepted.

**Rationale:** Expected traffic is low. Separating streaming endpoints onto a separate
limit adds complexity that is not warranted at this scale.

---

## SD-4 — Proxy does not terminate TLS natively

**Finding:** The proxy speaks plain HTTP and relies on an upstream TLS-terminating
reverse proxy. If that layer is absent or misconfigured, credentials and file contents
travel in cleartext.

**Decision:** Accepted. TLS termination at a reverse proxy is a required deployment
constraint.

**Rationale:** Adding native TLS support (certificate loading/renewal) adds complexity
and certificate management burden the proxy shouldn't own. The deployment topology
(reverse proxy in front) is standard and documented. A development-mode flag exists for
local HTTP testing, without weakening production defaults.

---

## SD-5 — Synology `Content-Type` forwarded without a MIME allowlist

**Finding:** The download handler forwards whatever `Content-Type` header Synology
sends. A compromised NAS could instruct the proxy to serve `text/html` with script
content.

**Decision:** Accepted.

**Rationale:** The proxy's threat model is outside→NAS: protecting the NAS from external
actors. It does not defend against a compromised NAS. The NAS is on an internal network
and is not reachable from the internet. A compromised NAS is already a full breach of the
environment this proxy protects. `Content-Disposition: attachment` and
`X-Content-Type-Options: nosniff` remain in place as defence-in-depth against
browser-side mishandling.

---

## SD-6 — HSTS `includeSubDomains` retained

**Finding:** `Strict-Transport-Security: max-age=63072000; includeSubDomains` pins HTTPS
for the proxy's hostname and all of its subdomains for two years.

**Decision:** Accepted.

**Rationale:** `includeSubDomains` applies only to subdomains of the host that sent the
header — not to sibling subdomains on the parent domain. A proxy deployed at
`share.example.com` pins `*.share.example.com`, not `other.example.com`. In practice this
proxy has no subdomains of its own, so the directive is a no-op beyond the host itself.
Removing it would weaken HSTS coverage for no benefit.

---

## SD-7 — IPv4-mapped IPv6 addresses counted separately in rate limiter

**Finding:** If an upstream proxy or dual-stack network produces both `::ffff:1.2.3.4` and
`1.2.3.4` in `X-Forwarded-For` headers, the rate limiter tracks them as distinct IPs and
gives each its own bucket, effectively doubling the rate allowance for that client.

**Decision:** Accepted.

**Rationale:** Exploiting this requires a misconfigured upstream proxy that produces
inconsistent IP representations — not an attacker-controlled condition. Even if triggered,
the worst case is one client getting 2× the configured rate limit, which is bounded and not
meaningfully exploitable. The correct mitigation is consistent proxy configuration upstream.
Adding IP-normalisation logic to the rate limiter is not warranted.

---

## SD-8 — Synology's own session token is used directly as the browser session cookie

**Finding:** Every backend's session cookie value is the session token Synology itself
issued for that share (FileStation/Photos: `sharing_sid`; Drive: a per-share
`sharing_token`), forwarded as-is. Anyone who captures the cookie could make the
equivalent Synology API calls directly, bypassing the proxy's own controls entirely.

**Decision:** Accepted, for all backends.

**Rationale:** The NAS is not internet-reachable; capturing the cookie requires being on
the internal network. More importantly, in every backend this token is not a secret
independent of the share link — it is derived from (and, for password-protected shares,
gated by the same password as) the share's own identifier, which is already embedded in
the URL and visible in browser history, server access logs, and referrer headers
everywhere the cookie could plausibly be intercepted. An attacker who can observe the
cookie can equally observe the share link and obtain an equivalent token directly from
Synology without involving the proxy at all. A proxy-managed session layer (an opaque
token mapped internally to the real one) would add significant complexity for no
meaningful security gain, and would need to be re-justified independently for every
backend rather than reasoned about once.
