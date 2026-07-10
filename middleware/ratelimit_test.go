package middleware

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimit_AllowsUnderLimit(t *testing.T) {
	logger := NewLogger("info")
	handler := RateLimit(5, 1000, nil, logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "1.2.3.4:9000"
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: got %d, want 200", i+1, rr.Code)
		}
	}
}

func TestRateLimit_BlocksOverLimit(t *testing.T) {
	logger := NewLogger("info")
	handler := RateLimit(3, 1000, nil, logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 3; i++ {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "5.6.7.8:9000"
		httptest.NewRecorder()
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}

	// Fourth request should be rate-limited.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "5.6.7.8:9000"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Errorf("got %d, want 429", rr.Code)
	}
}

func TestRateLimit_IsolatesIPs(t *testing.T) {
	logger := NewLogger("info")
	handler := RateLimit(2, 1000, nil, logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Exhaust limit for IP A.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = "10.0.0.1:9000"
		handler.ServeHTTP(httptest.NewRecorder(), req)
	}

	// IP B should still pass.
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.2:9000"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("different IP should not be rate-limited, got %d", rr.Code)
	}
}

func TestRealIP_NoTrustedProxies(t *testing.T) {
	rl := &rateLimiter{}
	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "1.2.3.4:9000"
	req.Header.Set("X-Forwarded-For", "9.9.9.9")

	// Without trusted proxies, should use RemoteAddr.
	if got := rl.realIP(req); got != "1.2.3.4" {
		t.Errorf("realIP = %q, want %q", got, "1.2.3.4")
	}
}

func TestRealIP_TrustedProxy(t *testing.T) {
	_, cidr, _ := net.ParseCIDR("10.0.0.0/8")
	rl := &rateLimiter{trusted: []net.IPNet{*cidr}}

	req := httptest.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.1:9000" // trusted proxy
	req.Header.Set("X-Forwarded-For", "203.0.113.5, 10.0.0.1")

	// Should skip the trusted proxy and return the real client IP.
	if got := rl.realIP(req); got != "203.0.113.5" {
		t.Errorf("realIP = %q, want %q", got, "203.0.113.5")
	}
}
