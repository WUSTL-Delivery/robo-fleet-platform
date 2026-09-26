package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"fleetplatform/server/internal/gateway"
)

func TestConsoleConfigServesMapView(t *testing.T) {
	cc := ConsoleConfig{Map: &MapView{Center: LatLon{Lat: 38.6488, Lon: -90.3108}, RadiusM: 1500, Lock: true}}
	srv := httptest.NewServer(HandlerWithConsoleConfig(&gateway.Gateway{}, cc))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/console/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, content-type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	var got ConsoleConfig
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Map == nil || *got.Map != *cc.Map {
		t.Fatalf("got %+v, want %+v", got.Map, cc.Map)
	}
}

func TestConsoleConfigWithoutMapIsNull(t *testing.T) {
	srv := httptest.NewServer(Handler(&gateway.Gateway{}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/console/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if v, ok := raw["map"]; !ok || v != nil {
		t.Fatalf("body = %v, want {\"map\": null}", raw)
	}

	post, err := http.Post(srv.URL+"/api/console/config", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	post.Body.Close()
	if post.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status %d, want 405", post.StatusCode)
	}
}
