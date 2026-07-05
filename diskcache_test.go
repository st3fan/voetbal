package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testDiskCache(t *testing.T, ttl time.Duration, maxBytes int64) *diskCache {
	t.Helper()
	c, err := newDiskCache(filepath.Join(t.TempDir(), "cache"), ttl, maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDiskCachePutOpen(t *testing.T) {
	c := testDiskCache(t, time.Hour, 1<<20)
	now := time.Now()

	if _, ok := c.open("nos/1/848x480/seg.ts", now); ok {
		t.Fatal("open before put hit")
	}
	c.put("nos/1/848x480/seg.ts", []byte("segment"), now)
	path, ok := c.open("nos/1/848x480/seg.ts", now)
	if !ok {
		t.Fatal("open after put missed")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "segment" {
		t.Errorf("file contents = %q, %v", data, err)
	}
}

func TestDiskCacheTTLExpiry(t *testing.T) {
	c := testDiskCache(t, time.Hour, 1<<20)
	now := time.Now()

	c.put("nos/1/848x480/seg.ts", []byte("segment"), now)
	path, _ := c.open("nos/1/848x480/seg.ts", now)
	if _, ok := c.open("nos/1/848x480/seg.ts", now.Add(2*time.Hour)); ok {
		t.Fatal("expired entry served")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expired file not removed: %v", err)
	}
}

func TestDiskCacheSizeEviction(t *testing.T) {
	c := testDiskCache(t, time.Hour, 100)
	now := time.Now()

	c.put("a", make([]byte, 60), now.Add(-2*time.Second))
	c.put("b", make([]byte, 60), now.Add(-time.Second))
	c.put("c", make([]byte, 60), now)

	if _, ok := c.open("a", now); ok {
		t.Error("oldest entry survived size eviction")
	}
	if _, ok := c.open("c", now); !ok {
		t.Error("newest entry evicted")
	}
	if total := c.totalBytes(); total > 100 {
		t.Errorf("total %d exceeds cap 100", total)
	}
}

func TestDiskCacheRescan(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	now := time.Now()
	first, err := newDiskCache(root, time.Hour, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	first.put("nos/1/848x480/seg.ts", []byte("segment"), now)

	second, err := newDiskCache(root, time.Hour, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	path, ok := second.open("nos/1/848x480/seg.ts", now)
	if !ok {
		t.Fatal("rescan lost the cached segment")
	}
	if data, _ := os.ReadFile(path); string(data) != "segment" {
		t.Errorf("file contents = %q", data)
	}
}

func TestDiskCacheRescanDropsExpired(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	first, err := newDiskCache(root, time.Hour, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	first.put("old.ts", []byte("old"), time.Now())
	path, _ := first.open("old.ts", time.Now())
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatal(err)
	}

	second, err := newDiskCache(root, time.Hour, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := second.open("old.ts", time.Now()); ok {
		t.Error("expired file survived rescan")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expired file not removed at rescan: %v", err)
	}
}

func TestDiskCacheTraversalGuard(t *testing.T) {
	c := testDiskCache(t, time.Hour, 1<<20)
	now := time.Now()
	outside := filepath.Join(c.root, "..", "escape")

	c.put("../escape", []byte("nope"), now)
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Errorf("traversal key escaped the cache root: %v", err)
	}
	if _, ok := c.open("../escape", now); ok {
		t.Error("traversal key served")
	}
}

func TestDiskCacheDisabled(t *testing.T) {
	c := &diskCache{} // zero value: disabled
	now := time.Now()
	c.put("k", []byte("x"), now)
	if _, ok := c.open("k", now); ok {
		t.Error("disabled cache served an entry")
	}
	if views := c.views(now); views != nil {
		t.Errorf("disabled cache has views: %+v", views)
	}
	if c.enabled() {
		t.Error("zero-value cache reports enabled")
	}
}

func TestDiskCacheViewsAggregated(t *testing.T) {
	c := testDiskCache(t, time.Hour, 1<<20)
	now := time.Now()
	c.put("nos/1/848x480/seg-1.ts", make([]byte, 1000), now.Add(-time.Minute))
	c.put("nos/1/848x480/seg-2.ts", make([]byte, 1000), now)
	c.put("nos/1/1920x1080/seg-1.ts", make([]byte, 2000), now)

	views := c.views(now)
	if len(views) != 2 {
		t.Fatalf("got %d views, want 2 aggregated groups: %+v", len(views), views)
	}
	if views[0].Key != "nos/1/1920x1080/*" || views[1].Key != "nos/1/848x480/*" {
		t.Errorf("keys = %q, %q", views[0].Key, views[1].Key)
	}
	if !strings.Contains(views[1].Contents, "2 segments") || !strings.Contains(views[1].Contents, "spanning 1m") {
		t.Errorf("aggregate contents = %q", views[1].Contents)
	}
}

func TestParseSize(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"512MB", 512 << 20, false},
		{"12GB", 12 << 30, false},
		{"512", 512 << 20, false},
		{"0", 0, false},
		{" 2gb ", 2 << 30, false},
		{"twelve", 0, true},
		{"-1", 0, true},
	}
	for _, tt := range tests {
		got, err := parseSize(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("parseSize(%q) = %d, %v; want %d, err=%v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestDiskCacheStats(t *testing.T) {
	c := testDiskCache(t, time.Hour, 1<<20)
	now := time.Now()

	c.open("k", now) // miss
	c.put("k", []byte("x"), now)
	c.open("k", now)                  // hit
	c.open("k", now.Add(2*time.Hour)) // expired: miss
	h, m := c.stats()
	if h != 1 || m != 2 {
		t.Errorf("stats = %d hits, %d misses; want 1, 2", h, m)
	}
}
