package main

import (
	"cmp"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// m3uAttr sanitizes s for use inside a quoted #EXTINF attribute value.
var m3uAttr = strings.NewReplacer("\"", "'", "\r", " ", "\n", " ").Replace

type m3uEntry struct {
	Name string
	Logo string
	Path string
}

// playlistEntries expands streams into playlist entries, one per quality
// (highest first) so players offer explicit quality selection. Streams whose
// qualities cannot be determined get a single entry with the master playlist,
// which plays adaptively.
func playlistEntries(streams []Stream, qualities func(Stream) []Quality) []m3uEntry {
	entries := make([][]m3uEntry, len(streams))
	var wg sync.WaitGroup
	for i, stream := range streams {
		wg.Go(func() {
			u := streamURL(stream)
			if u == "" {
				return
			}
			title := cmp.Or(stream.Title, "(no title)")
			logo := thumbnailURL(stream, 640)
			qs := qualities(stream)
			if len(qs) == 0 {
				path := streamPath(stream.ID, "")
				if stream.ID == "" {
					path = proxyPath(u)
				}
				entries[i] = []m3uEntry{{Name: title, Logo: logo, Path: path}}
				return
			}
			for _, q := range slices.Backward(qs) {
				path := streamPath(stream.ID, q.Resolution)
				if stream.ID == "" || q.Resolution == "" {
					path = proxyPath(q.URL)
				}
				entries[i] = append(entries[i], m3uEntry{
					Name: fmt.Sprintf("%s (%s)", title, q.Label),
					Logo: logo,
					Path: path,
				})
			}
		})
	}
	wg.Wait()
	return slices.Concat(entries...)
}

// buildM3U renders an extended M3U playlist with one channel per entry,
// each pointing at the proxied stream URL.
func buildM3U(baseURL string, entries []m3uEntry) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	for _, e := range entries {
		name := m3uAttr(e.Name)
		fmt.Fprintf(&b, "#EXTINF:-1 tvg-name=\"%s\"", name)
		if e.Logo != "" {
			fmt.Fprintf(&b, " tvg-logo=\"%s\"", m3uAttr(e.Logo))
		}
		fmt.Fprintf(&b, " group-title=\"NOS\",%s\n", name)
		b.WriteString(baseURL + e.Path + "\n")
	}
	return b.String()
}

func handlePlaylist(w http.ResponseWriter, r *http.Request) {
	streams, err := fetchStreams()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	fmt.Fprint(w, buildM3U(requestBaseURL(r), playlistEntries(streams, streamQualities)))
}
