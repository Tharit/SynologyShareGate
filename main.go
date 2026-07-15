package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/tharit/synologysharegate/drive"
	"github.com/tharit/synologysharegate/middleware"
	"github.com/tharit/synologysharegate/photo"
	"github.com/tharit/synologysharegate/proxy"
	"github.com/tharit/synologysharegate/sharing"
)

type config struct {
	SynoHost          string
	SynoHTTPS         bool
	SynoSkipVerify    bool
	ListenAddr        string
	RateLimitRPM      int
	MaxTrackedIPs     int
	MaxUploadBytes    int64
	MaxConcurrent     int
	LogLevel          string
	DevMode           bool
	TrustedProxies    []net.IPNet
}

func loadConfig() (*config, error) {
	host := os.Getenv("SYNO_HOST")
	if host == "" {
		return nil, fmt.Errorf("SYNO_HOST is required")
	}

	cfg := &config{
		SynoHost:       host,
		SynoHTTPS:      envBool("SYNO_HTTPS", true),
		SynoSkipVerify: envBool("SYNO_SKIP_VERIFY", false),
		ListenAddr:     envString("LISTEN_ADDR", ":8080"),
		RateLimitRPM:   envInt("RATE_LIMIT_RPM", 60),
		MaxTrackedIPs:  envInt("MAX_TRACKED_IPS", 1000),
		MaxUploadBytes: envInt64("MAX_UPLOAD_BYTES", 100<<20), // 100 MB
		MaxConcurrent:  envInt("MAX_CONCURRENT", 5),
		LogLevel:       envString("LOG_LEVEL", "info"),
		DevMode:        envBool("DEV_MODE", false),
	}

	if raw := os.Getenv("TRUSTED_PROXIES"); raw != "" {
		for _, cidrStr := range strings.Split(raw, ",") {
			cidrStr = strings.TrimSpace(cidrStr)
			if cidrStr == "" {
				continue
			}
			_, cidr, err := net.ParseCIDR(cidrStr)
			if err != nil {
				return nil, fmt.Errorf("invalid TRUSTED_PROXIES CIDR %q: %w", cidrStr, err)
			}
			cfg.TrustedProxies = append(cfg.TrustedProxies, *cidr)
		}
	}

	return cfg, nil
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	i, err := strconv.Atoi(v)
	if err != nil || i <= 0 {
		return def
	}
	return i
}

func envInt64(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	i, err := strconv.ParseInt(v, 10, 64)
	if err != nil || i <= 0 {
		return def
	}
	return i
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	logger := middleware.NewLogger(cfg.LogLevel)
	client := proxy.NewClient(cfg.SynoHost, cfg.SynoHTTPS, cfg.SynoSkipVerify, logger)

	mux := http.NewServeMux()

	sh := sharing.NewHandler(client, logger, cfg.MaxUploadBytes, cfg.DevMode)
	ph := photo.NewHandler(client, logger, cfg.MaxUploadBytes, cfg.DevMode)

	// handlerTimeout covers the full round-trip for non-streaming routes:
	// Synology context fetch (≤30 s) + API call (≤15 s) + response write.
	const handlerTimeout = 45 * time.Second
	wrap := func(h http.HandlerFunc) http.Handler {
		return http.TimeoutHandler(h, handlerTimeout, `{"error":"request timeout"}`)
	}

	// Non-streaming handlers get a response deadline via TimeoutHandler.
	mux.Handle("GET /sharing/{id}", wrap(sh.Browse))
	mux.Handle("GET /photo/mo/sharing/{id}", wrap(ph.BrowsePage))
	mux.Handle("GET /photo/mo/request/{id}", wrap(ph.RequestPage))
	mux.Handle("GET /drive/{id}", wrap(drive.HandlePage))
	mux.Handle("GET /api/sharing/list", wrap(sh.APIList))
	mux.Handle("POST /api/sharing/unlock", wrap(sh.APIUnlock))
	mux.Handle("GET /api/photo/list", wrap(ph.APIList))
	mux.Handle("POST /api/photo/unlock", wrap(ph.APIUnlock))
	mux.Handle("GET /api/photo/thumbnail", wrap(ph.APIThumbnail))
	mux.Handle("GET /api/drive/", wrap(drive.HandleAPI))

	// Streaming handlers must not have a response deadline.
	mux.HandleFunc("GET /api/sharing/download", sh.APIDownload)
	mux.HandleFunc("GET /api/sharing/download-folder", sh.APIDownloadFolder)
	mux.HandleFunc("POST /api/sharing/upload", sh.APIUpload)
	mux.HandleFunc("GET /api/photo/download", ph.APIDownload)
	mux.HandleFunc("GET /api/photo/download-album", ph.APIDownloadAlbum)
	mux.HandleFunc("POST /api/photo/upload", ph.APIUpload)

	// Middleware chain (outermost first):
	// Logger → SecurityHeaders → RateLimit → GlobalConcurrency → mux
	handler := middleware.Chain(mux,
		middleware.RequestLogger(logger, cfg.TrustedProxies),
		middleware.SecurityHeaders(cfg.DevMode),
		middleware.RateLimit(cfg.RateLimitRPM, cfg.MaxTrackedIPs, cfg.TrustedProxies, logger),
		middleware.GlobalConcurrency(cfg.MaxConcurrent, logger),
	)

	srv := &http.Server{
		Addr:    cfg.ListenAddr,
		Handler: handler,
		// ReadHeaderTimeout prevents slow-header attacks without limiting request body
		// streaming (uploads and download responses may run for an extended period).
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: download responses stream indefinitely; http.TimeoutHandler
		// enforces deadlines on all non-streaming routes.
		IdleTimeout: 120 * time.Second,
	}

	if cfg.DevMode {
		logger.Warn("DEV_MODE is enabled — session cookies are not Secure and HSTS is disabled; do not use in production")
	}
	if len(cfg.TrustedProxies) == 0 {
		logger.Warn("TRUSTED_PROXIES not set — rate limiting and access logs will use the direct connection IP, not the real client IP when behind a reverse proxy")
	}

	logger.Info("starting syno-proxy",
		middleware.F("addr", cfg.ListenAddr),
		middleware.F("syno_host", cfg.SynoHost),
		middleware.F("rate_limit_rpm", cfg.RateLimitRPM),
		middleware.F("max_tracked_ips", cfg.MaxTrackedIPs),
		middleware.F("max_upload_bytes", cfg.MaxUploadBytes),
		middleware.F("max_concurrent", cfg.MaxConcurrent),
	)

	// Graceful shutdown on SIGINT/SIGTERM.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("server error", middleware.F("err", err.Error()))
			os.Exit(1)
		}
	}()

	<-quit
	logger.Info("shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("shutdown error", middleware.F("err", err.Error()))
	}
}
