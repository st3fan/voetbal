package main

import (
	"cmp"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	watcherWindow    = 30 * time.Second
	watcherIdleLimit = 10 * time.Minute
)

type watcher struct {
	FirstSeen time.Time
	LastSeen  time.Time
	Requests  int64
}

type watcherView struct {
	IP       string
	Since    string
	AgoSecs  int
	Requests int64
}

type watcherTracker struct {
	mu   sync.Mutex
	byIP map[string]*watcher
}

func newWatcherTracker() *watcherTracker {
	return &watcherTracker{byIP: make(map[string]*watcher)}
}

var watchers = newWatcherTracker()

func (t *watcherTracker) touch(ip string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	maps.DeleteFunc(t.byIP, func(_ string, w *watcher) bool {
		return now.Sub(w.LastSeen) > watcherIdleLimit
	})
	w := t.byIP[ip]
	if w == nil {
		w = &watcher{FirstSeen: now}
		t.byIP[ip] = w
	}
	w.LastSeen = now
	w.Requests++
}

func (t *watcherTracker) active(now time.Time, window time.Duration) []watcherView {
	t.mu.Lock()
	defer t.mu.Unlock()
	type entry struct {
		ip string
		w  watcher
	}
	var entries []entry
	for ip, w := range t.byIP {
		if now.Sub(w.LastSeen) <= window {
			entries = append(entries, entry{ip, *w})
		}
	}
	slices.SortFunc(entries, func(a, b entry) int { return a.w.FirstSeen.Compare(b.w.FirstSeen) })
	views := make([]watcherView, len(entries))
	for i, e := range entries {
		views[i] = watcherView{
			IP:       anonymizeIP(e.ip),
			Since:    e.w.FirstSeen.Format("15:04:05"),
			AgoSecs:  int(now.Sub(e.w.LastSeen).Seconds()),
			Requests: e.w.Requests,
		}
	}
	return views
}

func anonymizeIP(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return "unknown"
	}
	addr = addr.Unmap()
	if addr.Is4() {
		b := addr.As4()
		return fmt.Sprintf("%d.%d.x.x", b[0], b[1])
	}
	groups := strings.Split(addr.StringExpanded(), ":")
	trim := func(g string) string { return cmp.Or(strings.TrimLeft(g, "0"), "0") }
	return trim(groups[0]) + ":" + trim(groups[1]) + ":x:x"
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func handleWatchers(w http.ResponseWriter, r *http.Request) {
	render(w, "watchers.html", struct{ Watchers []watcherView }{watchers.active(time.Now(), watcherWindow)})
}
