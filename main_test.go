package main

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	defer func(old bool) { copyShortURLs = old }(copyShortURLs)
	copyShortURLs = false

	stream := Stream{ID: "2616266", Title: "Test"}
	qualities := []Quality{{Label: "1080p", URL: "https://x.cdn.nos.nl/a.m3u8", Resolution: "1920x1080", Height: 1080}}

	w := httptest.NewRecorder()
	render(w, "index.html", indexData{
		BaseURL: "http://voetbal.example:8000",
		Cards: []card{{
			Title:     "Test",
			Qualities: qualityViews(stream, qualities),
		}},
	})

	body := w.Body.String()
	want := `data-url="http://voetbal.example:8000/stream/nos/2616266/1920x1080"`
	if !strings.Contains(body, want) {
		t.Errorf("copy button URL not short: want substring %q in body:\n%s", want, body)
	}
	if wantPlay := `href="/player/nos/2616266/1920x1080"`; !strings.Contains(body, wantPlay) {
		t.Errorf("web play link not short: want substring %q in body:\n%s", wantPlay, body)
	}
	if strings.Contains(body, "x.cdn.nos.nl") {
		t.Errorf("page still carries the direct stream URL")
	}
	if strings.Contains(body, "?url=") || strings.Contains(body, "?src=") {
		t.Errorf("page still carries a long url= or src= link")
	}
}

func TestQualityViewsWithoutStreamID(t *testing.T) {
	defer func(old bool) { copyShortURLs = old }(copyShortURLs)
	copyShortURLs = false

	qualities := []Quality{{Label: "1080p", URL: "https://x.cdn.nos.nl/a.m3u8", Resolution: "1920x1080", Height: 1080}}
	views := qualityViews(Stream{Title: "No ID"}, qualities)
	if len(views) != 1 || views[0].Path != proxyPath("https://x.cdn.nos.nl/a.m3u8") {
		t.Errorf("expected long proxy path fallback, got %+v", views)
	}
	wantPlay := "/play?" + url.Values{"src": {"https://x.cdn.nos.nl/a.m3u8"}, "title": {"No ID"}}.Encode()
	if len(views) == 1 && views[0].PlayPath != wantPlay {
		t.Errorf("PlayPath = %q, want %q", views[0].PlayPath, wantPlay)
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
