package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientIP(t *testing.T) {
	tests := []struct {
		name    string
		remote  string
		headers map[string]string
		want    string
	}{
		{"public remote no headers", "203.0.113.7:1234", nil, "203.0.113.7"},
		{"remote without port", "203.0.113.7", nil, "203.0.113.7"},
		{"public remote ignores xff", "203.0.113.7:1234", map[string]string{"X-Forwarded-For": "8.8.8.8"}, "203.0.113.7"},
		{"private remote single xff", "192.168.1.1:1234", map[string]string{"X-Forwarded-For": "8.8.8.8"}, "8.8.8.8"},
		{"private remote takes last xff", "192.168.1.1:1234", map[string]string{"X-Forwarded-For": "8.8.8.8, 203.0.113.9"}, "203.0.113.9"},
		{"real ip ignored", "192.168.1.1:1234", map[string]string{"X-Real-IP": "8.8.8.8"}, "192.168.1.1"},
		{"loopback v6 remote", "[::1]:1234", map[string]string{"X-Forwarded-For": "8.8.8.8"}, "8.8.8.8"},
		{"private remote no headers", "10.0.0.5:1234", nil, "10.0.0.5"},
	}
	for _, tt := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tt.remote
		for k, v := range tt.headers {
			r.Header.Set(k, v)
		}
		if got := clientIP(r); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestAnonymizeIP(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"45.76.32.237", "45.76.x.x"},
		{"127.0.0.1", "127.0.x.x"},
		{"2001:db8:1:2::3", "2001:db8:x:x"},
		{"::1", "0:0:x:x"},
		{"::ffff:10.1.2.3", "10.1.x.x"},
		{"not-an-ip", "unknown"},
		{"", "unknown"},
	}
	for _, tt := range tests {
		if got := anonymizeIP(tt.in); got != tt.want {
			t.Errorf("anonymizeIP(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestWatcherTrackerTouch(t *testing.T) {
	tracker := newWatcherTracker()
	now := time.Now()
	tracker.touch("203.0.113.7", now)
	tracker.touch("203.0.113.7", now.Add(5*time.Second))

	views := tracker.active(now.Add(5*time.Second), watcherWindow)
	if len(views) != 1 {
		t.Fatalf("got %d watchers, want 1", len(views))
	}
	if views[0].IP != "203.0.x.x" {
		t.Errorf("IP not anonymized: %q", views[0].IP)
	}
	if views[0].Requests != 2 {
		t.Errorf("got %d requests, want 2", views[0].Requests)
	}
	if views[0].Since != now.Format("15:04:05") {
		t.Errorf("FirstSeen changed: got %q, want %q", views[0].Since, now.Format("15:04:05"))
	}
	if views[0].AgoSecs != 0 {
		t.Errorf("got AgoSecs %d, want 0", views[0].AgoSecs)
	}
}

func TestWatcherTrackerActiveWindow(t *testing.T) {
	tracker := newWatcherTracker()
	now := time.Now()
	tracker.touch("203.0.113.7", now)
	tracker.touch("198.51.100.9", now.Add(time.Minute))

	views := tracker.active(now.Add(time.Minute), watcherWindow)
	if len(views) != 1 || views[0].IP != "198.51.x.x" {
		t.Fatalf("got %+v, want only 198.51.x.x", views)
	}
}

func TestWatcherTrackerPrune(t *testing.T) {
	tracker := newWatcherTracker()
	now := time.Now()
	tracker.touch("203.0.113.7", now)
	tracker.touch("198.51.100.9", now.Add(watcherIdleLimit+time.Second))

	if len(tracker.byIP) != 1 {
		t.Fatalf("got %d entries, want idle entry pruned", len(tracker.byIP))
	}
	if _, ok := tracker.byIP["198.51.100.9"]; !ok {
		t.Error("fresh entry missing after prune")
	}
}
