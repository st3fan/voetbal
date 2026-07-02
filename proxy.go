package main

import (
	"cmp"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

var proxyAllowedHosts = []string{
	".cdn.nos.nl",
	".streamgate.nl",
	".streamgate.io",
	".cdn.bcms.kpn.com",
	"resolver.streaming.api.nos.nl",
	"prod.npoplayer.nl",
}

var playlistContentTypes = []string{
	"application/vnd.apple.mpegurl",
	"application/x-mpegurl",
	"audio/mpegurl",
	"application/mpegurl",
}

var uriAttr = regexp.MustCompile(`URI="([^"]*)"`)

func hostAllowed(host string) bool {
	host = strings.ToLower(host)
	return slices.ContainsFunc(proxyAllowedHosts, func(h string) bool {
		return host == h || strings.HasSuffix(host, h)
	})
}

func proxyPath(u string) string {
	return "/proxy?" + url.Values{"url": {u}}.Encode()
}

func isPlaylist(contentType string, u *url.URL) bool {
	ctype, _, _ := strings.Cut(contentType, ";")
	if slices.Contains(playlistContentTypes, strings.ToLower(strings.TrimSpace(ctype))) {
		return true
	}
	return strings.HasSuffix(strings.ToLower(u.Path), ".m3u8")
}

func resolve(base *url.URL, ref string) string {
	parsed, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return base.ResolveReference(parsed).String()
}

func rewritePlaylist(text string, base *url.URL) string {
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
				uri := uriAttr.FindStringSubmatch(m)[1]
				return `URI="` + proxyPath(resolve(base, uri)) + `"`
			}))
		default:
			b.WriteString(proxyPath(resolve(base, stripped)))
		}
	}
	return b.String()
}

func handleProxy(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("url")
	parsed, err := url.Parse(target)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || !hostAllowed(parsed.Hostname()) {
		http.Error(w, "upstream host not allowed", http.StatusForbidden)
		return
	}
	watchers.touch(clientIP(r), time.Now())
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		http.Error(w, "invalid upstream url", http.StatusBadRequest)
		return
	}
	req.Header.Set("User-Agent", userAgent)
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		http.Error(w, "upstream fetch failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	contentType := cmp.Or(resp.Header.Get("Content-Type"), "application/octet-stream")
	if resp.StatusCode < 400 && isPlaylist(contentType, resp.Request.URL) {
		body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
		if err != nil {
			http.Error(w, "upstream read failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", hlsMimetype)
		io.WriteString(w, rewritePlaylist(string(body), resp.Request.URL))
		return
	}

	for _, name := range []string{"Content-Length", "Content-Range", "Accept-Ranges"} {
		if v := resp.Header.Get(name); v != "" {
			w.Header().Set(name, v)
		}
	}
	ctype, _, _ := strings.Cut(contentType, ";")
	w.Header().Set("Content-Type", strings.TrimSpace(ctype))
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}
