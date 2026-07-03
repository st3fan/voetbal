package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

type accessLock interface {
	allow(r *http.Request) bool
}

func lockMiddleware(locks []accessLock, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, l := range locks {
			if l.allow(r) {
				next.ServeHTTP(w, r)
				return
			}
		}
		http.Error(w, "not available from your location", http.StatusForbidden)
	})
}

func parseNetworkLock(s string) (prefixes []netip.Prefix, asns []string, err error) {
	for entry := range strings.SplitSeq(s, ",") {
		entry = strings.TrimSpace(entry)
		switch upper := strings.ToUpper(entry); {
		case entry == "":
		case strings.HasPrefix(upper, "ASN"), strings.HasPrefix(upper, "AS"):
			num := strings.TrimPrefix(strings.TrimPrefix(upper, "AS"), "N")
			if num == "" || strings.ContainsFunc(num, func(r rune) bool { return r < '0' || r > '9' }) {
				return nil, nil, fmt.Errorf("invalid ASN %q", entry)
			}
			asns = append(asns, num)
		case strings.Contains(entry, "/"):
			prefix, err := netip.ParsePrefix(entry)
			if err != nil {
				return nil, nil, err
			}
			prefixes = append(prefixes, prefix.Masked())
		default:
			addr, err := netip.ParseAddr(entry)
			if err != nil {
				return nil, nil, err
			}
			prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
		}
	}
	return prefixes, asns, nil
}

func ripePrefixesURL(asn string) string {
	return "https://stat.ripe.net/data/announced-prefixes/data.json?sourceapp=voetbal&resource=" + url.QueryEscape("AS"+asn)
}

func fetchASNPrefixes(u string) ([]netip.Prefix, error) {
	resp, err := get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var payload struct {
		Data struct {
			Prefixes []struct {
				Prefix string `json:"prefix"`
			} `json:"prefixes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	prefixes := make([]netip.Prefix, 0, len(payload.Data.Prefixes))
	for _, p := range payload.Data.Prefixes {
		prefix, err := netip.ParsePrefix(p.Prefix)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

type networkLock struct {
	prefixes []netip.Prefix
}

func (l *networkLock) allow(r *http.Request) bool {
	addr, err := netip.ParseAddr(clientIP(r))
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() {
		return true
	}
	for _, prefix := range l.prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
