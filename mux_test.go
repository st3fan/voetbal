package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func slowUpstream(t *testing.T, hits *atomic.Int64, first, rest []byte, gate chan struct{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(first)
		w.(http.Flusher).Flush()
		if gate != nil {
			<-gate
		}
		w.Write(rest)
	}))
}

func TestMuxCoalescesConcurrent(t *testing.T) {
	var hits atomic.Int64
	gate := make(chan struct{})
	srv := slowUpstream(t, &hits, []byte("first"), []byte("rest"), gate)
	defer srv.Close()

	c := newMuxCache(64 << 20)
	e1 := c.get("k", srv.URL, time.Minute, nil)
	e2 := c.get("k", srv.URL, time.Minute, nil)
	if e1 != e2 {
		t.Fatal("concurrent gets returned different entries")
	}
	close(gate)
	body, err := e1.waitDone()
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "firstrest" {
		t.Errorf("got %q", body)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hit %d times, want 1", hits.Load())
	}
}

func TestMuxServesFromCacheWithinTTL(t *testing.T) {
	var hits atomic.Int64
	srv := slowUpstream(t, &hits, []byte("body"), nil, nil)
	defer srv.Close()

	c := newMuxCache(64 << 20)
	c.get("k", srv.URL, time.Minute, nil).waitDone()
	c.get("k", srv.URL, time.Minute, nil).waitDone()
	if hits.Load() != 1 {
		t.Errorf("upstream hit %d times, want 1", hits.Load())
	}
}

func TestMuxRefetchesAfterExpiry(t *testing.T) {
	var hits atomic.Int64
	srv := slowUpstream(t, &hits, []byte("body"), nil, nil)
	defer srv.Close()

	c := newMuxCache(64 << 20)
	e := c.get("k", srv.URL, time.Minute, nil)
	e.waitDone()
	e.created = time.Now().Add(-2 * time.Minute)
	e2 := c.get("k", srv.URL, time.Minute, nil)
	if e2 == e {
		t.Fatal("expired entry was reused")
	}
	e2.waitDone()
	if hits.Load() != 2 {
		t.Errorf("upstream hit %d times, want 2", hits.Load())
	}
}

func TestMuxPeek(t *testing.T) {
	var hits atomic.Int64
	srv := slowUpstream(t, &hits, []byte("body"), nil, nil)
	defer srv.Close()

	c := newMuxCache(64 << 20)
	if c.peek("k") != nil {
		t.Fatal("peek before get returned an entry")
	}
	if hits.Load() != 0 {
		t.Fatal("peek started a fetch")
	}
	e := c.get("k", srv.URL, time.Minute, nil)
	e.waitDone()
	if c.peek("k") != e {
		t.Errorf("peek after get missed")
	}
}

func TestMuxFollowerJoinsMidDownload(t *testing.T) {
	var hits atomic.Int64
	gate := make(chan struct{})
	srv := slowUpstream(t, &hits, bytes.Repeat([]byte("a"), 1000), bytes.Repeat([]byte("b"), 1000), gate)
	defer srv.Close()

	c := newMuxCache(64 << 20)
	leader := c.get("k", srv.URL, time.Minute, nil)
	if err := leader.waitHeader(); err != nil {
		t.Fatal(err)
	}
	for leader.size() == 0 {
		time.Sleep(time.Millisecond)
	}

	follower := c.get("k", srv.URL, time.Minute, nil)
	if follower != leader {
		t.Fatal("follower got a different entry")
	}
	var wg sync.WaitGroup
	var got []byte
	wg.Go(func() {
		rec := httptest.NewRecorder()
		follower.serve(rec)
		got = rec.Body.Bytes()
	})
	close(gate)
	wg.Wait()
	if len(got) != 2000 {
		t.Errorf("follower got %d bytes, want 2000", len(got))
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hit %d times, want 1", hits.Load())
	}
}

func TestMuxDisabledFetchesFreshEveryTime(t *testing.T) {
	var hits atomic.Int64
	srv := slowUpstream(t, &hits, []byte("body"), nil, nil)
	defer srv.Close()

	c := newMuxCache(64 << 20)
	c.disabled = true
	c.get("k", srv.URL, time.Minute, nil).waitDone()
	if c.peek("k") != nil {
		t.Fatal("peek on a disabled cache returned an entry")
	}
	c.get("k", srv.URL, time.Minute, nil).waitDone()
	if hits.Load() != 2 {
		t.Errorf("upstream hit %d times, want 2", hits.Load())
	}
	if total := c.totalBytes(); total != 0 {
		t.Errorf("disabled cache retains %d bytes", total)
	}
	if views := c.views(time.Now()); len(views) != 0 {
		t.Errorf("disabled cache has %d views", len(views))
	}
}

func TestMuxDisabledStillPersists(t *testing.T) {
	var hits atomic.Int64
	srv := slowUpstream(t, &hits, []byte("segment-bytes"), nil, nil)
	defer srv.Close()

	persisted := make(chan []byte, 1)
	c := newMuxCache(64 << 20)
	c.disabled = true
	c.get("k", srv.URL, time.Minute, func(data []byte) { persisted <- data }).waitDone()
	select {
	case data := <-persisted:
		if string(data) != "segment-bytes" {
			t.Errorf("persisted %q", data)
		}
	case <-time.After(time.Second):
		t.Fatal("persist callback not called on a disabled cache")
	}
}

func TestMuxErrorEvictedAndRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := newMuxCache(64 << 20)
	e := c.get("k", url, time.Minute, nil)
	if err := e.waitHeader(); err == nil {
		t.Fatal("expected fetch error")
	}
	if c.get("k", url, time.Minute, nil) == e {
		t.Fatal("errored entry was reused")
	}
}

func TestMuxStatusPassthrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := newMuxCache(64 << 20)
	e := c.get("k", srv.URL, time.Minute, nil)
	if err := e.waitHeader(); err != nil {
		t.Fatal(err)
	}
	if e.status != http.StatusNotFound {
		t.Errorf("got status %d, want 404", e.status)
	}
}

func TestMuxPersistOnSuccess(t *testing.T) {
	var hits atomic.Int64
	srv := slowUpstream(t, &hits, []byte("segment-bytes"), nil, nil)
	defer srv.Close()

	persisted := make(chan []byte, 1)
	c := newMuxCache(64 << 20)
	c.get("k", srv.URL, time.Minute, func(data []byte) { persisted <- data }).waitDone()
	select {
	case data := <-persisted:
		if string(data) != "segment-bytes" {
			t.Errorf("persisted %q", data)
		}
	case <-time.After(time.Second):
		t.Fatal("persist callback not called")
	}
}

func TestMuxNoPersistOnErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	persisted := make(chan []byte, 1)
	c := newMuxCache(64 << 20)
	c.get("k", srv.URL, time.Minute, func(data []byte) { persisted <- data }).waitDone()
	select {
	case <-persisted:
		t.Fatal("non-200 response was persisted")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestMuxEviction(t *testing.T) {
	var hits atomic.Int64
	srv := slowUpstream(t, &hits, bytes.Repeat([]byte("x"), 80), nil, nil)
	defer srv.Close()

	c := newMuxCache(100)
	c.get("one", srv.URL+"/one", time.Minute, nil).waitDone()
	c.get("two", srv.URL+"/two", time.Minute, nil).waitDone()
	c.get("three", srv.URL+"/three", time.Minute, nil).waitDone()

	c.mu.Lock()
	_, hasFirst := c.entries["one"]
	count := len(c.entries)
	c.mu.Unlock()
	if hasFirst {
		t.Error("oldest entry not evicted")
	}
	if count > 2 {
		t.Errorf("cache holds %d entries, want <= 2", count)
	}
}

func TestMuxViews(t *testing.T) {
	var hits atomic.Int64
	srv := slowUpstream(t, &hits, []byte("0123456789"), nil, nil)
	defer srv.Close()

	c := newMuxCache(64 << 20)
	c.get("nos/1/848x480/seg-1.ts", srv.URL, time.Minute, nil).waitDone()
	c.get("nos/1/848x480/playlist", srv.URL, time.Minute, nil).waitDone()

	views := c.views(time.Now())
	if len(views) != 2 {
		t.Fatalf("got %d views: %+v", len(views), views)
	}
	byKey := map[string]string{}
	for _, v := range views {
		byKey[v.Key] = v.Contents
	}
	if got := byKey["nos/1/848x480/seg-1.ts"]; got != "segment, 10 B" {
		t.Errorf("segment contents = %q", got)
	}
	if got := byKey["nos/1/848x480/playlist"]; got != "media playlist, 10 B" {
		t.Errorf("playlist contents = %q", got)
	}
	if total := c.totalBytes(); total != 20 {
		t.Errorf("totalBytes = %d, want 20", total)
	}
}

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		n    int64
		want string
	}{
		{512, "512 B"},
		{4 << 10, "4 KB"},
		{5 << 20, "5.0 MB"},
		{12 << 30, "12.0 GB"},
	}
	for _, tt := range tests {
		if got := humanBytes(tt.n); got != tt.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tt.n, got, tt.want)
		}
	}
}

func TestMuxStats(t *testing.T) {
	var hits atomic.Int64
	srv := slowUpstream(t, &hits, []byte("body"), nil, nil)
	defer srv.Close()

	c := newMuxCache(64 << 20)
	if h, m := c.stats(); h != 0 || m != 0 {
		t.Fatalf("fresh cache stats = %d/%d", h, m)
	}
	c.peek("k") // peek miss: not counted, the request outcome is decided later
	c.get("k", srv.URL, time.Minute, nil).waitDone()
	c.get("k", srv.URL, time.Minute, nil)
	c.peek("k")
	h, m := c.stats()
	if h != 2 || m != 1 {
		t.Errorf("stats = %d hits, %d misses; want 2, 1", h, m)
	}
}

// captureLogs redirects slog.Default to a buffer for the duration of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

// withThreshold temporarily overrides the slow-segment warning threshold.
func withThreshold(t *testing.T, d time.Duration) {
	t.Helper()
	old := slowSegmentThreshold.Load()
	slowSegmentThreshold.Store(d)
	t.Cleanup(func() { slowSegmentThreshold.Store(old) })
}

// findLog returns the first captured JSON log entry with the given msg, or nil.
func findLog(t *testing.T, buf *bytes.Buffer, msg string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			continue
		}
		if m["msg"] == msg {
			return m
		}
	}
	return nil
}

func TestFetchWarnsOnSlowUpstreamSegment(t *testing.T) {
	withThreshold(t, 10*time.Millisecond)
	var hits atomic.Int64
	gate := make(chan struct{})
	srv := slowUpstream(t, &hits, []byte("first"), []byte("rest"), gate)
	defer srv.Close()

	buf := captureLogs(t)
	c := newMuxCache(64 << 20)
	e := c.get("nos/111/848x480/seg-1.ts", srv.URL, time.Minute, nil)
	time.Sleep(30 * time.Millisecond)
	close(gate)
	e.waitDone()
	c.wait() // the warning is emitted after finish, on the fetch goroutine

	entry := findLog(t, buf, "slow upstream segment")
	if entry == nil {
		t.Fatalf("expected slow upstream segment warning, got: %s", buf.String())
	}
	if entry["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", entry["level"])
	}
	if entry["key"] != "nos/111/848x480/seg-1.ts" {
		t.Errorf("key = %v", entry["key"])
	}
	if _, ok := entry["duration_ms"]; !ok {
		t.Errorf("duration_ms missing: %v", entry)
	}
	if bytes, _ := entry["bytes"].(float64); int(bytes) != len("firstrest") {
		t.Errorf("bytes = %v, want %d", entry["bytes"], len("firstrest"))
	}
}

func TestFetchDoesNotWarnForSlowPlaylist(t *testing.T) {
	withThreshold(t, 10*time.Millisecond)
	var hits atomic.Int64
	gate := make(chan struct{})
	srv := slowUpstream(t, &hits, []byte("#EXTM3U"), []byte("\nseg.ts"), gate)
	defer srv.Close()

	buf := captureLogs(t)
	c := newMuxCache(64 << 20)
	e := c.get("nos/111/848x480/playlist", srv.URL, time.Minute, nil)
	time.Sleep(30 * time.Millisecond)
	close(gate)
	e.waitDone()
	c.wait()

	if entry := findLog(t, buf, "slow upstream segment"); entry != nil {
		t.Errorf("playlist should not warn as slow segment: %v", entry)
	}
}
