package main

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"testing"
)

func TestParseNetworkLock(t *testing.T) {
	tests := []struct {
		in       string
		prefixes []string
		asns     []string
	}{
		{"", nil, nil},
		{" , ", nil, nil},
		{"1.1.1.1", []string{"1.1.1.1/32"}, nil},
		{"192.168.0.0/16", []string{"192.168.0.0/16"}, nil},
		{"192.168.1.5/16", []string{"192.168.0.0/16"}, nil},
		{"2001:db8::1", []string{"2001:db8::1/128"}, nil},
		{"ASN577", nil, []string{"577"}},
		{"AS577", nil, []string{"577"}},
		{"asn577", nil, []string{"577"}},
		{"192.168.0.0/16,1.1.1.1,ASN577", []string{"192.168.0.0/16", "1.1.1.1/32"}, []string{"577"}},
		{" 1.1.1.1 , asn12 ", []string{"1.1.1.1/32"}, []string{"12"}},
	}
	for _, tt := range tests {
		prefixes, asns, err := parseNetworkLock(tt.in)
		if err != nil {
			t.Errorf("parseNetworkLock(%q): %v", tt.in, err)
			continue
		}
		var got []string
		for _, p := range prefixes {
			got = append(got, p.String())
		}
		if !slices.Equal(got, tt.prefixes) {
			t.Errorf("parseNetworkLock(%q) prefixes = %v, want %v", tt.in, got, tt.prefixes)
		}
		if !slices.Equal(asns, tt.asns) {
			t.Errorf("parseNetworkLock(%q) asns = %v, want %v", tt.in, asns, tt.asns)
		}
	}
}

func TestParseNetworkLockInvalid(t *testing.T) {
	for _, in := range []string{"garbage", "1.2.3.4/40", "1.2.3", "ASN", "AS12x", "ASNAS12"} {
		if _, _, err := parseNetworkLock(in); err == nil {
			t.Errorf("parseNetworkLock(%q): expected error", in)
		}
	}
}

func TestRipePrefixesURL(t *testing.T) {
	want := "https://stat.ripe.net/data/announced-prefixes/data.json?sourceapp=voetbal&resource=AS577"
	if got := ripePrefixesURL("577"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFetchASNPrefixes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"prefixes":[{"prefix":"142.166.0.0/16"},{"prefix":"2620:f3::/48"}]}}`))
	}))
	defer srv.Close()

	got, err := fetchASNPrefixes(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"142.166.0.0/16", "2620:f3::/48"}
	var gotStrs []string
	for _, p := range got {
		gotStrs = append(gotStrs, p.String())
	}
	if !slices.Equal(gotStrs, want) {
		t.Errorf("got %v, want %v", gotStrs, want)
	}
}

func TestFetchASNPrefixesErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"http error", http.NotFound},
		{"bad json", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("not json")) }},
		{"bad prefix", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"data":{"prefixes":[{"prefix":"garbage"}]}}`))
		}},
	}
	for _, tt := range tests {
		srv := httptest.NewServer(tt.handler)
		if _, err := fetchASNPrefixes(srv.URL); err == nil {
			t.Errorf("%s: expected error", tt.name)
		}
		srv.Close()
	}
}

func TestNetworkLockAllow(t *testing.T) {
	l := &networkLock{prefixes: []netip.Prefix{
		netip.MustParsePrefix("142.166.0.0/16"),
		netip.MustParsePrefix("1.1.1.1/32"),
	}}
	tests := []struct {
		name   string
		remote string
		want   bool
	}{
		{"loopback", "127.0.0.1:1234", true},
		{"private", "10.0.0.1:1234", true},
		{"in prefix", "142.166.4.5:1234", true},
		{"exact address", "1.1.1.1:1234", true},
		{"outside", "8.8.8.8:1234", false},
		{"near miss", "1.1.1.2:1234", false},
		{"bad remote addr", "garbage", false},
	}
	for _, tt := range tests {
		if got := l.allow(requestFrom(tt.remote)); got != tt.want {
			t.Errorf("%s: allow = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestLockMiddlewareEitherPasses(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	network := &networkLock{prefixes: []netip.Prefix{netip.MustParsePrefix("142.166.0.0/16")}}
	region := &regionLock{allowed: parseRegionLock("US"), lookup: &stubLookup{code: "CA"}}
	handler := lockMiddleware([]accessLock{network, region}, next)

	// Network lock passes, region lock would deny.
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, requestFrom("142.166.4.5:1234"))
	if w.Code != http.StatusOK {
		t.Errorf("network match: got %d, want 200", w.Code)
	}

	// Network lock denies, region lock passes.
	region.lookup = &stubLookup{code: "US"}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, requestFrom("8.8.8.8:1234"))
	if w.Code != http.StatusOK {
		t.Errorf("region match: got %d, want 200", w.Code)
	}

	// Both deny.
	region.lookup = &stubLookup{code: "CA"}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, requestFrom("8.8.8.8:1234"))
	if w.Code != http.StatusForbidden {
		t.Errorf("both deny: got %d, want 403", w.Code)
	}
}
