package main

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oschwald/geoip2-golang/v2"
)

const (
	geoDBDir    = "/data"
	geoDBName   = "dbip-country-lite.mmdb"
	geoDBMaxAge = 7 * 24 * time.Hour
)

func parseRegionLock(s string) map[string]bool {
	allowed := make(map[string]bool)
	for code := range strings.SplitSeq(s, ",") {
		if code = strings.ToUpper(strings.TrimSpace(code)); code != "" {
			allowed[code] = true
		}
	}
	return allowed
}

func geoDBURL(t time.Time) string {
	return fmt.Sprintf("https://download.db-ip.com/free/dbip-country-lite-%s.mmdb.gz", t.Format("2006-01"))
}

func downloadGeoDB(url, dest string) error {
	resp, err := get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return err
	}
	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, gz); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

func ensureGeoDB(dir string, urlFor func(time.Time) string) (string, error) {
	path := filepath.Join(dir, geoDBName)
	info, statErr := os.Stat(path)
	if statErr == nil && time.Since(info.ModTime()) < geoDBMaxAge {
		return path, nil
	}
	now := time.Now()
	err := downloadGeoDB(urlFor(now), path)
	if err != nil {
		if err2 := downloadGeoDB(urlFor(now.AddDate(0, -1, 0)), path); err2 != nil {
			if statErr == nil {
				log.Printf("region lock: keeping stale geo database: %v", errors.Join(err, err2))
				return path, nil
			}
			return "", errors.Join(err, err2)
		}
	}
	return path, nil
}

type countryLookup interface {
	Country(netip.Addr) (string, error)
}

type geoDB struct {
	reader *geoip2.Reader
}

func (g geoDB) Country(addr netip.Addr) (string, error) {
	record, err := g.reader.Country(addr)
	if err != nil {
		return "", err
	}
	return record.Country.ISOCode, nil
}

type regionLock struct {
	allowed map[string]bool
	lookup  countryLookup
}

func (l *regionLock) allow(r *http.Request) bool {
	addr, err := netip.ParseAddr(clientIP(r))
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	if addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsUnspecified() {
		return true
	}
	code, err := l.lookup.Country(addr)
	if err != nil || code == "" {
		return false
	}
	return l.allowed[code]
}
