package main

import (
	"bytes"
	"compress/gzip"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseRegionLock(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"CA,US", []string{"CA", "US"}},
		{" ca , us ", []string{"CA", "US"}},
		{"CA,,US", []string{"CA", "US"}},
		{"", nil},
	}
	for _, tt := range tests {
		got := parseRegionLock(tt.in)
		if len(got) != len(tt.want) {
			t.Errorf("parseRegionLock(%q) = %v, want %v", tt.in, got, tt.want)
			continue
		}
		for _, code := range tt.want {
			if !got[code] {
				t.Errorf("parseRegionLock(%q) missing %q", tt.in, code)
			}
		}
	}
}

func TestDataPath(t *testing.T) {
	t.Setenv("VOETBAL_DATA_PATH", "")
	if got := dataPath(); got != "/data" {
		t.Errorf("got %q, want /data", got)
	}
	t.Setenv("VOETBAL_DATA_PATH", "/var/lib/voetbal")
	if got := dataPath(); got != "/var/lib/voetbal" {
		t.Errorf("got %q, want /var/lib/voetbal", got)
	}
}

func TestGeoDBURL(t *testing.T) {
	ts := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	want := "https://download.db-ip.com/free/dbip-country-lite-2026-07.mmdb.gz"
	if got := geoDBURL(ts); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func gzipBody(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDownloadGeoDB(t *testing.T) {
	body := gzipBody(t, []byte("mmdb-bytes"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), geoDBName)
	if err := downloadGeoDB(srv.URL, dest); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "mmdb-bytes" {
		t.Errorf("got %q, want mmdb-bytes", content)
	}
}

func TestDownloadGeoDBError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), geoDBName)
	if err := downloadGeoDB(srv.URL, dest); err == nil {
		t.Fatal("expected error")
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dest should not exist, stat err = %v", err)
	}
}

func TestEnsureGeoDBFresh(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write(gzipBody(t, []byte("new")))
	}))
	defer srv.Close()
	urlFor := func(time.Time) string { return srv.URL }

	dir := t.TempDir()
	path := filepath.Join(dir, geoDBName)
	if err := os.WriteFile(path, []byte("existing"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ensureGeoDB(dir, urlFor)
	if err != nil || got != path {
		t.Fatalf("got (%q, %v), want (%q, nil)", got, err, path)
	}
	if hits != 0 {
		t.Errorf("fresh file triggered %d downloads", hits)
	}
}

func TestEnsureGeoDBStale(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(gzipBody(t, []byte("new")))
	}))
	defer srv.Close()
	urlFor := func(time.Time) string { return srv.URL }

	dir := t.TempDir()
	path := filepath.Join(dir, geoDBName)
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}

	if _, err := ensureGeoDB(dir, urlFor); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(path)
	if string(content) != "new" {
		t.Errorf("got %q, want new", content)
	}
}

func TestEnsureGeoDBStaleRefreshFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(http.NotFound))
	defer srv.Close()
	urlFor := func(time.Time) string { return srv.URL }

	dir := t.TempDir()
	path := filepath.Join(dir, geoDBName)
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}

	got, err := ensureGeoDB(dir, urlFor)
	if err != nil || got != path {
		t.Fatalf("got (%q, %v), want stale path kept", got, err)
	}
}

func TestEnsureGeoDBMissingDownloadFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(http.NotFound))
	defer srv.Close()
	urlFor := func(time.Time) string { return srv.URL }

	if _, err := ensureGeoDB(t.TempDir(), urlFor); err == nil {
		t.Fatal("expected error")
	}
}

type stubLookup struct {
	code  string
	err   error
	calls int
}

func (s *stubLookup) Country(netip.Addr) (string, error) {
	s.calls++
	return s.code, s.err
}

func requestFrom(remoteAddr string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remoteAddr
	return r
}

func TestRegionLockAllow(t *testing.T) {
	allowed := parseRegionLock("CA,US")
	tests := []struct {
		name   string
		remote string
		stub   *stubLookup
		want   bool
		calls  int
	}{
		{"loopback", "127.0.0.1:1234", &stubLookup{code: "DE"}, true, 0},
		{"private", "192.168.1.10:1234", &stubLookup{code: "DE"}, true, 0},
		{"allowed country", "8.8.8.8:1234", &stubLookup{code: "US"}, true, 1},
		{"denied country", "185.93.175.0:1234", &stubLookup{code: "DE"}, false, 1},
		{"lookup error", "8.8.8.8:1234", &stubLookup{err: errors.New("boom")}, false, 1},
		{"unknown country", "8.8.8.8:1234", &stubLookup{}, false, 1},
		{"bad remote addr", "garbage", &stubLookup{code: "US"}, false, 0},
	}
	for _, tt := range tests {
		l := &regionLock{allowed: allowed, lookup: tt.stub}
		if got := l.allow(requestFrom(tt.remote)); got != tt.want {
			t.Errorf("%s: allow = %v, want %v", tt.name, got, tt.want)
		}
		if tt.stub.calls != tt.calls {
			t.Errorf("%s: %d lookups, want %d", tt.name, tt.stub.calls, tt.calls)
		}
	}
}

func TestRegionLockMiddleware(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	l := &regionLock{allowed: parseRegionLock("CA,US"), lookup: &stubLookup{code: "DE"}}
	handler := lockMiddleware([]accessLock{l}, next)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, requestFrom("8.8.8.8:1234"))
	if w.Code != http.StatusForbidden {
		t.Errorf("got %d, want 403", w.Code)
	}

	w = httptest.NewRecorder()
	handler.ServeHTTP(w, requestFrom("127.0.0.1:1234"))
	if w.Code != http.StatusOK {
		t.Errorf("got %d, want 200", w.Code)
	}
}
