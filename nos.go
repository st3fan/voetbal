package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	apiURL      = "https://nos.nl/api/live-livestreams"
	hlsMimetype = "application/vnd.apple.mpegurl"
	userAgent   = "voetbal/0.1"
)

var httpClient = &http.Client{
	Transport: loggingTransport{base: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		ResponseHeaderTimeout: 15 * time.Second,
		MaxIdleConnsPerHost:   8,
	}},
}

type Image struct {
	Width int    `json:"width"`
	URL   string `json:"url"`
}

type Format struct {
	Mimetype string `json:"mimetype"`
	URL      string `json:"url"`
}

type Stream struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	IsOnline     bool     `json:"isOnline"`
	AllowedAreas []string `json:"allowedAreas"`
	Formats      []Format `json:"formats"`
	IndexImage   struct {
		Ratio16x9 []Image `json:"Ratio16x9"`
	} `json:"indexImage"`
}

type Variant struct {
	Resolution string
	Bandwidth  int
	URL        string
}

type Quality struct {
	Label      string
	URL        string
	Resolution string
	Height     int
}

func get(u string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	return resp, nil
}

func fetchStreams() ([]Stream, error) {
	resp, err := get(apiURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var streams []Stream
	if err := json.NewDecoder(resp.Body).Decode(&streams); err != nil {
		return nil, err
	}
	return streams, nil
}

func fetchPlaylist(u string) (string, string, error) {
	resp, err := get(u)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return "", "", err
	}
	return resp.Request.URL.String(), string(body), nil
}

func streamURL(stream Stream) string {
	for _, f := range stream.Formats {
		if f.Mimetype == hlsMimetype && f.URL != "" {
			return f.URL
		}
	}
	for _, f := range stream.Formats {
		if f.URL != "" {
			return f.URL
		}
	}
	return ""
}

var (
	resolutionAttr = regexp.MustCompile(`(?:^|[:,])RESOLUTION="?(\d+x\d+)`)
	bandwidthAttr  = regexp.MustCompile(`(?:^|[:,])BANDWIDTH="?(\d+)`)
)

func attr(re *regexp.Regexp, line string) string {
	if m := re.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return ""
}

func parseVariants(playlist, baseURL string) []Variant {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil
	}
	lines := strings.Split(playlist, "\n")
	var variants []Variant
	for i, line := range lines {
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			continue
		}
		var uri string
		for _, next := range lines[i+1:] {
			if s := strings.TrimSpace(next); s != "" && !strings.HasPrefix(s, "#") {
				uri = s
				break
			}
		}
		if uri == "" {
			continue
		}
		bandwidth, _ := strconv.Atoi(attr(bandwidthAttr, line))
		variants = append(variants, Variant{
			Resolution: attr(resolutionAttr, line),
			Bandwidth:  bandwidth,
			URL:        resolve(base, uri),
		})
	}
	slices.SortFunc(variants, func(a, b Variant) int { return cmp.Compare(b.Bandwidth, a.Bandwidth) })
	return variants
}

func regionLabel(stream Stream) string {
	if len(stream.AllowedAreas) == 0 {
		return "worldwide"
	}
	return "geo-locked: " + strings.Join(stream.AllowedAreas, ", ")
}

func thumbnailURL(stream Stream, targetWidth int) string {
	images := slices.Clone(stream.IndexImage.Ratio16x9)
	if len(images) == 0 {
		return ""
	}
	slices.SortFunc(images, func(a, b Image) int { return cmp.Compare(a.Width, b.Width) })
	for _, image := range images {
		if image.Width >= targetWidth {
			return image.URL
		}
	}
	return images[len(images)-1].URL
}

func streamQualities(stream Stream) []Quality {
	resolver := streamURL(stream)
	if resolver == "" {
		return nil
	}
	masterURL, playlist, err := fetchPlaylist(resolver)
	if err != nil {
		return nil
	}
	var qualities []Quality
	for _, v := range parseVariants(playlist, masterURL) {
		_, h, ok := strings.Cut(v.Resolution, "x")
		if !ok || v.URL == "" {
			continue
		}
		height, err := strconv.Atoi(h)
		if err != nil {
			continue
		}
		qualities = append(qualities, Quality{Label: fmt.Sprintf("%dp", height), URL: v.URL, Resolution: v.Resolution, Height: height})
	}
	slices.SortFunc(qualities, func(a, b Quality) int { return cmp.Compare(a.Height, b.Height) })
	return qualities
}
