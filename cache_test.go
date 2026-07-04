package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// testStreamCache returns a cache backed by stub fetchers plus counters of
// how often each fetcher ran. The stream list has one stream "111"; its
// master is the testMaster fixture (640x360 and 1920x1080 variants).
func testStreamCache() (*streamCache, *int, *int) {
	streamCalls, playlistCalls := new(0), new(0)
	c := &streamCache{
		entries: map[string]cacheEntry{},
		fetchStreams: func() ([]Stream, error) {
			*streamCalls++
			return []Stream{{
				ID:      "111",
				Title:   "Stream A",
				Formats: []Format{{Mimetype: hlsMimetype, URL: "https://resolver.example/stream111"}},
			}}, nil
		},
		fetchPlaylist: func(u string) (string, string, error) {
			*playlistCalls++
			return "https://cdn.example.com/live/playlist.m3u8", testMaster, nil
		},
	}
	return c, streamCalls, playlistCalls
}

func TestCacheStream(t *testing.T) {
	c, streamCalls, _ := testStreamCache()
	now := time.Now()

	rec, ok, err := c.stream("111", now)
	if err != nil || !ok || rec.Title != "Stream A" || rec.StreamURL != "https://resolver.example/stream111" {
		t.Fatalf("first lookup: got %+v, %v, %v", rec, ok, err)
	}
	if _, ok, _ := c.stream("111", now); !ok {
		t.Errorf("second lookup missed")
	}
	if *streamCalls != 1 {
		t.Errorf("fetchStreams ran %d times, want 1 (second lookup should hit the cache)", *streamCalls)
	}
}

func TestCacheStreamExpiry(t *testing.T) {
	c, streamCalls, _ := testStreamCache()
	now := time.Now()

	c.stream("111", now)
	if _, ok, _ := c.stream("111", now.Add(cacheTTL+time.Minute)); !ok {
		t.Errorf("lookup after expiry missed (should refetch)")
	}
	if *streamCalls != 2 {
		t.Errorf("fetchStreams ran %d times, want 2 (expired entry refetched)", *streamCalls)
	}
}

func TestCacheUnknownStreamCooldown(t *testing.T) {
	c, streamCalls, _ := testStreamCache()
	now := time.Now()

	if _, ok, err := c.stream("999", now); ok || err != nil {
		t.Fatalf("unknown id: got hit (%v)", err)
	}
	if _, ok, _ := c.stream("999", now.Add(time.Second)); ok {
		t.Fatalf("unknown id: got hit")
	}
	if *streamCalls != 1 {
		t.Errorf("fetchStreams ran %d times, want 1 (cooldown suppresses refetch)", *streamCalls)
	}
	c.stream("999", now.Add(snapshotCooldown+time.Second))
	if *streamCalls != 2 {
		t.Errorf("fetchStreams ran %d times, want 2 (cooldown over)", *streamCalls)
	}
}

func TestCacheStreamFetchError(t *testing.T) {
	c, _, _ := testStreamCache()
	c.fetchStreams = func() ([]Stream, error) { return nil, errors.New("boom") }
	if _, ok, err := c.stream("111", time.Now()); ok || err == nil {
		t.Errorf("got ok=%v err=%v, want miss with error", ok, err)
	}
}

func TestCacheVariant(t *testing.T) {
	c, streamCalls, playlistCalls := testStreamCache()
	now := time.Now()

	v, ok, err := c.variant("111", "1920x1080", now)
	if err != nil || !ok {
		t.Fatalf("variant lookup: %v, %v", ok, err)
	}
	if want := "https://cdn.example.com/live/event-audio=256000-video=3499968.m3u8"; v.URL != want {
		t.Errorf("variant URL = %q, want %q", v.URL, want)
	}

	// Second resolution and the master come from the same cached fetch.
	if _, ok, _ := c.variant("111", "640x360", now); !ok {
		t.Errorf("640x360 missed after master resolve")
	}
	if _, ok, _ := c.master("111", now); !ok {
		t.Errorf("master missed after resolve")
	}
	if *streamCalls != 1 || *playlistCalls != 1 {
		t.Errorf("fetches: streams=%d playlists=%d, want 1 each", *streamCalls, *playlistCalls)
	}

	// A fresh master means an unknown resolution is a fast miss, no refetch.
	if _, ok, err := c.variant("111", "123x456", now); ok || err != nil {
		t.Errorf("bogus resolution: got hit (%v)", err)
	}
	if *playlistCalls != 1 {
		t.Errorf("bogus resolution triggered a master refetch (%d playlist fetches)", *playlistCalls)
	}
}

func TestCacheVariantMasterFetchFailureEvicts(t *testing.T) {
	c, streamCalls, _ := testStreamCache()
	now := time.Now()

	c.stream("111", now) // populate the stream record
	c.fetchPlaylist = func(string) (string, string, error) { return "", "", errors.New("gone") }
	if _, ok, err := c.variant("111", "1920x1080", now); ok || err == nil {
		t.Fatalf("got ok=%v err=%v, want miss with error", ok, err)
	}
	if _, found := c.get(streamKey("111"), now); found {
		t.Errorf("stream record not evicted after master fetch failure")
	}
	// Eviction resets the snapshot cooldown so the next lookup re-checks
	// streams.json immediately.
	c.stream("111", now.Add(time.Second))
	if *streamCalls != 2 {
		t.Errorf("fetchStreams ran %d times, want 2 (eviction lifts cooldown)", *streamCalls)
	}
}

func TestSetSnapshotPrunesVanishedStreams(t *testing.T) {
	c, _, _ := testStreamCache()
	now := time.Now()
	c.variant("111", "1920x1080", now) // populates nos/111, nos/111/master, nos/111/{res}

	c.setSnapshot([]Stream{{
		ID:      "222",
		Title:   "Stream B",
		Formats: []Format{{Mimetype: hlsMimetype, URL: "https://resolver.example/stream222"}},
	}}, now)

	for _, key := range []string{streamKey("111"), masterKey("111"), variantKey("111", "1920x1080")} {
		if _, found := c.get(key, now); found {
			t.Errorf("%s survived a snapshot that no longer lists stream 111", key)
		}
	}
	if _, found := c.get(streamKey("222"), now); !found {
		t.Errorf("new stream record missing after snapshot")
	}
}

func TestSetMasterPrunesStaleResolutions(t *testing.T) {
	c, _, _ := testStreamCache()
	now := time.Now()

	c.setMaster("111", "x", []Variant{
		{Resolution: "1920x1080", Bandwidth: 5000, URL: "https://cdn/hi.m3u8"},
		{Resolution: "1920x1080", Bandwidth: 3000, URL: "https://cdn/lo.m3u8"}, // duplicate: first (highest bandwidth) wins
		{Resolution: "960x540", Bandwidth: 1500, URL: "https://cdn/540.m3u8"},
	}, now)

	if v, _ := c.get(variantKey("111", "1920x1080"), now); v.(variantRecord).URL != "https://cdn/hi.m3u8" {
		t.Errorf("duplicate resolution: got %+v, want the highest-bandwidth variant", v)
	}

	c.setMaster("111", "x", []Variant{{Resolution: "1280x720", Bandwidth: 2000, URL: "https://cdn/720.m3u8"}}, now)
	if _, found := c.get(variantKey("111", "960x540"), now); found {
		t.Errorf("stale resolution survived a master refresh")
	}
	if _, found := c.get(variantKey("111", "1280x720"), now); !found {
		t.Errorf("new resolution missing after master refresh")
	}
}

func TestCacheViews(t *testing.T) {
	c, _, _ := testStreamCache()
	now := time.Now()
	c.variant("111", "1920x1080", now)

	views := c.views(now)
	wantKeys := []string{"nos/111", "nos/111/1920x1080", "nos/111/640x360", "nos/111/master"}
	if len(views) != len(wantKeys) {
		t.Fatalf("got %d entries, want %d: %+v", len(views), len(wantKeys), views)
	}
	for i, want := range wantKeys {
		if views[i].Key != want {
			t.Errorf("view %d key = %q, want %q (sorted)", i, views[i].Key, want)
		}
	}
	if views[0].Expires != "in 12h 0m" {
		t.Errorf("expires label = %q, want %q", views[0].Expires, "in 12h 0m")
	}
	if !strings.Contains(views[0].Contents, `stream "Stream A"`) {
		t.Errorf("stream contents = %q", views[0].Contents)
	}
	if !strings.Contains(views[len(views)-1].Contents, "master playlist") {
		t.Errorf("master contents = %q", views[len(views)-1].Contents)
	}

	if got := c.views(now.Add(cacheTTL + time.Minute)); len(got) != 0 {
		t.Errorf("expired entries not pruned from views: %+v", got)
	}
}

func TestUntilLabel(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{12 * time.Hour, "12h 0m"},
		{90 * time.Minute, "1h 30m"},
		{5 * time.Minute, "5m"},
		{30 * time.Second, "30s"},
		{-time.Second, "0s"},
	}
	for _, tt := range tests {
		if got := untilLabel(tt.d); got != tt.want {
			t.Errorf("untilLabel(%v) = %q, want %q", tt.d, got, tt.want)
		}
	}
}

func TestTruncateMiddle(t *testing.T) {
	if got := truncateMiddle("short", 80); got != "short" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("a", 50) + strings.Repeat("b", 50)
	got := truncateMiddle(long, 21)
	if len([]rune(got)) != 21 || !strings.HasPrefix(got, "aaa") || !strings.HasSuffix(got, "bbb") || !strings.Contains(got, "…") {
		t.Errorf("got %q", got)
	}
}
