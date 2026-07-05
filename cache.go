package main

import (
	"cmp"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// cacheTTL bounds how long stream metadata is reused. The upstream
	// variant URLs embed a token that is valid for 24 hours, so entries
	// cached for 12 hours always carry a usable token.
	cacheTTL = 12 * time.Hour
	// snapshotCooldown rate-limits streams.json refetches caused by
	// lookups of unknown stream ids.
	snapshotCooldown = 30 * time.Second
)

type streamRecord struct {
	Title     string
	StreamURL string
}

type masterRecord struct {
	Text     string
	Variants int
}

type variantRecord struct {
	URL       string
	Bandwidth int
}

type cacheEntry struct {
	Fetched time.Time
	Expires time.Time
	Value   any
}

// streamCache is a read-through cache of upstream stream metadata, keyed
//
//	nos/{id}          streamRecord, from streams.json
//	nos/{id}/master   masterRecord, the raw master playlist
//	nos/{id}/{res}    variantRecord, the resolved variant URL
//
// Records are immutable and replaced wholesale; every multi-key update
// happens under a single lock acquisition and derives from a single
// upstream response, so the cache never holds a torn set. Upstream fetches
// happen outside the lock: concurrent misses may fetch twice, and the last
// write wins with equally fresh data.
type streamCache struct {
	mu            sync.Mutex
	entries       map[string]cacheEntry
	lastSnapshot  time.Time
	fetchStreams  func() ([]Stream, error)
	fetchPlaylist func(string) (string, string, error)
}

var streamCaches = newStreamCache()

func newStreamCache() *streamCache {
	return &streamCache{
		entries:       map[string]cacheEntry{},
		fetchStreams:  fetchStreams,
		fetchPlaylist: fetchPlaylist,
	}
}

func streamKey(id string) string       { return "nos/" + id }
func masterKey(id string) string       { return "nos/" + id + "/master" }
func variantKey(id, res string) string { return "nos/" + id + "/" + res }
func childPrefix(id string) string     { return "nos/" + id + "/" }

// cacheKeyID extracts the stream id from any cache key.
func cacheKeyID(key string) string {
	id, _, _ := strings.Cut(strings.TrimPrefix(key, "nos/"), "/")
	return id
}

// get returns a live entry's value; expired entries are deleted and miss.
func (c *streamCache) get(key string, now time.Time) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if now.After(e.Expires) {
		delete(c.entries, key)
		return nil, false
	}
	return e.Value, true
}

// setSnapshot replaces the stream records with the given streams.json
// snapshot and prunes every entry (including master/variant children) of
// streams that are no longer in it.
func (c *streamCache) setSnapshot(streams []Stream, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	live := map[string]bool{}
	for _, s := range streams {
		if s.ID == "" || streamURL(s) == "" {
			continue
		}
		live[s.ID] = true
		c.entries[streamKey(s.ID)] = cacheEntry{now, now.Add(cacheTTL), streamRecord{Title: s.Title, StreamURL: streamURL(s)}}
	}
	maps.DeleteFunc(c.entries, func(key string, _ cacheEntry) bool {
		return !live[cacheKeyID(key)]
	})
	c.lastSnapshot = now
}

// setMaster stores the master playlist and one variant record per
// resolution (variants come bandwidth-sorted, the first per resolution
// wins) and prunes resolutions that disappeared from the master.
func (c *streamCache) setMaster(id, text string, variants []Variant, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	kept := map[string]bool{masterKey(id): true}
	c.entries[masterKey(id)] = cacheEntry{now, now.Add(cacheTTL), masterRecord{Text: text, Variants: len(variants)}}
	for _, v := range variants {
		key := variantKey(id, v.Resolution)
		if v.Resolution == "" || v.URL == "" || kept[key] {
			continue
		}
		kept[key] = true
		c.entries[key] = cacheEntry{now, now.Add(cacheTTL), variantRecord{URL: v.URL, Bandwidth: v.Bandwidth}}
	}
	maps.DeleteFunc(c.entries, func(key string, _ cacheEntry) bool {
		return strings.HasPrefix(key, childPrefix(id)) && !kept[key]
	})
}

// evict drops everything cached for a stream and forces the next miss to
// re-check streams.json (used when upstream stops serving the stream).
func (c *streamCache) evict(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, streamKey(id))
	maps.DeleteFunc(c.entries, func(key string, _ cacheEntry) bool {
		return strings.HasPrefix(key, childPrefix(id))
	})
	c.lastSnapshot = time.Time{}
}

// stream returns the cached record for id, refreshing the streams.json
// snapshot on a miss (at most once per snapshotCooldown).
func (c *streamCache) stream(id string, now time.Time) (streamRecord, bool, error) {
	if v, ok := c.get(streamKey(id), now); ok {
		return v.(streamRecord), true, nil
	}
	c.mu.Lock()
	recent := now.Sub(c.lastSnapshot) < snapshotCooldown
	c.mu.Unlock()
	if recent {
		return streamRecord{}, false, nil
	}
	streams, err := c.fetchStreams()
	if err != nil {
		return streamRecord{}, false, err
	}
	c.setSnapshot(streams, now)
	if v, ok := c.get(streamKey(id), now); ok {
		return v.(streamRecord), true, nil
	}
	return streamRecord{}, false, nil
}

// resolveMaster fetches and caches the master playlist and its variants.
// When the fetch fails the stream has likely ended: its entries are
// evicted so the next request re-checks streams.json.
func (c *streamCache) resolveMaster(id string, now time.Time) error {
	rec, ok, err := c.stream(id, now)
	if err != nil || !ok {
		return err
	}
	masterURL, text, err := c.fetchPlaylist(rec.StreamURL)
	if err != nil {
		c.evict(id)
		return err
	}
	c.setMaster(id, text, parseVariants(text, masterURL), now)
	return nil
}

func (c *streamCache) master(id string, now time.Time) (string, bool, error) {
	if v, ok := c.get(masterKey(id), now); ok {
		return v.(masterRecord).Text, true, nil
	}
	if err := c.resolveMaster(id, now); err != nil {
		return "", false, err
	}
	if v, ok := c.get(masterKey(id), now); ok {
		return v.(masterRecord).Text, true, nil
	}
	return "", false, nil
}

func (c *streamCache) variant(id, resolution string, now time.Time) (variantRecord, bool, error) {
	if v, ok := c.get(variantKey(id, resolution), now); ok {
		return v.(variantRecord), true, nil
	}
	// A live master record means the variant set is current and this
	// resolution simply does not exist: 404 without refetching.
	if _, ok := c.get(masterKey(id), now); ok {
		return variantRecord{}, false, nil
	}
	if err := c.resolveMaster(id, now); err != nil {
		return variantRecord{}, false, err
	}
	if v, ok := c.get(variantKey(id, resolution), now); ok {
		return v.(variantRecord), true, nil
	}
	return variantRecord{}, false, nil
}

type cacheView struct {
	Key      string
	Fetched  string
	Expires  string
	Contents string
}

// views prunes expired entries and returns the remainder for /caches.
func (c *streamCache) views(now time.Time) []cacheView {
	c.mu.Lock()
	defer c.mu.Unlock()
	maps.DeleteFunc(c.entries, func(_ string, e cacheEntry) bool { return now.After(e.Expires) })
	views := make([]cacheView, 0, len(c.entries))
	for key, e := range c.entries {
		views = append(views, cacheView{
			Key:      key,
			Fetched:  e.Fetched.Format("15:04:05"),
			Expires:  "in " + untilLabel(e.Expires.Sub(now)),
			Contents: describeRecord(e.Value),
		})
	}
	slices.SortFunc(views, func(a, b cacheView) int { return cmp.Compare(a.Key, b.Key) })
	return views
}

func untilLabel(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(max(d, 0).Seconds()))
	}
}

func describeRecord(v any) string {
	switch r := v.(type) {
	case streamRecord:
		return fmt.Sprintf("stream %q → %s", r.Title, truncateMiddle(r.StreamURL, 80))
	case masterRecord:
		return fmt.Sprintf("master playlist (%d variants)", r.Variants)
	case variantRecord:
		return fmt.Sprintf("%d kbps → %s", r.Bandwidth/1000, truncateMiddle(r.URL, 80))
	}
	return fmt.Sprintf("%v", v)
}

func truncateMiddle(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	half := (limit - 1) / 2
	return s[:half] + "…" + s[len(s)-half:]
}

// hitRate formats hit/miss counters for the /caches stat sections.
func hitRate(hits, misses int64) string {
	total := hits + misses
	if total == 0 {
		return "no lookups yet"
	}
	return fmt.Sprintf("%d hits · %d misses · %.1f%% hit rate", hits, misses, 100*float64(hits)/float64(total))
}

func handleCaches(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	entries := slices.Concat(streamCaches.views(now), streamMux.views(now), diskCaches.views(now))
	slices.SortFunc(entries, func(a, b cacheView) int { return cmp.Compare(a.Key, b.Key) })
	memory := "disabled"
	if !streamMux.disabled {
		memoryHits, memoryMisses := streamMux.stats()
		memory = fmt.Sprintf("%s · %s of %s · ttl %s",
			hitRate(memoryHits, memoryMisses), humanBytes(streamMux.totalBytes()),
			humanBytes(streamMux.maxBytes), untilLabel(memoryCacheTTL))
	}
	disk := "disabled"
	if diskCaches.enabled() {
		diskHits, diskMisses := diskCaches.stats()
		disk = fmt.Sprintf("%s · %s of %s · ttl %s",
			hitRate(diskHits, diskMisses), humanBytes(diskCaches.totalBytes()),
			humanBytes(diskCaches.maxBytes), untilLabel(diskCaches.ttl))
	}
	render(w, "caches.html", struct {
		Entries      []cacheView
		Memory, Disk string
	}{entries, memory, disk})
}
