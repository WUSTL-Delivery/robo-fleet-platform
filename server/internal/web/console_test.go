package web

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

func TestConsoleHandlerAbsentWithoutIndex(t *testing.T) {
	if _, ok := consoleHandler(fstest.MapFS{".gitkeep": {}}); ok {
		t.Fatal("console handler enabled without index.html")
	}
}

func TestConsoleHandlerServesSPA(t *testing.T) {
	h, ok := consoleHandler(fstest.MapFS{
		"index.html":      {Data: []byte("<!doctype html><title>console</title>")},
		"assets/app-1.js": {Data: []byte("console.log(1)")},
		"favicon.svg":     {Data: []byte("<svg/>")},
	})
	if !ok {
		t.Fatal("console handler not enabled")
	}
	cases := []struct {
		path      string
		status    int
		wantIndex bool
	}{
		{"/", 200, true},
		{"/index.html", 200, true},
		{"/fleet/robots/r_1", 200, true}, // client-side route
		{"/assets/app-1.js", 200, false},
		{"/favicon.svg", 200, false},
		{"/assets/missing.js", 404, false},
		{"/missing.png", 404, false},
		{"/api/admin/fleets/x/operator-invites", 404, false},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.path, nil))
		if rec.Code != c.status {
			t.Errorf("GET %s: status %d, want %d", c.path, rec.Code, c.status)
			continue
		}
		isIndex := rec.Body.String() == "<!doctype html><title>console</title>"
		if isIndex != c.wantIndex {
			t.Errorf("GET %s: served index=%v, want %v", c.path, isIndex, c.wantIndex)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/assets/app-1.js", nil))
	if got := rec.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("assets Cache-Control = %q", got)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /: status %d, want 405", rec.Code)
	}
}
