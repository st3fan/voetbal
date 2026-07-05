package main

import (
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	sloghttp "github.com/samber/slog-http"
)

// maskClientIP zeroes the host part of ip for logging: IPv4 keeps only its
// first two octets (192.168.1.7 -> 192.168.0.0). IPv6 loopback is reported
// as IPv4 loopback (127.0.0.0); other IPv6 keeps its first two groups
// (2001:db8:1:2::3 -> 2001:db8::).
func maskClientIP(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return "unknown"
	}
	addr = addr.Unmap()
	if addr.Is6() && addr.IsLoopback() {
		addr = netip.AddrFrom4([4]byte{127, 0, 0, 1})
	}
	if addr.Is4() {
		b := addr.As4()
		return netip.AddrFrom4([4]byte{b[0], b[1], 0, 0}).String()
	}
	b := addr.As16()
	var masked [16]byte
	copy(masked[:], b[:4])
	return netip.AddrFrom16(masked).String()
}

// newRequestLogger returns middleware that logs every request to logger,
// tagged with the anonymized client IP (first two octets only). Request and
// response bodies and headers are never logged.
func newRequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	logMiddleware := sloghttp.NewWithConfig(logger, sloghttp.Config{
		// Everything logs at INFO for now; level-based filtering may come
		// later.
		DefaultLevel:     slog.LevelInfo,
		ClientErrorLevel: slog.LevelInfo,
		ServerErrorLevel: slog.LevelInfo,

		WithRequestID:      true,
		WithRequestBody:    false,
		WithRequestHeader:  false,
		WithResponseBody:   false,
		WithResponseHeader: false,
	})
	return func(h http.Handler) http.Handler {
		return logMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sloghttp.AddCustomAttributes(r, slog.String("client_ip", maskClientIP(clientIP(r))))
			h.ServeHTTP(w, r)
		}))
	}
}

func requestLogger(h http.Handler) http.Handler {
	return newRequestLogger(slog.Default())(h)
}

// loggingTransport logs every outbound HTTP request as a structured entry:
// method, full URL, status, and time to response headers. Each redirect hop
// is logged individually. The logger is resolved at request time so the
// package-level httpClient picks up the JSON default set in main.
type loggingTransport struct {
	base   http.RoundTripper
	logger *slog.Logger // nil = slog.Default()
}

func (t loggingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	logger := t.logger
	if logger == nil {
		logger = slog.Default()
	}
	start := time.Now()
	resp, err := t.base.RoundTrip(req)
	duration := time.Since(start).Milliseconds()
	if err != nil {
		logger.Info("upstream request failed",
			"method", req.Method, "url", req.URL.String(),
			"duration_ms", duration, "error", err.Error())
		return resp, err
	}
	logger.Info("upstream request",
		"method", req.Method, "url", req.URL.String(),
		"status", resp.StatusCode, "duration_ms", duration,
		"length", resp.ContentLength)
	return resp, nil
}
