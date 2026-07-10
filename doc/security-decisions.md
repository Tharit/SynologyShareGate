# Security Decisions

This file documents security findings that were reviewed and deliberately accepted,
along with the rationale. It exists so future reviewers understand these are known,
considered trade-offs — not oversights.

---

## SD-1 — CSP allows `unsafe-inline` scripts

**Finding:** `Content-Security-Policy: script-src 'unsafe-inline'` weakens XSS protection
because injected inline scripts would execute.

**Decision:** Accepted.

**Rationale:** All data is already escaped server-side (`html/template`) and client-side
(`escHTML()`). More importantly, there are no credentials to steal via XSS: the session
cookie is `HttpOnly` (JS cannot read it), and the only APIs exposed are scoped to a
public share link the attacker must already possess. The XSS risk surface does not
justify the refactor required to eliminate `unsafe-inline`.

---

## SD-2 — Rate limiter IP table uses a hard cap, not LRU eviction

**Finding:** Once `MAX_TRACKED_IPS` distinct IPs fill the rate-limiter table, all new IPs
are rejected until the 5-minute cleanup cycle runs. An attacker with enough IPs could
fill the table and deny access to legitimate users.

**Decision:** Accepted. `MAX_TRACKED_IPS` is configurable; default (1000) is appropriate
for expected traffic.

**Rationale:** LRU eviction is not a meaningful improvement — evicting the
least-recently-seen IP effectively removes its rate-limit entry, allowing an attacker
cycling through IPs to bypass rate limiting entirely for evicted addresses. The correct
mitigation for a table-exhaustion attack at this scale is upstream (firewall, reverse-proxy
connection limits), not in application code.

---

## SD-3 — Global concurrency limit can be saturated by slow downloads

**Finding:** The global concurrency semaphore (default: 5 slots) is shared across all
endpoints. Five parallel long-running downloads hold all slots and block other users,
including those making fast API calls.

**Decision:** Accepted.

**Rationale:** Expected traffic is low. Separating streaming endpoints onto a different
semaphore adds complexity that is not warranted at this scale.

---

## SD-4 — Proxy does not terminate TLS natively

**Finding:** The proxy speaks plain HTTP and relies on an upstream TLS-terminating reverse
proxy. If that layer is absent or misconfigured, credentials and file contents travel
in cleartext.

**Decision:** Accepted. TLS termination at a reverse proxy is a required deployment
constraint.

**Rationale:** Adding optional `TLS_CERT`/`TLS_KEY` support adds complexity and certificate
management burden. The deployment topology (reverse proxy in front) is standard and
documented. `DEV_MODE=true` is available for local HTTP testing without weakening
production defaults.

---

## SD-5 — Synology `Content-Type` forwarded without a MIME allowlist

**Finding:** The download handler forwards whatever `Content-Type` header Synology sends.
A compromised NAS could instruct the proxy to serve `text/html` with script content.

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

## SD-8 — Synology `sharing_sid` used directly as the browser session token

**Finding:** The `sid` session cookie value is Synology's internal `sharing_sid`. Anyone
who captures it could make Synology API calls directly, bypassing the proxy's controls.

**Decision:** Accepted.

**Rationale:** The NAS is not internet-reachable; capturing the cookie requires being on
the internal network. More importantly, the `sharing_sid` is not a secret independent of
the share link: it is derived from the sharing ID, which is embedded in the URL and visible
in browser history, server access logs, and referrer headers everywhere the cookie could
plausibly be intercepted. An attacker who can observe the cookie can observe the sharing ID
and obtain an equivalent SID directly from Synology without involving the proxy at all.
A proxy-managed session layer (opaque token → internal SID mapping) would add significant
complexity for no meaningful security gain.
