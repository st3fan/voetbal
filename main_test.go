package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSetupRequiredHandler(t *testing.T) {
	handler := setupRequiredHandler()

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusOK {
		t.Errorf("index: got %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "VOETBAL_NETWORK_LOCK") {
		t.Errorf("index: setup note missing from body")
	}

	for _, path := range []string{"/play", "/proxy", "/watchers", "/nope"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: got %d, want 403", path, w.Code)
		}
	}
}
