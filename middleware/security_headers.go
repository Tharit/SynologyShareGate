package middleware

import "net/http"

// SecurityHeaders sets strict security headers on every response.
// When devMode is false (production), HSTS is included.
func SecurityHeaders(devMode bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := w.Header()
			h.Set("Content-Security-Policy",
				"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; object-src 'none'; base-uri 'self'; form-action 'self'; connect-src 'self'")
			h.Set("X-Frame-Options", "DENY")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("Permissions-Policy",
				"geolocation=(), camera=(), microphone=(), usb=(), payment=(), interest-cohort=()")
			h.Set("Cache-Control", "no-store")
			if !devMode {
				h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
			}
			next.ServeHTTP(w, r)
		})
	}
}
