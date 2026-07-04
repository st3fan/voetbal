package main

import (
	"strings"
	"testing"
)

func TestM3UAttr(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"NOS Livestream", "NOS Livestream"},
		{`say "hi"`, "say 'hi'"},
		{"line\nbreak", "line break"},
		{"cr\r\nlf", "cr  lf"},
	}
	for _, tt := range tests {
		if got := m3uAttr(tt.in); got != tt.want {
			t.Errorf("m3uAttr(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPlaylistEntries(t *testing.T) {
	streams := []Stream{
		{
			Title:   "Stream A",
			Formats: []Format{{Mimetype: hlsMimetype, URL: "https://cdn.nos.nl/a/master.m3u8"}},
		},
		{
			Title:   "Stream B",
			Formats: []Format{{Mimetype: hlsMimetype, URL: "https://cdn.nos.nl/b/master.m3u8"}},
		},
		{
			Title: "No URL", // no formats: should be skipped
		},
	}
	streams[0].IndexImage.Ratio16x9 = []Image{{Width: 1280, URL: "https://cdn.nos.nl/a/thumb.jpg"}}

	// Stream A has qualities (ascending, as streamQualities returns them);
	// stream B has none and should fall back to its master URL.
	qualities := func(s Stream) []Quality {
		if s.Title != "Stream A" {
			return nil
		}
		return []Quality{
			{Label: "540p", URL: "https://cdn.nos.nl/a/540.m3u8", Height: 540},
			{Label: "1080p", URL: "https://cdn.nos.nl/a/1080.m3u8", Height: 1080},
		}
	}

	got := playlistEntries(streams, qualities)
	want := []m3uEntry{
		{Name: "Stream A (1080p)", Logo: "https://cdn.nos.nl/a/thumb.jpg", URL: "https://cdn.nos.nl/a/1080.m3u8"},
		{Name: "Stream A (540p)", Logo: "https://cdn.nos.nl/a/thumb.jpg", URL: "https://cdn.nos.nl/a/540.m3u8"},
		{Name: "Stream B", URL: "https://cdn.nos.nl/b/master.m3u8"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestPlaylistEntriesNoTitle(t *testing.T) {
	streams := []Stream{{Formats: []Format{{Mimetype: hlsMimetype, URL: "https://cdn.nos.nl/x.m3u8"}}}}
	entries := playlistEntries(streams, func(Stream) []Quality { return nil })
	if len(entries) != 1 || entries[0].Name != "(no title)" {
		t.Errorf("expected single (no title) entry, got %+v", entries)
	}
}

func TestBuildM3U(t *testing.T) {
	entries := []m3uEntry{
		{Name: "Stream A (1080p)", Logo: "https://cdn.nos.nl/a/thumb.jpg", URL: "https://cdn.nos.nl/a/1080.m3u8"},
		{Name: `Voetbal "Extra"`, URL: "https://cdn.nos.nl/b/master.m3u8"},
	}

	got := buildM3U("http://example.com:8000", entries)
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")

	want := []string{
		"#EXTM3U",
		`#EXTINF:-1 tvg-name="Stream A (1080p)" tvg-logo="https://cdn.nos.nl/a/thumb.jpg" group-title="NOS",Stream A (1080p)`,
		"http://example.com:8000" + proxyPath("https://cdn.nos.nl/a/1080.m3u8"),
		`#EXTINF:-1 tvg-name="Voetbal 'Extra'" group-title="NOS",Voetbal 'Extra'`,
		"http://example.com:8000" + proxyPath("https://cdn.nos.nl/b/master.m3u8"),
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

func TestBuildM3UEmpty(t *testing.T) {
	if got := buildM3U("http://example.com", nil); got != "#EXTM3U\n" {
		t.Errorf("buildM3U with no entries = %q, want %q", got, "#EXTM3U\n")
	}
}
