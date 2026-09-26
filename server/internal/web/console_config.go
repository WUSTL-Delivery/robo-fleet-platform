package web

import (
	"encoding/json"
	"net/http"
)

// ConsoleConfig is installation config the console reads at startup, served
// unauthenticated at GET /api/console/config. It must hold nothing secret: the
// login screen fetches it before anyone has signed in.
type ConsoleConfig struct {
	// Map is the map's home view; nil lets the console fit the view to the fleet.
	Map *MapView `json:"map"`
}

// MapView centres the map on a point and shows radius_m around it. With Lock,
// the console keeps panning and zooming inside that area.
type MapView struct {
	Center  LatLon  `json:"center"`
	RadiusM float64 `json:"radius_m"`
	Lock    bool    `json:"lock"`
}

type LatLon struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

func consoleConfigHandler(cc ConsoleConfig) http.HandlerFunc {
	body, err := json.Marshal(cc)
	if err != nil {
		panic(err) // plain structs of numbers and bools always marshal
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(body)
	}
}
