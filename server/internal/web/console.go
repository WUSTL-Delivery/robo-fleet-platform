package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// dist holds the built console (console/ -> Vite -> dist). In the repo it is
// only a .gitkeep, so `go build` and `go test` never need npm; the Docker
// build and `npm run embed` copy the real assets in before compiling.
//
//go:embed all:dist
var dist embed.FS

// embeddedConsole returns the embedded console's file tree.
func embeddedConsole() fs.FS {
	root, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // the embed pattern guarantees dist/ exists
	}
	return root
}

// consoleHandler serves a built console as a single-page app: real files are
// served as-is, any other path gets index.html so client-side routes survive a
// reload. ok is false when root has no index.html (console not built).
func consoleHandler(root fs.FS) (h http.Handler, ok bool) {
	index, err := fs.ReadFile(root, "index.html")
	if err != nil {
		return nil, false
	}
	files := http.FileServerFS(root)
	serveIndex := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(index)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		// An API path nothing else claimed (e.g. the admin API while it is off)
		// is a 404 whatever the method, so a POST there doesn't read as 405.
		if name == "api" || strings.HasPrefix(name, "api/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if name == "" || name == "index.html" {
			serveIndex(w)
			return
		}
		if st, err := fs.Stat(root, name); err == nil && !st.IsDir() {
			// Vite fingerprints everything under assets/, so it never changes.
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			files.ServeHTTP(w, r)
			return
		}
		// A missing asset, or an API path nothing else claimed (e.g. the admin
		// API while it is off), is a real 404; anything else is a client route.
		if name == "api" || strings.HasPrefix(name, "api/") || strings.HasPrefix(name, "assets/") || path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		serveIndex(w)
	}), true
}
