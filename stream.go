package main

import (
	"cmp"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Short stream URLs. A stream is addressed by its NOS id and optionally a
// variant resolution; the upstream URLs are resolved through streamCaches
// (see cache.go) and an id or resolution that no longer exists is a 404.
//
//	/player/nos/{id}[/{resolution}]        in-browser player page
//	/stream/nos/{id}                       master playlist (adaptive)
//	/stream/nos/{id}/{resolution}          variant media playlist
//	/proxy/nos/{id}/{resolution}/{file}    segment fetch

func nosPath(kind, id, resolution string) string {
	p := "/" + kind + "/nos/" + url.PathEscape(id)
	if resolution != "" {
		p += "/" + url.PathEscape(resolution)
	}
	return p
}

func streamPath(id, resolution string) string { return nosPath("stream", id, resolution) }
func playerPath(id, resolution string) string { return nosPath("player", id, resolution) }

// rewriteMasterPlaylist replaces every variant URI with the matching
// /stream/nos/{id}/{resolution} path. Variants without a RESOLUTION and
// trick-play I-frame playlists cannot be addressed that way and are dropped.
func rewriteMasterPlaylist(text, id string) string {
	var out []string
	resolution := ""
	for line := range strings.SplitSeq(text, "\n") {
		stripped := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(stripped, "#EXT-X-I-FRAME-STREAM-INF:"):
		case strings.HasPrefix(stripped, "#EXT-X-STREAM-INF:"):
			if resolution = attr(resolutionAttr, stripped); resolution != "" {
				out = append(out, line)
			}
		case stripped != "" && !strings.HasPrefix(stripped, "#"):
			if resolution != "" {
				out = append(out, streamPath(id, resolution))
				resolution = ""
			}
		default:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// rewriteMediaPlaylist points every URI in a variant playlist at the segment
// proxy: URIs inside the playlist's own directory become prefix+file, so the
// served playlist never embeds an upstream URL.
func rewriteMediaPlaylist(text string, base *url.URL, prefix string) string {
	dir := *base
	dir.RawQuery, dir.Fragment = "", ""
	if i := strings.LastIndex(dir.Path, "/"); i >= 0 {
		dir.Path = dir.Path[:i+1]
	}
	baseDir := dir.String()
	rewrite := func(uri string) string {
		resolved := resolve(base, uri)
		if file, ok := strings.CutPrefix(resolved, baseDir); ok && file != "" {
			return prefix + file
		}
		return proxyPath(resolved)
	}
	var b strings.Builder
	b.Grow(2 * len(text))
	for i, line := range strings.Split(text, "\n") {
		if i > 0 {
			b.WriteByte('\n')
		}
		stripped := strings.TrimSpace(line)
		switch {
		case stripped == "":
			b.WriteString(line)
		case strings.HasPrefix(stripped, "#"):
			b.WriteString(uriAttr.ReplaceAllStringFunc(line, func(m string) string {
				return `URI="` + rewrite(uriAttr.FindStringSubmatch(m)[1]) + `"`
			}))
		default:
			b.WriteString(rewrite(stripped))
		}
	}
	return b.String()
}

// lookupVariant returns the cached variant record for the request's
// {id}/{resolution}. On failure it writes the error response and reports
// false.
func lookupVariant(w http.ResponseWriter, r *http.Request) (variantRecord, bool) {
	v, ok, err := streamCaches.variant(r.PathValue("id"), r.PathValue("resolution"), time.Now())
	if err != nil {
		http.Error(w, "upstream fetch failed: "+err.Error(), http.StatusBadGateway)
		return variantRecord{}, false
	}
	if !ok {
		http.NotFound(w, r)
		return variantRecord{}, false
	}
	return v, true
}

func handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	resolution := r.PathValue("resolution")
	if resolution == "" {
		master, ok, err := streamCaches.master(id, time.Now())
		if err != nil {
			http.Error(w, "upstream fetch failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", hlsMimetype)
		io.WriteString(w, rewriteMasterPlaylist(master, id))
		return
	}
	variant, ok := lookupVariant(w, r)
	if !ok {
		return
	}
	// The variant media playlist is a live sliding window: kept in memory
	// only briefly (muxPlaylistTTL) so concurrent viewers share one
	// upstream poll, never cached to disk.
	e := streamMux.get("nos/"+id+"/"+resolution+"/playlist", variant.URL, muxPlaylistTTL, nil)
	media, err := e.waitDone()
	if err != nil || e.status != http.StatusOK {
		streamCaches.evict(id) // stream likely ended; re-resolve next time
		if err == nil {
			err = fmt.Errorf("status %d", e.status)
		}
		http.Error(w, "upstream fetch failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", hlsMimetype)
	io.WriteString(w, rewriteMediaPlaylist(string(media), e.finalURL, nosPath("proxy", id, resolution)+"/"))
}

// handleSegment serves segment bytes: from the in-memory mux (shared with
// everyone watching live), else from the disk cache (seek-back), else via
// a coalesced upstream fetch that is written through to disk on success.
func handleSegment(w http.ResponseWriter, r *http.Request) {
	variant, ok := lookupVariant(w, r)
	if !ok {
		return
	}
	base, err := url.Parse(variant.URL)
	if err != nil {
		http.Error(w, "upstream fetch failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	file := r.PathValue("file")
	key := "nos/" + r.PathValue("id") + "/" + r.PathValue("resolution") + "/" + file
	ref := file
	onDisk := r.URL.RawQuery == "" // querystring segments stay out of the disk cache
	if q := r.URL.RawQuery; q != "" {
		ref += "?" + q
		key += "?" + q
	}
	target := resolve(base, ref)
	if parsed, err := url.Parse(target); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || !hostAllowed(parsed.Hostname()) {
		http.Error(w, "upstream host not allowed", http.StatusForbidden)
		return
	}
	now := time.Now()
	if r.Header.Get("Range") != "" {
		if path, ok := diskCaches.open(key, now); ok && onDisk {
			watchers.touch(clientIP(r), now)
			serveSegmentFile(w, r, path)
			return
		}
		proxyUpstream(w, r, target) // ServeFile-less passthrough, honors Range
		return
	}
	watchers.touch(clientIP(r), now)
	if e := streamMux.peek(key); e != nil {
		serveMuxEntry(w, e)
		return
	}
	if onDisk {
		if path, ok := diskCaches.open(key, now); ok {
			serveSegmentFile(w, r, path)
			return
		}
	}
	var persist func([]byte)
	if onDisk {
		disk := diskCaches // captured now: persist runs on the fetch goroutine later
		persist = func(data []byte) { disk.put(key, data, time.Now()) }
	}
	serveMuxEntry(w, streamMux.get(key, target, memoryCacheTTL, persist))
}

// serveMuxEntry streams a mux entry to the client, following the shared
// buffer while the upstream fetch is still in progress.
func serveMuxEntry(w http.ResponseWriter, e *muxEntry) {
	if err := e.waitHeader(); err != nil {
		http.Error(w, "upstream fetch failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	ctype, _, _ := strings.Cut(e.contentType, ";")
	w.Header().Set("Content-Type", strings.TrimSpace(ctype))
	if length, ok := e.lengthIfDone(); ok {
		w.Header().Set("Content-Length", strconv.Itoa(length))
	}
	w.WriteHeader(e.status)
	e.serve(w)
}

// serveSegmentFile serves a disk-cached segment; http.ServeFile provides
// Range support, the content type comes from the extension.
func serveSegmentFile(w http.ResponseWriter, r *http.Request, path string) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ts":
		w.Header().Set("Content-Type", "video/MP2T")
	case ".mp4", ".m4s":
		w.Header().Set("Content-Type", "video/mp4")
	}
	http.ServeFile(w, r, path)
}

func handlePlayer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec, ok, err := streamCaches.stream(id, time.Now())
	if err != nil {
		http.Error(w, "upstream fetch failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	title := cmp.Or(rec.Title, "Stream")
	src := streamPath(id, r.PathValue("resolution"))
	render(w, "player.html", struct{ Title, Src string }{title, src})
}
