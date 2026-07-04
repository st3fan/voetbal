package main

import (
	"cmp"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// Short stream URLs. A stream is addressed by its NOS id and optionally a
// variant resolution; the upstream URL is re-resolved from the NOS API on
// every request, so there is no state and an id or resolution that no
// longer exists is a 404.
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

func findStream(streams []Stream, id string) (Stream, bool) {
	i := slices.IndexFunc(streams, func(s Stream) bool { return s.ID == id })
	if i < 0 {
		return Stream{}, false
	}
	return streams[i], true
}

// variantForResolution returns the URL of the first (highest-bandwidth)
// variant with exactly the requested resolution, e.g. "1920x1080".
func variantForResolution(variants []Variant, resolution string) (string, bool) {
	i := slices.IndexFunc(variants, func(v Variant) bool { return v.Resolution == resolution })
	if i < 0 {
		return "", false
	}
	return variants[i].URL, true
}

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

// lookupStream finds the requested {id} in the live stream list. On failure
// it writes the error response and reports false.
func lookupStream(w http.ResponseWriter, r *http.Request) (Stream, bool) {
	streams, err := fetchStreams()
	if err != nil {
		http.Error(w, "upstream fetch failed: "+err.Error(), http.StatusBadGateway)
		return Stream{}, false
	}
	stream, ok := findStream(streams, r.PathValue("id"))
	if !ok || streamURL(stream) == "" {
		http.NotFound(w, r)
		return Stream{}, false
	}
	return stream, true
}

func handleStream(w http.ResponseWriter, r *http.Request) {
	stream, ok := lookupStream(w, r)
	if !ok {
		return
	}
	masterURL, master, err := fetchPlaylist(streamURL(stream))
	if err != nil {
		http.Error(w, "upstream fetch failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	resolution := r.PathValue("resolution")
	if resolution == "" {
		w.Header().Set("Content-Type", hlsMimetype)
		io.WriteString(w, rewriteMasterPlaylist(master, stream.ID))
		return
	}
	variantURL, ok := variantForResolution(parseVariants(master, masterURL), resolution)
	if !ok {
		http.NotFound(w, r)
		return
	}
	finalURL, media, err := fetchPlaylist(variantURL)
	if err != nil {
		http.Error(w, "upstream fetch failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	base, err := url.Parse(finalURL)
	if err != nil {
		http.Error(w, "upstream fetch failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", hlsMimetype)
	io.WriteString(w, rewriteMediaPlaylist(media, base, nosPath("proxy", stream.ID, resolution)+"/"))
}

func handleSegment(w http.ResponseWriter, r *http.Request) {
	stream, ok := lookupStream(w, r)
	if !ok {
		return
	}
	masterURL, master, err := fetchPlaylist(streamURL(stream))
	if err != nil {
		http.Error(w, "upstream fetch failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	variantURL, ok := variantForResolution(parseVariants(master, masterURL), r.PathValue("resolution"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	base, err := url.Parse(variantURL)
	if err != nil {
		http.Error(w, "upstream fetch failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	file := r.PathValue("file")
	if q := r.URL.RawQuery; q != "" {
		file += "?" + q
	}
	proxyUpstream(w, r, resolve(base, file))
}

func handlePlayer(w http.ResponseWriter, r *http.Request) {
	stream, ok := lookupStream(w, r)
	if !ok {
		return
	}
	title := cmp.Or(stream.Title, "Stream")
	src := streamPath(stream.ID, r.PathValue("resolution"))
	render(w, "player.html", struct{ Title, Src string }{title, src})
}
