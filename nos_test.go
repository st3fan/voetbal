package main

import (
	"slices"
	"testing"
)

func TestParseVariants(t *testing.T) {
	playlist := `#EXTM3U
#EXT-X-STREAM-INF:BANDWIDTH=1400000,RESOLUTION=960x540,FRAME-RATE=25
mid/index.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=3500000,RESOLUTION=1280x720
https://other.example.com/high.m3u8
#EXT-X-STREAM-INF:BANDWIDTH=700000
low/index.m3u8
`
	variants := parseVariants(playlist, "https://cdn.example.com/live/master.m3u8")
	if len(variants) != 3 {
		t.Fatalf("got %d variants, want 3", len(variants))
	}
	if variants[0].URL != "https://other.example.com/high.m3u8" || variants[0].Resolution != "1280x720" {
		t.Errorf("unexpected first variant: %+v", variants[0])
	}
	if variants[1].URL != "https://cdn.example.com/live/mid/index.m3u8" {
		t.Errorf("relative URL not resolved: %q", variants[1].URL)
	}
	bandwidths := []int{variants[0].Bandwidth, variants[1].Bandwidth, variants[2].Bandwidth}
	if !slices.IsSorted(slices.Clone(bandwidths)) && !slices.Equal(bandwidths, []int{3500000, 1400000, 700000}) {
		t.Errorf("not sorted by bandwidth desc: %v", bandwidths)
	}
	if variants[2].Resolution != "" {
		t.Errorf("expected empty resolution, got %q", variants[2].Resolution)
	}
}

func TestStreamURL(t *testing.T) {
	hls := Format{Mimetype: hlsMimetype, URL: "https://a/hls.m3u8"}
	dash := Format{Mimetype: "application/dash+xml", URL: "https://a/dash.mpd"}
	tests := []struct {
		name    string
		formats []Format
		want    string
	}{
		{"prefers hls", []Format{dash, hls}, hls.URL},
		{"falls back to any", []Format{dash}, dash.URL},
		{"skips empty urls", []Format{{Mimetype: hlsMimetype}, dash}, dash.URL},
		{"none", nil, ""},
	}
	for _, tt := range tests {
		if got := streamURL(Stream{Formats: tt.formats}); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestRegionLabel(t *testing.T) {
	if got := regionLabel(Stream{}); got != "worldwide" {
		t.Errorf("got %q, want worldwide", got)
	}
	if got := regionLabel(Stream{AllowedAreas: []string{"nl", "be"}}); got != "geo-locked: nl, be" {
		t.Errorf("got %q", got)
	}
}

func TestThumbnailURL(t *testing.T) {
	var stream Stream
	if got := thumbnailURL(stream, 640); got != "" {
		t.Errorf("got %q, want empty", got)
	}
	stream.IndexImage.Ratio16x9 = []Image{
		{Width: 1280, URL: "w1280"},
		{Width: 320, URL: "w320"},
		{Width: 640, URL: "w640"},
	}
	if got := thumbnailURL(stream, 640); got != "w640" {
		t.Errorf("got %q, want w640", got)
	}
	if got := thumbnailURL(stream, 2000); got != "w1280" {
		t.Errorf("got %q, want w1280", got)
	}
}
