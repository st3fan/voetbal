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
	if entry["level"] != "INFO" {
		t.Errorf("level = %v, want INFO (all HTTP logging is INFO, even 4xx)", entry["level"])
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

func TestLoggingTransport(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("hello"))
	}))
	defer srv.Close()

	var buf bytes.Buffer
	client := &http.Client{Transport: loggingTransport{
		base:   http.DefaultTransport,
		logger: slog.New(slog.NewJSONHandler(&buf, nil)),
	}}
	resp, err := client.Get(srv.URL + "/path?x=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("expected one JSON log line, got %q: %v", buf.String(), err)
	}
	if entry["msg"] != "upstream request" || entry["level"] != "INFO" {
		t.Errorf("msg/level = %v/%v", entry["msg"], entry["level"])
	}
	if entry["method"] != "GET" || entry["url"] != srv.URL+"/path?x=1" {
		t.Errorf("method/url = %v/%v", entry["method"], entry["url"])
	}
	if status, _ := entry["status"].(float64); int(status) != http.StatusOK {
		t.Errorf("status = %v", entry["status"])
	}
	if _, ok := entry["duration_ms"]; !ok {
		t.Errorf("duration_ms missing: %v", entry)
	}
}

func TestLoggingTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := srv.URL
	srv.Close()

	var buf bytes.Buffer
	client := &http.Client{Transport: loggingTransport{
		base:   http.DefaultTransport,
		logger: slog.New(slog.NewJSONHandler(&buf, nil)),
	}}
	if _, err := client.Get(dead); err == nil {
		t.Fatal("expected connection error")
	}

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("expected one JSON log line, got %q: %v", buf.String(), err)
	}
	if entry["msg"] != "upstream request failed" || entry["level"] != "INFO" {
		t.Errorf("msg/level = %v/%v", entry["msg"], entry["level"])
	}
	if errText, _ := entry["error"].(string); errText == "" {
		t.Errorf("error attribute missing: %v", entry)
	}
}
