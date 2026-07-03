package main

import (
	"cmp"
	"embed"
	"flag"
	"fmt"
	"html/template"
	"log"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/oschwald/geoip2-golang/v2"
)

const version = "voetbal/0.1"

//go:embed templates/*.html
var templateFS embed.FS

var templates = template.Must(template.ParseFS(templateFS, "templates/*.html"))

type card struct {
	Title     string
	Thumb     string
	Locked    bool
	Region    string
	IsOnline  bool
	Qualities []Quality
}

type indexData struct {
	Error string
	Cards []card
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
				Qualities: streamQualities(stream),
			}
		})
	}
	wg.Wait()
	return cards
}

func render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	var data indexData
	if streams, err := fetchStreams(); err != nil {
		data.Error = err.Error()
	} else {
		data.Cards = buildCards(streams)
	}
	render(w, "index.html", data)
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

func main() {
	addr := flag.String("addr", ":8000", "listen address")
	showVersion := flag.Bool("v", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleIndex)
	mux.HandleFunc("GET /play", handlePlay)
	mux.HandleFunc("GET /proxy", handleProxy)
	mux.HandleFunc("GET /watchers", handleWatchers)
	var handler http.Handler = mux
	if allowed := parseRegionLock(os.Getenv("VOETBAL_REGION_LOCK")); len(allowed) > 0 {
		path, err := ensureGeoDB(geoDBDir, geoDBURL)
		if err != nil {
			log.Fatalf("region lock: %v", err)
		}
		reader, err := geoip2.Open(path)
		if err != nil {
			log.Fatalf("region lock: %v", err)
		}
		handler = (&regionLock{allowed: allowed, lookup: geoDB{reader}}).middleware(mux)
		log.Printf("region lock enabled: %s", strings.Join(slices.Sorted(maps.Keys(allowed)), ","))
	}
	log.Printf("listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, handler))
}
