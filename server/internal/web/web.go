// Package web mounts the HTTP surface: /ws for clients, /healthz, and (with the
// console milestone) the static console + HTTP API.
package web

import (
	"net/http"

	"fleetplatform/server/internal/gateway"
)

// Handler builds the mux. admin, when non-nil, is mounted at /api/admin/
// (internal/admin; nil means the admin API is off).
func Handler(gw *gateway.Gateway, admin ...http.Handler) http.Handler {
	mux := http.NewServeMux()
	for _, h := range admin {
		if h != nil {
			mux.Handle("/api/admin/", h)
		}
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/ws", gw.ServeWS)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("fleet-platform server (console lands with the vertical slice)\n"))
	})
	return mux
}
