package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamAndPlayerPath(t *testing.T) {
	tests := []struct {
		id, resolution, wantStream, wantPlayer string
	}{
		{"2616266", "1920x1080", "/stream/nos/2616266/1920x1080", "/player/nos/2616266/1920x1080"},
		{"2616266", "", "/stream/nos/2616266", "/player/nos/2616266"},
		{"weird/id", "960x540", "/stream/nos/weird%2Fid/960x540", "/player/nos/weird%2Fid/960x540"},
	}
	for _, tt := range tests {
		if got := streamPath(tt.id, tt.resolution); got != tt.wantStream {
			t.Errorf("streamPath(%q, %q) = %q, want %q", tt.id, tt.resolution, got, tt.wantStream)
		}
		if got := playerPath(tt.id, tt.resolution); got != tt.wantPlayer {
			t.Errorf("playerPath(%q, %q) = %q, want %q", tt.id, tt.resolution, got, tt.wantPlayer)
		}
	}
}

// Mirrors the shape of the real NOS master playlist (Unified Streaming).
const testMaster = `#EXTM3U
#EXT-X-VERSION:4
## Created with Unified Streaming Platform

# AUDIO groups
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio-aacl-256",NAME="English",DEFAULT=YES,CHANNELS="2"

# variants
#EXT-X-STREAM-INF:BANDWIDTH=882000,RESOLUTION=640x360,AUDIO="audio-aacl-256"
event-audio=256000-video=499968.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=4380000,RESOLUTION=1920x1080,AUDIO="audio-aacl-256"
event-audio=256000-video=3499968.m3u8

# keyframes
#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=73000,RESOLUTION=640x360,URI="keyframes/event-video=499968.m3u8"
`

func TestRewriteMasterPlaylist(t *testing.T) {
	got := rewriteMasterPlaylist(testMaster, "2616266")

	if strings.Contains(got, "I-FRAME") {
		t.Errorf("trick-play lines not dropped:\n%s", got)
	}
	if strings.Contains(got, ".m3u8") {
		t.Errorf("upstream variant URI leaked:\n%s", got)
	}
	for _, want := range []string{
		"#EXTM3U",
		"#EXT-X-MEDIA:TYPE=AUDIO",
		"#EXT-X-STREAM-INF:BANDWIDTH=882000,RESOLUTION=640x360",
		"\n/stream/nos/2616266/640x360\n",
		"\n/stream/nos/2616266/1920x1080\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestRewriteMasterPlaylistDropsVariantsWithoutResolution(t *testing.T) {
	in := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=96000,CODECS=\"mp4a.40.2\"\naudio-only.m3u8\n"
	got := rewriteMasterPlaylist(in, "1")
	if strings.Contains(got, "audio-only") || strings.Contains(got, "STREAM-INF") {
		t.Errorf("resolution-less variant not dropped:\n%s", got)
	}
}

func TestRewriteMediaPlaylist(t *testing.T) {
	base, _ := url.Parse("https://cdn.example.com/jwt-token/live/event/variant.m3u8")
	in := `#EXTM3U
#EXT-X-TARGETDURATION:8
#EXT-X-MAP:URI="init.mp4"
#EXTINF:7.68, no desc
seg-1.ts
#EXTINF:7.68, no desc
sub/dir/seg-2.ts?token=abc
## Media sequence discontinuity
#EXT-X-GAP
#EXTINF:0, no desc
https://cdn.example.com/jwt-token/live/event/seg-3.ts
https://other.example.com/elsewhere/seg-4.ts`

	got := rewriteMediaPlaylist(in, base, "/proxy/nos/2616266/848x480/")
	lines := strings.Split(got, "\n")

	want := []string{
		"#EXTM3U",
		"#EXT-X-TARGETDURATION:8",
		`#EXT-X-MAP:URI="/proxy/nos/2616266/848x480/init.mp4"`,
		"#EXTINF:7.68, no desc",
		"/proxy/nos/2616266/848x480/seg-1.ts",
		"#EXTINF:7.68, no desc",
		"/proxy/nos/2616266/848x480/sub/dir/seg-2.ts?token=abc",
		"## Media sequence discontinuity",
		"#EXT-X-GAP",
		"#EXTINF:0, no desc",
		"/proxy/nos/2616266/848x480/seg-3.ts",
		proxyPath("https://other.example.com/elsewhere/seg-4.ts"),
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), got)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

// stubStreamEnv swaps the package caches for test instances whose variant
// record for stream 111 / 848x480 points at upstreamURL/variant.m3u8, and
// allowlists the httptest host.
func stubStreamEnv(t *testing.T, upstreamURL string) {
	t.Helper()
	oldCaches, oldMux, oldDisk, oldHosts := streamCaches, streamMux, diskCaches, proxyAllowedHosts
	t.Cleanup(func() {
		streamCaches, streamMux, diskCaches, proxyAllowedHosts = oldCaches, oldMux, oldDisk, oldHosts
	})
	mux := newMuxCache(64 << 20)
	streamMux = mux
	disk, err := newDiskCache(filepath.Join(t.TempDir(), "cache"), time.Hour, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	diskCaches = disk
	// Registered after t.TempDir so it runs first: background fetches (and
	// their disk write-through) must finish before the temp dir is removed
	// and the globals are restored.
	t.Cleanup(mux.wait)
	proxyAllowedHosts = append(slices.Clone(proxyAllowedHosts), "127.0.0.1")

	now := time.Now()
	c := &streamCache{entries: map[string]cacheEntry{}}
	c.entries[streamKey("111")] = cacheEntry{now, now.Add(cacheTTL), streamRecord{Title: "Test", StreamURL: upstreamURL + "/master.m3u8"}}
	c.entries[masterKey("111")] = cacheEntry{now, now.Add(cacheTTL), masterRecord{Text: "#EXTM3U", Variants: 1}}
	c.entries[variantKey("111", "848x480")] = cacheEntry{now, now.Add(cacheTTL), variantRecord{URL: upstreamURL + "/variant.m3u8", Bandwidth: 1465000}}
	streamCaches = c
}

func segmentRequest(file string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/proxy/nos/111/848x480/"+file, nil)
	r.SetPathValue("id", "111")
	r.SetPathValue("resolution", "848x480")
	r.SetPathValue("file", file)
	return r
}

func TestHandleSegmentConcurrentViewersShareOneFetch(t *testing.T) {
	var hits atomic.Int64
	gate := make(chan struct{})
	srv := slowUpstream(t, &hits, bytes.Repeat([]byte("a"), 1000), bytes.Repeat([]byte("b"), 1000), gate)
	defer srv.Close()
	stubStreamEnv(t, srv.URL)

	var wg sync.WaitGroup
	bodies := make([][]byte, 2)
	for i := range 2 {
		wg.Go(func() {
			rec := httptest.NewRecorder()
			handleSegment(rec, segmentRequest("seg-1.ts"))
			bodies[i] = rec.Body.Bytes()
		})
	}
	time.Sleep(20 * time.Millisecond) // let both viewers join the same entry
	close(gate)
	wg.Wait()

	if !bytes.Equal(bodies[0], bodies[1]) || len(bodies[0]) != 2000 {
		t.Errorf("bodies differ or wrong size: %d vs %d", len(bodies[0]), len(bodies[1]))
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hit %d times, want 1", hits.Load())
	}
}

func TestHandleSegmentServedFromDiskAfterMuxExpiry(t *testing.T) {
	var hits atomic.Int64
	srv := slowUpstream(t, &hits, []byte("segment-bytes"), nil, nil)
	defer srv.Close()
	stubStreamEnv(t, srv.URL)

	rec := httptest.NewRecorder()
	handleSegment(rec, segmentRequest("seg-1.ts"))
	if rec.Code != http.StatusOK || rec.Body.String() != "segment-bytes" {
		t.Fatalf("first request: %d %q", rec.Code, rec.Body.String())
	}

	// Wait for the write-through, then drop the memory tier to prove the
	// second request is served from disk without an upstream hit.
	key := "nos/111/848x480/seg-1.ts"
	for range 100 {
		if _, ok := diskCaches.open(key, time.Now()); ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	streamMux = newMuxCache(64 << 20)

	rec = httptest.NewRecorder()
	handleSegment(rec, segmentRequest("seg-1.ts"))
	if rec.Code != http.StatusOK || rec.Body.String() != "segment-bytes" {
		t.Fatalf("disk request: %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "video/MP2T" {
		t.Errorf("disk hit content type = %q, want video/MP2T", got)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hit %d times, want 1 (second request should come from disk)", hits.Load())
	}
}

func TestHandleSegmentRejectsForeignHost(t *testing.T) {
	srv := slowUpstream(t, new(atomic.Int64), []byte("x"), nil, nil)
	defer srv.Close()
	stubStreamEnv(t, srv.URL)
	proxyAllowedHosts = slices.DeleteFunc(slices.Clone(proxyAllowedHosts), func(h string) bool { return h == "127.0.0.1" })

	rec := httptest.NewRecorder()
	handleSegment(rec, segmentRequest("seg-1.ts"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("got %d, want 403 for non-allowlisted upstream", rec.Code)
	}
}

func TestHandleStreamMediaPlaylistViaMux(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", hlsMimetype)
		w.Write([]byte("#EXTM3U\n#EXTINF:7.68, no desc\nseg-1.ts\n"))
	}))
	defer srv.Close()
	stubStreamEnv(t, srv.URL)

	streamRequest := func() *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/stream/nos/111/848x480", nil)
		r.SetPathValue("id", "111")
		r.SetPathValue("resolution", "848x480")
		return r
	}

	rec := httptest.NewRecorder()
	handleStream(rec, streamRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/proxy/nos/111/848x480/seg-1.ts") {
		t.Errorf("playlist not rewritten:\n%s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handleStream(rec, streamRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("second poll: got %d", rec.Code)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hit %d times, want 1 (polls within 2s coalesce)", hits.Load())
	}
}

func TestHandleSegmentWarnsOnSlowDelivery(t *testing.T) {
	withThreshold(t, 10*time.Millisecond)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(40 * time.Millisecond) // delay the response headers -> slow TTFB
		w.Write([]byte("segment-bytes"))
	}))
	defer srv.Close()
	stubStreamEnv(t, srv.URL)

	buf := captureLogs(t)
	rec := httptest.NewRecorder()
	handleSegment(rec, segmentRequest("seg-1.ts"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}

	entry := findLog(t, buf, "slow segment delivery")
	if entry == nil {
		t.Fatalf("expected slow segment delivery warning, got: %s", buf.String())
	}
	if entry["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", entry["level"])
	}
	if entry["tier"] != "upstream" {
		t.Errorf("tier = %v, want upstream", entry["tier"])
	}
	if entry["file"] != "seg-1.ts" {
		t.Errorf("file = %v, want seg-1.ts", entry["file"])
	}
	if entry["client_ip"] == nil {
		t.Errorf("client_ip missing: %v", entry)
	}
}

func TestHandleSegmentNoWarnOnFastDelivery(t *testing.T) {
	withThreshold(t, time.Second)
	var hits atomic.Int64
	srv := slowUpstream(t, &hits, []byte("segment-bytes"), nil, nil)
	defer srv.Close()
	stubStreamEnv(t, srv.URL)

	buf := captureLogs(t)
	rec := httptest.NewRecorder()
	handleSegment(rec, segmentRequest("seg-1.ts"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if entry := findLog(t, buf, "slow segment delivery"); entry != nil {
		t.Errorf("fast delivery should not warn: %v", entry)
	}
}
