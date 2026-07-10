package middleware

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type rateLimiter struct {
	mu             sync.Mutex
	timestamps     map[string][]int64 // IP → Unix nanosecond timestamps in current window
	limit          int
	maxTrackedIPs  int
	windowDuration time.Duration
	trusted        []net.IPNet
	logger         *Logger
}

// RateLimit returns a sliding-window rate-limit middleware.
// limit is max requests per window (60s).
// maxTrackedIPs is the maximum number of distinct client IPs tracked simultaneously.
// trusted is the list of proxy CIDRs whose X-Forwarded-For header is trusted.
func RateLimit(limit, maxTrackedIPs int, trusted []net.IPNet, logger *Logger) func(http.Handler) http.Handler {
	rl := &rateLimiter{
		timestamps:     make(map[string][]int64),
		limit:          limit,
		maxTrackedIPs:  maxTrackedIPs,
		windowDuration: 60 * time.Second,
		trusted:        trusted,
		logger:         logger,
	}

	// Background cleanup: remove stale IP entries every 5 minutes.
	// No shutdown wiring needed: once the server stops accepting requests, no new
	// allow() calls race against cleanup(). The brief mutex hold every 5 minutes
	// is harmless and the process exits cleanly after the drain window.
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			rl.cleanup()
		}
	}()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := rl.realIP(r)
			if !rl.allow(ip) {
				logger.Info("rate limit exceeded", F("remote_ip", ip))
				w.Header().Set("Retry-After", "60")
				http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func (rl *rateLimiter) allow(ip string) bool {
	now := time.Now().UnixNano()
	cutoff := now - int64(rl.windowDuration)

	rl.mu.Lock()
	defer rl.mu.Unlock()

	ts, known := rl.timestamps[ip]

	if !known {
		// Reject new IPs when the table is at capacity.
		if len(rl.timestamps) >= rl.maxTrackedIPs {
			return false
		}
	}

	// Drop timestamps outside the window.
	valid := ts[:0]
	for _, t := range ts {
		if t > cutoff {
			valid = append(valid, t)
		}
	}

	if len(valid) >= rl.limit {
		rl.timestamps[ip] = valid
		return false
	}

	rl.timestamps[ip] = append(valid, now)
	return true
}

func (rl *rateLimiter) cleanup() {
	cutoff := time.Now().Add(-rl.windowDuration).UnixNano()
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for ip, ts := range rl.timestamps {
		allOld := true
		for _, t := range ts {
			if t > cutoff {
				allOld = false
				break
			}
		}
		if allOld {
			delete(rl.timestamps, ip)
		}
	}
}

// RealIP extracts the real client IP from r, respecting trusted proxy CIDRs.
// If the connecting IP is trusted, it walks X-Forwarded-For right-to-left and
// returns the first untrusted IP. Otherwise it returns r.RemoteAddr directly.
func RealIP(r *http.Request, trusted []net.IPNet) string {
	remoteIP, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteIP = r.RemoteAddr
	}

	if len(trusted) == 0 || !isTrustedIP(remoteIP, trusted) {
		return remoteIP
	}

	// Walk X-Forwarded-For from right to left; take the first untrusted IP.
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return remoteIP
	}

	parts := strings.Split(xff, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		ip := strings.TrimSpace(parts[i])
		if !isTrustedIP(ip, trusted) {
			return ip
		}
	}
	return remoteIP
}

func isTrustedIP(ipStr string, trusted []net.IPNet) bool {
	ip := net.ParseIP(strings.TrimSpace(ipStr))
	if ip == nil {
		return false
	}
	for _, cidr := range trusted {
		if cidr.Contains(ip) {
			return true
		}
	}
	return false
}

func (rl *rateLimiter) realIP(r *http.Request) string {
	return RealIP(r, rl.trusted)
}

// GlobalConcurrency returns a middleware that caps the number of requests handled
// concurrently across the entire server. Requests beyond n are rejected immediately
// with 503 rather than queued, so memory usage stays bounded.
func GlobalConcurrency(n int, logger *Logger) func(http.Handler) http.Handler {
	sem := make(chan struct{}, n)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
				next.ServeHTTP(w, r)
			default:
				logger.Info("global concurrency limit reached")
				w.Header().Set("Retry-After", "5")
				http.Error(w, `{"error":"server busy"}`, http.StatusServiceUnavailable)
			}
		})
	}
}

// Chain applies middlewares in order: first middleware is outermost.
func Chain(h http.Handler, middlewares ...func(http.Handler) http.Handler) http.Handler {
	for i := len(middlewares) - 1; i >= 0; i-- {
		h = middlewares[i](h)
	}
	return h
}
