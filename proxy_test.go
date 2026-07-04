package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestHostAllowed(t *testing.T) {
	tests := []struct {
		host string
		want bool
	}{
		{"live.cdn.nos.nl", true},
		{"LIVE.CDN.NOS.NL", true},
		{"resolver.streaming.api.nos.nl", true},
		{"prod.npoplayer.nl", true},
		{"x.streamgate.io", true},
		{"nos.nl", false},
		{"evil.com", false},
		{"cdn.nos.nl.evil.com", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := hostAllowed(tt.host); got != tt.want {
			t.Errorf("hostAllowed(%q) = %v, want %v", tt.host, got, tt.want)
		}
	}
}

func TestIsPlaylist(t *testing.T) {
	m3u8URL, _ := url.Parse("https://x/live/index.M3U8?token=1")
	segURL, _ := url.Parse("https://x/live/seg1.ts")
	tests := []struct {
		contentType string
		u           *url.URL
		want        bool
	}{
		{"application/vnd.apple.mpegurl", segURL, true},
		{"Application/X-MPEGURL; charset=utf-8", segURL, true},
		{"video/mp2t", m3u8URL, true},
		{"video/mp2t", segURL, false},
		{"", segURL, false},
	}
	for _, tt := range tests {
		if got := isPlaylist(tt.contentType, tt.u); got != tt.want {
			t.Errorf("isPlaylist(%q, %s) = %v, want %v", tt.contentType, tt.u, got, tt.want)
		}
	}
}

func TestRewritePlaylist(t *testing.T) {
	base, _ := url.Parse("https://cdn.example.com/live/master.m3u8")
	in := `#EXTM3U
#EXT-X-KEY:METHOD=AES-128,URI="keys/key.bin"
#EXT-X-STREAM-INF:BANDWIDTH=1

variant.m3u8
https://abs.example.com/seg.ts`
	out := rewritePlaylist(in, base)
	lines := strings.Split(out, "\n")
	if lines[0] != "#EXTM3U" {
		t.Errorf("comment line changed: %q", lines[0])
	}
	wantKey := `#EXT-X-KEY:METHOD=AES-128,URI="/proxy?url=` + url.QueryEscape("https://cdn.example.com/live/keys/key.bin") + `"`
	if lines[1] != wantKey {
		t.Errorf("got %q, want %q", lines[1], wantKey)
	}
	if lines[3] != "" {
		t.Errorf("blank line not preserved: %q", lines[3])
	}
	if want := "/proxy?url=" + url.QueryEscape("https://cdn.example.com/live/variant.m3u8"); lines[4] != want {
		t.Errorf("got %q, want %q", lines[4], want)
	}
	if want := "/proxy?url=" + url.QueryEscape("https://abs.example.com/seg.ts"); lines[5] != want {
		t.Errorf("got %q, want %q", lines[5], want)
	}
}

func TestStreamProxyPath(t *testing.T) {
	tests := []struct {
		id, resolution, want string
	}{
		{"2616266", "1920x1080", "/proxy/nos/2616266/1920x1080"},
		{"2616266", "", "/proxy/nos/2616266"},
		{"weird/id", "960x540", "/proxy/nos/weird%2Fid/960x540"},
	}
	for _, tt := range tests {
		if got := streamProxyPath(tt.id, tt.resolution); got != tt.want {
			t.Errorf("streamProxyPath(%q, %q) = %q, want %q", tt.id, tt.resolution, got, tt.want)
		}
	}
}

func TestFindStream(t *testing.T) {
	streams := []Stream{{ID: "111", Title: "A"}, {ID: "222", Title: "B"}}
	if s, ok := findStream(streams, "222"); !ok || s.Title != "B" {
		t.Errorf("findStream(222) = %+v, %v; want B, true", s, ok)
	}
	if _, ok := findStream(streams, "999"); ok {
		t.Errorf("findStream(999) found a stream, want miss")
	}
	if _, ok := findStream(nil, ""); ok {
		t.Errorf("findStream on empty list found a stream, want miss")
	}
}

func TestVariantForResolution(t *testing.T) {
	variants := []Variant{
		{Resolution: "1920x1080", Bandwidth: 5000, URL: "https://cdn/a-hi.m3u8"},
		{Resolution: "1920x1080", Bandwidth: 3000, URL: "https://cdn/a-lo.m3u8"},
		{Resolution: "960x540", Bandwidth: 1500, URL: "https://cdn/b.m3u8"},
	}
	if u, ok := variantForResolution(variants, "1920x1080"); !ok || u != "https://cdn/a-hi.m3u8" {
		t.Errorf("1920x1080: got %q, %v; want highest-bandwidth variant", u, ok)
	}
	if u, ok := variantForResolution(variants, "960x540"); !ok || u != "https://cdn/b.m3u8" {
		t.Errorf("960x540: got %q, %v", u, ok)
	}
	if _, ok := variantForResolution(variants, "640x360"); ok {
		t.Errorf("640x360: found a variant, want miss")
	}
}

func TestHandleProxyForbidden(t *testing.T) {
	for _, target := range []string{
		"https://evil.com/x.m3u8",
		"ftp://live.cdn.nos.nl/x.m3u8",
		"not a url",
		"",
	} {
		r := httptest.NewRequest(http.MethodGet, "/proxy?url="+url.QueryEscape(target), nil)
		w := httptest.NewRecorder()
		handleProxy(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("url=%q: got status %d, want 403", target, w.Code)
		}
	}
}
