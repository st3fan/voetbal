package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMaskClientIP(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"203.0.113.7", "203.0.0.0"},
		{"127.0.0.1", "127.0.0.0"},
		{"192.168.1.22", "192.168.0.0"},
		{"::1", "127.0.0.0"},
		{"::ffff:192.168.1.22", "192.168.0.0"},
		{"2001:db8:1:2::3", "2001:db8::"},
		{"not-an-ip", "unknown"},
		{"", "unknown"},
	}
	for _, tt := range tests {
		if got := maskClientIP(tt.in); got != tt.want {
			t.Errorf("maskClientIP(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestNewRequestLogger(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	handler := newRequestLogger(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte("secret response body"))
	}))

	req := httptest.NewRequest(http.MethodGet, "http://voetbal.example/playlist.m3u?x=1", strings.NewReader("secret request body"))
	req.Header.Set("Authorization", "secret-token")
	req.RemoteAddr = "203.0.113.7:4321"
	handler.ServeHTTP(httptest.NewRecorder(), req)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("expected one JSON log line, got %q: %v", buf.String(), err)
	}

	request, ok := entry["request"].(map[string]any)
	if !ok {
		t.Fatalf("no request group in log entry: %v", entry)
	}
	response, ok := entry["response"].(map[string]any)
	if !ok {
		t.Fatalf("no response group in log entry: %v", entry)
	}
	if request["method"] != "GET" {
		t.Errorf("request method = %v, want GET", request["method"])
	}
	if request["path"] != "/playlist.m3u" {
		t.Errorf("request path = %v, want /playlist.m3u", request["path"])
	}
	if status, _ := response["status"].(float64); int(status) != http.StatusTeapot {
		t.Errorf("response status = %v, want %d", response["status"], http.StatusTeapot)
	}

	if entry["client_ip"] != "203.0.0.0" {
		t.Errorf("client_ip = %v, want 203.0.0.0", entry["client_ip"])
	}

	for _, secret := range []string{"secret request body", "secret response body", "secret-token", "203.0.113.7"} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("log entry leaks %q: %s", secret, buf.String())
		}
	}
	for name, group := range map[string]map[string]any{"request": request, "response": response} {
		if _, found := group["body"]; found {
			t.Errorf("%s body logged: %v", name, group)
		}
		if _, found := group["header"]; found {
			t.Errorf("%s headers logged: %v", name, group)
		}
	}
}
