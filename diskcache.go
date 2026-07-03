package main

import (
	"cmp"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// The disk tier of the segment cache: every successfully fetched segment is
// written through to {VOETBAL_DATA_PATH}/cache/{key}, so seeking back in a
// stream is served from disk instead of the CDN. The index is rebuilt from
// the directory at startup, so the cache survives restarts. Bounded by TTL
// and size, whichever hits first. A zero size (VOETBAL_DISK_CACHE_SIZE=0)
// or an unusable directory disables the tier.

type diskFile struct {
	Size    int64
	Fetched time.Time
}

type diskCache struct {
	mu       sync.Mutex
	root     string // "" when disabled
	ttl      time.Duration
	maxBytes int64
	files    map[string]diskFile
	hits     int64
	misses   int64
}

// diskCaches is disabled until main wires it up from the environment.
var diskCaches = &diskCache{}

// newDiskCache creates root if needed and indexes what is already there,
// removing entries that expired while the server was down.
func newDiskCache(root string, ttl time.Duration, maxBytes int64) (*diskCache, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	c := &diskCache{root: root, ttl: ttl, maxBytes: maxBytes, files: map[string]diskFile{}}
	now := time.Now()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || strings.HasPrefix(d.Name(), ".tmp-") {
			os.Remove(path)
			return nil
		}
		c.files[filepath.ToSlash(rel)] = diskFile{Size: info.Size(), Fetched: info.ModTime()}
		return nil
	})
	if err != nil {
		return nil, err
	}
	c.prune(now)
	return c, nil
}

func (c *diskCache) enabled() bool { return c.root != "" && c.maxBytes > 0 }

// path maps a cache key to its file path, rejecting keys that would escape
// the cache root.
func (c *diskCache) path(key string) (string, bool) {
	path := filepath.Join(c.root, filepath.FromSlash(key))
	if !strings.HasPrefix(path, c.root+string(filepath.Separator)) {
		return "", false
	}
	return path, true
}

// put stores a complete segment body under key (temp file + atomic rename)
// and evicts whatever the new entry pushes over the TTL or size limit.
func (c *diskCache) put(key string, data []byte, now time.Time) {
	if !c.enabled() {
		return
	}
	path, ok := c.path(key)
	if !ok {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.files[key] = diskFile{Size: int64(len(data)), Fetched: now}
	c.pruneLocked(now)
}

// open returns the file path for a fresh cache hit; expired entries are
// removed and miss.
func (c *diskCache) open(key string, now time.Time) (string, bool) {
	if !c.enabled() {
		return "", false
	}
	path, ok := c.path(key)
	if !ok {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	f, found := c.files[key]
	if !found {
		c.misses++
		return "", false
	}
	if now.Sub(f.Fetched) > c.ttl {
		delete(c.files, key)
		os.Remove(path)
		c.misses++
		return "", false
	}
	c.hits++
	return path, true
}

// stats returns how many lookups were served from disk vs. missed.
func (c *diskCache) stats() (hits, misses int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}

// prune drops entries past the TTL and then the oldest entries while the
// total exceeds maxBytes — time or space, whichever hits first.
func (c *diskCache) prune(now time.Time) {
	if !c.enabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
}

func (c *diskCache) pruneLocked(now time.Time) {
	var total int64
	type aged struct {
		key string
		f   diskFile
	}
	var all []aged
	for key, f := range c.files {
		if now.Sub(f.Fetched) > c.ttl {
			c.removeLocked(key)
			continue
		}
		total += f.Size
		all = append(all, aged{key, f})
	}
	if total <= c.maxBytes {
		return
	}
	slices.SortFunc(all, func(a, b aged) int { return a.f.Fetched.Compare(b.f.Fetched) })
	for _, entry := range all {
		if total <= c.maxBytes {
			return
		}
		total -= entry.f.Size
		c.removeLocked(entry.key)
	}
}

func (c *diskCache) removeLocked(key string) {
	delete(c.files, key)
	if path, ok := c.path(key); ok {
		os.Remove(path)
	}
}

func (c *diskCache) totalBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var total int64
	for _, f := range c.files {
		total += f.Size
	}
	return total
}

// views aggregates the disk tier per directory (nos/{id}/{res}) — listing
// thousands of individual segments would swamp the /caches page.
func (c *diskCache) views(now time.Time) []cacheView {
	if !c.enabled() {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(now)
	type group struct {
		count          int
		bytes          int64
		oldest, newest time.Time
	}
	groups := map[string]*group{}
	for key, f := range c.files {
		dir := key
		if i := strings.LastIndex(key, "/"); i >= 0 {
			dir = key[:i]
		}
		g := groups[dir]
		if g == nil {
			g = &group{oldest: f.Fetched, newest: f.Fetched}
			groups[dir] = g
		}
		g.count++
		g.bytes += f.Size
		g.oldest = minTime(g.oldest, f.Fetched)
		g.newest = maxTime(g.newest, f.Fetched)
	}
	views := make([]cacheView, 0, len(groups))
	for dir, g := range groups {
		views = append(views, cacheView{
			Key:      dir + "/*",
			Fetched:  g.newest.Format("15:04:05"),
			Expires:  "in " + untilLabel(g.oldest.Add(c.ttl).Sub(now)),
			Contents: fmt.Sprintf("disk: %d segments, %s, spanning %s", g.count, humanBytes(g.bytes), untilLabel(g.newest.Sub(g.oldest))),
		})
	}
	slices.SortFunc(views, func(a, b cacheView) int { return cmp.Compare(a.Key, b.Key) })
	return views
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
