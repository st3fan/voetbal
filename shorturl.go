package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"strings"
	"sync"
)

// copyShortURLs makes the copy buttons on the index page hand out short
// /r/{code} URLs instead of the full /proxy?url=... form.
var copyShortURLs = os.Getenv("VOETBAL_COPY_SHORT_URLS") != ""

type shortURLRegistry struct {
	mu     sync.Mutex
	byCode map[string]string
}

var shortURLs = shortURLRegistry{byCode: map[string]string{}}

// register maps u to a short code — the last 3 hex chars of its SHA-256,
// extended one char at a time on collision — and returns the code.
func (reg *shortURLRegistry) register(u string) string {
	sum := sha256.Sum256([]byte(u))
	full := hex.EncodeToString(sum[:])
	reg.mu.Lock()
	defer reg.mu.Unlock()
	for n := 3; n <= len(full); n++ {
		code := full[len(full)-n:]
		if existing, taken := reg.byCode[code]; taken && existing != u {
			continue
		}
		reg.byCode[code] = u
		return code
	}
	return full
}

func (reg *shortURLRegistry) lookup(code string) (string, bool) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	u, ok := reg.byCode[code]
	return u, ok
}

func copyPath(u string) string {
	if !copyShortURLs {
		return proxyPath(u)
	}
	return "/r/" + shortURLs.register(u)
}

// refreshShortURLs re-derives the codes for the current streams, so a code
// typed from another device keeps working after a restart. Codes are
// deterministic, so this reproduces the same mapping.
var refreshShortURLs = func() {
	streams, err := fetchStreams()
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	for _, stream := range streams {
		wg.Go(func() {
			for _, q := range streamQualities(stream) {
				shortURLs.register(q.URL)
			}
		})
	}
	wg.Wait()
}

func handleShortURL(w http.ResponseWriter, r *http.Request) {
	code := strings.ToLower(r.PathValue("code"))
	target, ok := shortURLs.lookup(code)
	if !ok {
		refreshShortURLs()
		target, ok = shortURLs.lookup(code)
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, proxyPath(target), http.StatusFound)
}
