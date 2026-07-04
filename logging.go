package main

import (
	"log/slog"
	"net/http"
	"net/netip"
	"os"

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
		DefaultLevel:     slog.LevelInfo,
		ClientErrorLevel: slog.LevelWarn,
		ServerErrorLevel: slog.LevelError,

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
	return newRequestLogger(slog.New(slog.NewJSONHandler(os.Stderr, nil)))(h)
}
