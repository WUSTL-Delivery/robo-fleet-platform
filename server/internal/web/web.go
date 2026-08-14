// Package web mounts the HTTP surface: /ws for clients, /healthz, and (with the
// console milestone) the static console + HTTP API.
package web

import (
	"net/http"

	"fleetplatform/server/internal/gateway"
)

func Handler(gw *gateway.Gateway) http.Handler {
	mux := http.NewServeMux()
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
