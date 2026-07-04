package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestShortURLRegister(t *testing.T) {
	reg := shortURLRegistry{byCode: map[string]string{}}
	u := "https://x.cdn.nos.nl/a.m3u8"

	code := reg.register(u)
	if len(code) != 3 {
		t.Errorf("code length: got %d (%q), want 3", len(code), code)
	}
	sum := sha256.Sum256([]byte(u))
	if want := hex.EncodeToString(sum[:])[61:]; code != want {
		t.Errorf("code: got %q, want last 3 hash chars %q", code, want)
	}
	if again := reg.register(u); again != code {
		t.Errorf("re-register: got %q, want %q", again, code)
	}
	if got, ok := reg.lookup(code); !ok || got != u {
		t.Errorf("lookup(%q): got %q, %v; want %q, true", code, got, ok, u)
	}
}

func TestShortURLRegisterCollision(t *testing.T) {
	u := "https://x.cdn.nos.nl/a.m3u8"
	code := (&shortURLRegistry{byCode: map[string]string{}}).register(u)

	reg := shortURLRegistry{byCode: map[string]string{code: "https://other.example/b.m3u8"}}
	longer := reg.register(u)
	if len(longer) != 4 || !strings.HasSuffix(longer, code) {
		t.Errorf("collision: got %q, want 4-char code ending in %q", longer, code)
	}
	if got, ok := reg.lookup(code); !ok || got != "https://other.example/b.m3u8" {
		t.Errorf("collision overwrote existing code: got %q, %v", got, ok)
	}
}

func TestCopyPath(t *testing.T) {
	defer func(old bool) { copyShortURLs = old }(copyShortURLs)
	u := "https://x.cdn.nos.nl/copypath.m3u8"

	copyShortURLs = false
	if got := copyPath(u); got != proxyPath(u) {
		t.Errorf("disabled: got %q, want %q", got, proxyPath(u))
	}

	copyShortURLs = true
	got := copyPath(u)
	code, ok := strings.CutPrefix(got, "/r/")
	if !ok {
		t.Fatalf("enabled: got %q, want /r/ prefix", got)
	}
	if target, ok := shortURLs.lookup(code); !ok || target != u {
		t.Errorf("lookup(%q): got %q, %v; want %q, true", code, target, ok, u)
	}
}

func TestHandleShortURL(t *testing.T) {
	defer func(old func()) { refreshShortURLs = old }(refreshShortURLs)
	refreshShortURLs = func() {}

	u := "https://x.cdn.nos.nl/handler.m3u8"
	code := shortURLs.register(u)

	req := httptest.NewRequest(http.MethodGet, "/r/"+code, nil)
	req.SetPathValue("code", code)
	w := httptest.NewRecorder()
	handleShortURL(w, req)
	if w.Code != http.StatusFound {
		t.Errorf("known code: got %d, want 302", w.Code)
	}
	if got := w.Header().Get("Location"); got != proxyPath(u) {
		t.Errorf("known code: Location %q, want %q", got, proxyPath(u))
	}

	upper := strings.ToUpper(code)
	req = httptest.NewRequest(http.MethodGet, "/r/"+upper, nil)
	req.SetPathValue("code", upper)
	w = httptest.NewRecorder()
	handleShortURL(w, req)
	if w.Code != http.StatusFound {
		t.Errorf("uppercase code: got %d, want 302", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/r/zzzz", nil)
	req.SetPathValue("code", "zzzz")
	w = httptest.NewRecorder()
	handleShortURL(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("unknown code: got %d, want 404", w.Code)
	}
}

func TestHandleShortURLRefreshesOnMiss(t *testing.T) {
	defer func(old func()) { refreshShortURLs = old }(refreshShortURLs)
	u := "https://x.cdn.nos.nl/refresh.m3u8"
	refreshShortURLs = func() { shortURLs.register(u) }

	sum := sha256.Sum256([]byte(u))
	code := hex.EncodeToString(sum[:])[61:]
	if _, ok := shortURLs.lookup(code); ok {
		t.Fatalf("code %q registered before test", code)
	}

	req := httptest.NewRequest(http.MethodGet, "/r/"+code, nil)
	req.SetPathValue("code", code)
	w := httptest.NewRecorder()
	handleShortURL(w, req)
	if w.Code != http.StatusFound {
		t.Errorf("refreshed code: got %d, want 302", w.Code)
	}
	if got := w.Header().Get("Location"); got != proxyPath(u) {
		t.Errorf("refreshed code: Location %q, want %q", got, proxyPath(u))
	}
}

func TestIndexCopyURLIsShortWhenEnabled(t *testing.T) {
	defer func(old bool) { copyShortURLs = old }(copyShortURLs)
	copyShortURLs = true

	stream := Stream{ID: "2616266", Title: "Test"}
	qualities := []Quality{{Label: "1080p", URL: "https://x.cdn.nos.nl/short.m3u8", Resolution: "1920x1080", Height: 1080}}

	w := httptest.NewRecorder()
	render(w, "index.html", indexData{
		BaseURL: "http://voetbal.example:8000",
		Cards: []card{{
			Title:     "Test",
			Qualities: qualityViews(stream, qualities),
		}},
	})

	body := w.Body.String()
	want := `data-url="http://voetbal.example:8000/r/`
	if !strings.Contains(body, want) {
		t.Errorf("copy button URL not shortened: want substring %q in body:\n%s", want, body)
	}
	if strings.Contains(body, "/proxy?") {
		t.Errorf("copy button still carries the long proxy URL")
	}
}
