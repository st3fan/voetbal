package main

import (
	"cmp"
	"embed"
	"flag"
	"fmt"
	"html/template"
	"log"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oschwald/geoip2-golang/v2"
)

const version = "voetbal/0.1"

//go:embed templates/*.html
var templateFS embed.FS

var templates = template.Must(template.New("").
	Funcs(template.FuncMap{"proxyPath": proxyPath, "copyPath": copyPath}).
	ParseFS(templateFS, "templates/*.html"))

type qualityView struct {
	Label    string
	PlayPath string // in-browser player page link
	Path     string // short proxied path handed out by the copy button
}

type card struct {
	Title     string
	Thumb     string
	Locked    bool
	Region    string
	IsOnline  bool
	Qualities []qualityView
}

func qualityViews(stream Stream, qualities []Quality) []qualityView {
	var views []qualityView
	for _, q := range qualities {
		path := streamPath(stream.ID, q.Resolution)
		playPath := playerPath(stream.ID, q.Resolution)
		if stream.ID == "" {
			playPath = "/play?" + url.Values{"src": {q.URL}, "title": {cmp.Or(stream.Title, "Stream")}}.Encode()
			path = copyPath(q.URL)
		}
		views = append(views, qualityView{Label: q.Label, PlayPath: playPath, Path: path})
	}
	return views
}

type indexData struct {
	SetupRequired bool
	Error         string
	BaseURL       string
	Cards         []card
}

func buildCards(streams []Stream) []card {
	cards := make([]card, len(streams))
	var wg sync.WaitGroup
	for i, stream := range streams {
		wg.Go(func() {
			cards[i] = card{
				Title:     cmp.Or(stream.Title, "(no title)"),
				Thumb:     thumbnailURL(stream, 640),
				Locked:    len(stream.AllowedAreas) > 0,
				Region:    regionLabel(stream),
				IsOnline:  stream.IsOnline,
				Qualities: qualityViews(stream, streamQualities(stream)),
			}
		})
	}
	wg.Wait()
	return cards
}

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, name, data); err != nil {
		slog.Info("render failed", "template", name, "error", err.Error())
	}
}

func requestBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	data := indexData{BaseURL: requestBaseURL(r)}
	if streams, err := fetchStreams(); err != nil {
		data.Error = err.Error()
	} else {
		data.Cards = buildCards(streams)
	}
	render(w, "index.html", data)
}

func setupRequiredHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			render(w, "index.html", indexData{SetupRequired: true})
			return
		}
		http.Error(w, "access is disabled: set VOETBAL_NETWORK_LOCK or VOETBAL_REGION_LOCK", http.StatusForbidden)
	})
}

func handlePlay(w http.ResponseWriter, r *http.Request) {
	src := r.URL.Query().Get("src")
	if !strings.HasPrefix(src, "http://") && !strings.HasPrefix(src, "https://") {
		http.Error(w, "invalid stream URL", http.StatusBadRequest)
		return
	}
	title := cmp.Or(r.URL.Query().Get("title"), "Stream")
	render(w, "player.html", struct{ Title, Src string }{title, proxyPath(src)})
}

// parseSize parses "512MB", "12GB" or a plain number of megabytes.
func parseSize(value string) (int64, error) {
	s := strings.ToUpper(strings.TrimSpace(value))
	unit := int64(1 << 20)
	if suffix, ok := strings.CutSuffix(s, "GB"); ok {
		unit, s = 1<<30, suffix
	} else if suffix, ok := strings.CutSuffix(s, "MB"); ok {
		s = suffix
	}
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", value)
	}
	return n * unit, nil
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		log.Fatalf("%s: invalid duration %q", name, value)
	}
	return d
}

func envSize(name string, fallback int64) int64 {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	n, err := parseSize(value)
	if err != nil {
		log.Fatalf("%s: %v", name, err)
	}
	return n
}

func main() {
	// All logging is structured JSON on stdout; this also routes the std
	// log package (log.Fatalf) through the same handler.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	addr := flag.String("addr", ":8000", "listen address")
	showVersion := flag.Bool("v", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleIndex)
	mux.HandleFunc("GET /caches", handleCaches)
	mux.HandleFunc("GET /play", handlePlay)
	mux.HandleFunc("GET /player/nos/{id}", handlePlayer)
	mux.HandleFunc("GET /player/nos/{id}/{resolution}", handlePlayer)
	mux.HandleFunc("GET /playlist.m3u", handlePlaylist)
	mux.HandleFunc("GET /proxy", handleProxy)
	mux.HandleFunc("GET /proxy/nos/{id}/{resolution}/{file...}", handleSegment)
	mux.HandleFunc("GET /stream/nos/{id}", handleStream)
	mux.HandleFunc("GET /stream/nos/{id}/{resolution}", handleStream)
	mux.HandleFunc("GET /r/{code}", handleShortURL)
	mux.HandleFunc("GET /watchers", handleWatchers)
	memoryCacheTTL = envDuration("VOETBAL_MEMORY_CACHE_TTL", memoryCacheTTL)
	streamMux.maxBytes = envSize("VOETBAL_MEMORY_CACHE_SIZE", streamMux.maxBytes)
	slog.Info("memory cache", "ttl", untilLabel(memoryCacheTTL), "max", humanBytes(streamMux.maxBytes))
	diskTTL := envDuration("VOETBAL_DISK_CACHE_TTL", 3*time.Hour)
	if diskSize := envSize("VOETBAL_DISK_CACHE_SIZE", 12<<30); diskSize > 0 {
		cache, err := newDiskCache(filepath.Join(dataPath(), "cache"), diskTTL, diskSize)
		if err != nil {
			slog.Info("disk cache disabled", "error", err.Error())
		} else {
			diskCaches = cache
			slog.Info("disk cache", "path", cache.root, "ttl", untilLabel(diskTTL),
				"max", humanBytes(diskSize), "cached", humanBytes(cache.totalBytes()))
			go func() {
				for range time.Tick(time.Minute) {
					diskCaches.prune(time.Now())
				}
			}()
		}
	} else {
		slog.Info("disk cache disabled", "reason", "VOETBAL_DISK_CACHE_SIZE=0")
	}
	var handler http.Handler = mux
	var locks []accessLock
	prefixes, asns, err := parseNetworkLock(os.Getenv("VOETBAL_NETWORK_LOCK"))
	if err != nil {
		log.Fatalf("network lock: %v", err)
	}
	for _, asn := range asns {
		announced, err := fetchASNPrefixes(ripePrefixesURL(asn))
		if err != nil {
			log.Fatalf("network lock: AS%s: %v", asn, err)
		}
		slog.Info("network lock: ASN resolved", "asn", asn, "prefixes", len(announced))
		prefixes = append(prefixes, announced...)
	}
	if len(prefixes) > 0 || len(asns) > 0 {
		locks = append(locks, &networkLock{prefixes: prefixes})
		slog.Info("network lock enabled", "prefixes", len(prefixes))
	}
	if allowed := parseRegionLock(os.Getenv("VOETBAL_REGION_LOCK")); len(allowed) > 0 {
		path, err := ensureGeoDB(dataPath(), geoDBURL)
		if err != nil {
			log.Fatalf("region lock: %v", err)
		}
		reader, err := geoip2.Open(path)
		if err != nil {
			log.Fatalf("region lock: %v", err)
		}
		locks = append(locks, &regionLock{allowed: allowed, lookup: geoDB{reader}})
		slog.Info("region lock enabled", "regions", strings.Join(slices.Sorted(maps.Keys(allowed)), ","))
	}
	if len(locks) > 0 {
		handler = lockMiddleware(locks, mux)
	} else {
		slog.Info("no access lock configured: set VOETBAL_NETWORK_LOCK or VOETBAL_REGION_LOCK to enable access")
		handler = setupRequiredHandler()
	}
	handler = requestLogger(handler)
	slog.Info("listening", "addr", *addr)
	log.Fatal(http.ListenAndServe(*addr, handler))
}
