package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// The memory tier of the segment cache: concurrent requests for the same
// key share one in-flight upstream fetch (followers stream from the growing
// buffer), and completed bodies are kept for memoryCacheTTL so live viewers
// within that window are served from memory and stay in sync. Bounded by
// TTL and size, whichever hits first.

const (
	muxPlaylistTTL = 2 * time.Second
	muxFetchLimit  = time.Minute
)

// Configurable via VOETBAL_MEMORY_CACHE_TTL / VOETBAL_MEMORY_CACHE_SIZE;
// VOETBAL_MEMORY_CACHE_DISABLED turns the tier off entirely.
var memoryCacheTTL = 3 * time.Minute

// atomicDuration is a time.Duration that is safe to read from the decoupled
// fetch goroutine while it is (re)configured elsewhere.
type atomicDuration struct{ v atomic.Int64 }

func (a *atomicDuration) Load() time.Duration   { return time.Duration(a.v.Load()) }
func (a *atomicDuration) Store(d time.Duration) { a.v.Store(int64(d)) }

// slowSegmentThreshold bounds how long a segment may take before it is logged
// as a stutter risk: a shared upstream download or a client's time-to-first-
// byte past this triggers a slog.Warn. Configurable via
// VOETBAL_SLOW_SEGMENT_WARN. Stored atomically because warnIfSlow reads it
// from the decoupled fetch goroutine.
var slowSegmentThreshold = func() *atomicDuration {
	a := &atomicDuration{}
	a.Store(3 * time.Second)
	return a
}()

// isPlaylistKey reports whether a mux key addresses a media playlist rather
// than a segment; playlists poll on a short TTL and are excluded from the
// slow-segment warnings.
func isPlaylistKey(key string) bool { return strings.HasSuffix(key, "/playlist") }

type muxEntry struct {
	key     string
	ttl     time.Duration
	created time.Time
	persist func([]byte) // called once with the complete body on success

	mu          sync.Mutex
	cond        *sync.Cond
	buf         []byte
	ready       bool
	done        bool
	err         error
	status      int
	contentType string
	finalURL    *url.URL
}

func newMuxEntry(key string, ttl time.Duration, persist func([]byte)) *muxEntry {
	e := &muxEntry{key: key, ttl: ttl, persist: persist, created: time.Now()}
	e.cond = sync.NewCond(&e.mu)
	return e
}

// fetch downloads rawURL into the shared buffer. It runs decoupled from any
// client's request context so a disconnecting viewer does not abort the
// download for the others.
func (e *muxEntry) fetch(rawURL string) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), muxFetchLimit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		e.finish(err)
		return
	}
	setBrowserHeaders(req)
	resp, err := httpClient.Do(req)
	if err != nil {
		e.finish(err)
		return
	}
	defer resp.Body.Close()
	ttfb := time.Since(start)
	e.mu.Lock()
	e.status = resp.StatusCode
	e.contentType = cmp.Or(resp.Header.Get("Content-Type"), "application/octet-stream")
	e.finalURL = resp.Request.URL
	e.ready = true
	e.cond.Broadcast()
	e.mu.Unlock()
	chunk := make([]byte, 64<<10)
	var bytes int64
	for {
		n, err := resp.Body.Read(chunk)
		if n > 0 {
			bytes += int64(n)
			e.mu.Lock()
			e.buf = append(e.buf, chunk[:n]...)
			e.cond.Broadcast()
			e.mu.Unlock()
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = nil
			}
			status := resp.StatusCode
			e.finish(err)
			e.warnIfSlow(rawURL, ttfb, time.Since(start), bytes, status, err)
			return
		}
	}
}

// warnIfSlow logs a WARN when a shared upstream segment download takes longer
// than slowSegmentThreshold: a slow leader fetch stalls every viewer coalesced
// onto it. Playlists and failed fetches are skipped (the latter are already
// logged by loggingTransport).
func (e *muxEntry) warnIfSlow(rawURL string, ttfb, total time.Duration, bytes int64, status int, err error) {
	if err != nil || isPlaylistKey(e.key) || total <= slowSegmentThreshold.Load() {
		return
	}
	slog.Warn("slow upstream segment",
		"key", e.key, "url", rawURL,
		"ttfb_ms", ttfb.Milliseconds(), "duration_ms", total.Milliseconds(),
		"bytes", bytes, "status", status)
}

func (e *muxEntry) finish(err error) {
	e.mu.Lock()
	e.done = true
	e.err = err
	success := err == nil && e.status == http.StatusOK
	buf := e.buf // not appended to anymore once done
	e.cond.Broadcast()
	e.mu.Unlock()
	if success && e.persist != nil {
		e.persist(buf)
	}
}

func (e *muxEntry) waitHeader() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for !e.ready && !e.done {
		e.cond.Wait()
	}
	return e.err
}

func (e *muxEntry) waitDone() ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for !e.done {
		e.cond.Wait()
	}
	return e.buf, e.err
}

func (e *muxEntry) lengthIfDone() (int, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done && e.err == nil {
		return len(e.buf), true
	}
	return 0, false
}

// serve streams the buffer to w, following along while the fetch is still
// in progress.
func (e *muxEntry) serve(w http.ResponseWriter) {
	flusher, _ := w.(http.Flusher)
	offset := 0
	for {
		e.mu.Lock()
		for len(e.buf) == offset && !e.done {
			e.cond.Wait()
		}
		chunk := e.buf[offset:]
		done := e.done
		e.mu.Unlock()
		if len(chunk) > 0 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			offset += len(chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if done && len(chunk) == 0 {
			return
		}
	}
}

func (e *muxEntry) size() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.buf)
}

// expired reports whether the entry should no longer be served: past its
// TTL, or finished with an error (so failures retry immediately).
func (e *muxEntry) expired(now time.Time) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done && e.err != nil {
		return true
	}
	return now.Sub(e.created) > e.ttl
}

func (e *muxEntry) describe() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case !e.done:
		return fmt.Sprintf("in flight, %s so far", humanBytes(int64(len(e.buf))))
	case strings.HasSuffix(e.key, "/playlist"):
		return fmt.Sprintf("media playlist, %s", humanBytes(int64(len(e.buf))))
	default:
		return fmt.Sprintf("segment, %s", humanBytes(int64(len(e.buf))))
	}
}

type muxCache struct {
	mu       sync.Mutex
	disabled bool // VOETBAL_MEMORY_CACHE_DISABLED: nothing shared, nothing kept
	maxBytes int64
	entries  map[string]*muxEntry
	hits     int64 // requests served from an existing entry
	misses   int64 // requests that started an upstream fetch
	fetches  sync.WaitGroup
}

var streamMux = newMuxCache(512 << 20)

func newMuxCache(maxBytes int64) *muxCache {
	return &muxCache{maxBytes: maxBytes, entries: make(map[string]*muxEntry)}
}

// peek returns the live entry for key without starting a fetch. A peek
// miss is not counted: the request may still be served by the disk tier
// or end up in get, which counts the outcome.
func (c *muxCache) peek(key string) *muxEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled {
		return nil
	}
	if e, ok := c.entries[key]; ok && !e.expired(time.Now()) {
		c.hits++
		return e
	}
	return nil
}

// get returns the live entry for key, starting a shared fetch of rawURL on
// a miss. persist, if non-nil, receives the complete body after a
// successful fetch. On a disabled cache every call fetches fresh: the entry
// is not stored, so nothing is shared or retained, but persist still runs
// (the disk tier is controlled separately).
func (c *muxCache) get(key, rawURL string, ttl time.Duration, persist func([]byte)) *muxEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled {
		e := newMuxEntry(key, ttl, persist)
		c.fetches.Go(func() { e.fetch(rawURL) })
		return e
	}
	now := time.Now()
	if e, ok := c.entries[key]; ok && !e.expired(now) {
		c.hits++
		return e
	}
	c.misses++
	e := newMuxEntry(key, ttl, persist)
	c.entries[key] = e
	c.evict(now, key)
	c.fetches.Go(func() { e.fetch(rawURL) })
	return e
}

// wait blocks until all background fetches (including their disk
// write-through) have finished. Used by tests to drain the cache.
func (c *muxCache) wait() {
	c.fetches.Wait()
}

// stats returns how many requests were served from memory vs. had to
// start an upstream fetch.
func (c *muxCache) stats() (hits, misses int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses
}

// evict removes expired entries and, while the buffer exceeds maxBytes,
// the oldest remaining ones — TTL or size, whichever hits first. Callers
// hold c.mu.
func (c *muxCache) evict(now time.Time, keep string) {
	var total int64
	for key, e := range c.entries {
		if key != keep && e.expired(now) {
			delete(c.entries, key)
			continue
		}
		total += int64(e.size())
	}
	for total > c.maxBytes && len(c.entries) > 1 {
		var oldestKey string
		var oldest *muxEntry
		for key, e := range c.entries {
			if key != keep && (oldest == nil || e.created.Before(oldest.created)) {
				oldestKey, oldest = key, e
			}
		}
		if oldest == nil {
			return
		}
		total -= int64(oldest.size())
		delete(c.entries, oldestKey)
	}
}

func (c *muxCache) totalBytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var total int64
	for _, e := range c.entries {
		total += int64(e.size())
	}
	return total
}

// views prunes expired entries and returns the remainder for /caches.
func (c *muxCache) views(now time.Time) []cacheView {
	c.mu.Lock()
	defer c.mu.Unlock()
	views := make([]cacheView, 0, len(c.entries))
	for key, e := range c.entries {
		if e.expired(now) {
			delete(c.entries, key)
			continue
		}
		views = append(views, cacheView{
			Key:      key,
			Fetched:  e.created.Format("15:04:05"),
			Expires:  "in " + untilLabel(e.created.Add(e.ttl).Sub(now)),
			Contents: e.describe(),
		})
	}
	return views
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
