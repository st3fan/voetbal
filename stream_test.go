package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestStreamAndPlayerPath(t *testing.T) {
	tests := []struct {
		id, resolution, wantStream, wantPlayer string
	}{
		{"2616266", "1920x1080", "/stream/nos/2616266/1920x1080", "/player/nos/2616266/1920x1080"},
		{"2616266", "", "/stream/nos/2616266", "/player/nos/2616266"},
		{"weird/id", "960x540", "/stream/nos/weird%2Fid/960x540", "/player/nos/weird%2Fid/960x540"},
	}
	for _, tt := range tests {
		if got := streamPath(tt.id, tt.resolution); got != tt.wantStream {
			t.Errorf("streamPath(%q, %q) = %q, want %q", tt.id, tt.resolution, got, tt.wantStream)
		}
		if got := playerPath(tt.id, tt.resolution); got != tt.wantPlayer {
			t.Errorf("playerPath(%q, %q) = %q, want %q", tt.id, tt.resolution, got, tt.wantPlayer)
		}
	}
}

// Mirrors the shape of the real NOS master playlist (Unified Streaming).
const testMaster = `#EXTM3U
#EXT-X-VERSION:4
## Created with Unified Streaming Platform

# AUDIO groups
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio-aacl-256",NAME="English",DEFAULT=YES,CHANNELS="2"

# variants
#EXT-X-STREAM-INF:BANDWIDTH=882000,RESOLUTION=640x360,AUDIO="audio-aacl-256"
event-audio=256000-video=499968.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=4380000,RESOLUTION=1920x1080,AUDIO="audio-aacl-256"
event-audio=256000-video=3499968.m3u8

# keyframes
#EXT-X-I-FRAME-STREAM-INF:BANDWIDTH=73000,RESOLUTION=640x360,URI="keyframes/event-video=499968.m3u8"
`

func TestRewriteMasterPlaylist(t *testing.T) {
	got := rewriteMasterPlaylist(testMaster, "2616266")

	if strings.Contains(got, "I-FRAME") {
		t.Errorf("trick-play lines not dropped:\n%s", got)
	}
	if strings.Contains(got, ".m3u8") {
		t.Errorf("upstream variant URI leaked:\n%s", got)
	}
	for _, want := range []string{
		"#EXTM3U",
		"#EXT-X-MEDIA:TYPE=AUDIO",
		"#EXT-X-STREAM-INF:BANDWIDTH=882000,RESOLUTION=640x360",
		"\n/stream/nos/2616266/640x360\n",
		"\n/stream/nos/2616266/1920x1080\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestRewriteMasterPlaylistDropsVariantsWithoutResolution(t *testing.T) {
	in := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=96000,CODECS=\"mp4a.40.2\"\naudio-only.m3u8\n"
	got := rewriteMasterPlaylist(in, "1")
	if strings.Contains(got, "audio-only") || strings.Contains(got, "STREAM-INF") {
		t.Errorf("resolution-less variant not dropped:\n%s", got)
	}
}

func TestRewriteMediaPlaylist(t *testing.T) {
	base, _ := url.Parse("https://cdn.example.com/jwt-token/live/event/variant.m3u8")
	in := `#EXTM3U
#EXT-X-TARGETDURATION:8
#EXT-X-MAP:URI="init.mp4"
#EXTINF:7.68, no desc
seg-1.ts
#EXTINF:7.68, no desc
sub/dir/seg-2.ts?token=abc
## Media sequence discontinuity
#EXT-X-GAP
#EXTINF:0, no desc
https://cdn.example.com/jwt-token/live/event/seg-3.ts
https://other.example.com/elsewhere/seg-4.ts`

	got := rewriteMediaPlaylist(in, base, "/proxy/nos/2616266/848x480/")
	lines := strings.Split(got, "\n")

	want := []string{
		"#EXTM3U",
		"#EXT-X-TARGETDURATION:8",
		`#EXT-X-MAP:URI="/proxy/nos/2616266/848x480/init.mp4"`,
		"#EXTINF:7.68, no desc",
		"/proxy/nos/2616266/848x480/seg-1.ts",
		"#EXTINF:7.68, no desc",
		"/proxy/nos/2616266/848x480/sub/dir/seg-2.ts?token=abc",
		"## Media sequence discontinuity",
		"#EXT-X-GAP",
		"#EXTINF:0, no desc",
		"/proxy/nos/2616266/848x480/seg-3.ts",
		proxyPath("https://other.example.com/elsewhere/seg-4.ts"),
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
