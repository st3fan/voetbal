package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestBaseURL(t *testing.T) {
	plain := httptest.NewRequest(http.MethodGet, "/", nil)
	if got := requestBaseURL(plain); got != "http://example.com" {
		t.Errorf("plain: got %q, want %q", got, "http://example.com")
	}

	withTLS := httptest.NewRequest(http.MethodGet, "/", nil)
	withTLS.TLS = &tls.ConnectionState{}
	if got := requestBaseURL(withTLS); got != "https://example.com" {
		t.Errorf("tls: got %q, want %q", got, "https://example.com")
	}

	forwarded := httptest.NewRequest(http.MethodGet, "/", nil)
	forwarded.Host = "voetbal.example:8443"
	forwarded.Header.Set("X-Forwarded-Proto", "https")
	if got := requestBaseURL(forwarded); got != "https://voetbal.example:8443" {
		t.Errorf("forwarded: got %q, want %q", got, "https://voetbal.example:8443")
	}
}

func TestIndexCopyURLIsProxied(t *testing.T) {
	w := httptest.NewRecorder()
	render(w, "index.html", indexData{
		BaseURL: "http://voetbal.example:8000",
		Cards: []card{{
			Title:     "Test",
			Qualities: []Quality{{Label: "1080p", URL: "https://x.cdn.nos.nl/a.m3u8"}},
		}},
	})

	body := w.Body.String()
	want := `data-url="http://voetbal.example:8000` + proxyPath("https://x.cdn.nos.nl/a.m3u8")
	want = strings.ReplaceAll(want, "&", "&amp;")
	if !strings.Contains(body, want) {
		t.Errorf("copy button URL not proxied: want substring %q in body:\n%s", want, body)
	}
	if strings.Contains(body, `data-url="https://x.cdn.nos.nl`) {
		t.Errorf("copy button still carries the direct stream URL")
	}
}

func TestSetupRequiredHandler(t *testing.T) {
	handler := setupRequiredHandler()

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Errorf("index: got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "VOETBAL_NETWORK_LOCK") {
		t.Errorf("index: setup note missing from body")
	}

	for _, path := range []string{"/play", "/proxy", "/watchers", "/nope"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: got %d, want 403", path, w.Code)
		}
	}
}
