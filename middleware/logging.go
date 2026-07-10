package middleware

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

type Level int

const (
	LevelInfo  Level = 0
	LevelDebug Level = 1
)

type Field struct {
	Key   string
	Value any
}

func F(key string, value any) Field {
	return Field{Key: key, Value: value}
}

type Logger struct {
	level Level
	out   io.Writer
}

func NewLogger(level string) *Logger {
	l := &Logger{out: os.Stdout, level: LevelInfo}
	if level == "debug" {
		l.level = LevelDebug
	}
	return l
}

func (l *Logger) log(level, msg string, fields []Field) {
	entry := make(map[string]any, len(fields)+3)
	entry["time"] = time.Now().UTC().Format(time.RFC3339)
	entry["level"] = level
	entry["msg"] = msg
	for _, f := range fields {
		entry[f.Key] = f.Value
	}
	b, _ := json.Marshal(entry)
	fmt.Fprintf(l.out, "%s\n", b)
}

func (l *Logger) Info(msg string, fields ...Field)  { l.log("info", msg, fields) }
func (l *Logger) Error(msg string, fields ...Field) { l.log("error", msg, fields) }
func (l *Logger) Warn(msg string, fields ...Field)  { l.log("warn", msg, fields) }

func (l *Logger) Debug(msg string, fields ...Field) {
	if l.level >= LevelDebug {
		l.log("debug", msg, fields)
	}
}

// statusRecorder wraps ResponseWriter to capture the status code.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// RequestLogger logs method, status, and duration at INFO level.
// trusted is the same CIDR list used by RateLimit so that the logged IP
// matches the rate-limited IP (the real client, not the reverse proxy).
func RequestLogger(logger *Logger, trusted []net.IPNet) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			logger.Info("request",
				F("method", r.Method),
				F("path", r.URL.Path),
				F("status", rec.status),
				F("duration_ms", time.Since(start).Milliseconds()),
				F("remote_ip", RealIP(r, trusted)),
			)
		})
	}
}
