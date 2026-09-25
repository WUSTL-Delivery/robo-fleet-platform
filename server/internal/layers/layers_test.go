package layers

import (
	"encoding/json"
	"testing"
	"time"

	"fleetplatform/server/internal/protocol"
)

func layerEnv(typ, id, body string) protocol.Envelope {
	return protocol.Envelope{V: 0, Type: typ, ID: id, Payload: json.RawMessage(body)}
}

func TestLayerRetentionReplayOrderAndLatest(t *testing.T) {
	s := New()
	s.Update("f1", "svc", "b", layerEnv(protocol.TypeLayerUpdate, "m1", `{"layer_id":"b","data":1}`))
	s.Declare("f1", "svc", "a", layerEnv(protocol.TypeLayerDeclare, "m2", `{"layer_id":"a","kind":"geojson"}`))
	s.Update("f1", "svc", "a", layerEnv(protocol.TypeLayerUpdate, "m3", `{"layer_id":"a","data":1}`))
	s.Update("f1", "svc", "a", layerEnv(protocol.TypeLayerUpdate, "m4", `{"layer_id":"a","data":2}`))
	s.Declare("f2", "other", "a", layerEnv(protocol.TypeLayerDeclare, "m5", `{"layer_id":"a","kind":"geojson"}`))

	got := s.Replay("f1")
	if len(got) != 3 {
		t.Fatalf("replay len = %d: %+v", len(got), got)
	}
	if got[0].Type != protocol.TypeLayerDeclare || got[1].Type != protocol.TypeLayerUpdate ||
		string(got[1].Payload) != `{"layer_id":"a","data":2}` || string(got[2].Payload) != `{"layer_id":"b","data":1}` {
		t.Fatalf("replay = %+v", got)
	}
	for _, env := range got {
		if env.ID != "" {
			t.Fatalf("retained envelope kept sender id: %+v", env)
		}
	}
	if len(s.Replay("f2")) != 1 || len(s.Replay("nope")) != 0 {
		t.Fatal("fleets not isolated")
	}
}

func TestLayerRetentionOwnerTTL(t *testing.T) {
	s := New()
	t0 := time.Unix(1000, 0)
	s.Declare("f1", "svc", "a", layerEnv(protocol.TypeLayerDeclare, "", `{"layer_id":"a","kind":"geojson"}`))
	s.Declare("f1", "keep", "k", layerEnv(protocol.TypeLayerDeclare, "", `{"layer_id":"k","kind":"geojson"}`))

	// Reconnect inside the TTL keeps the layers.
	s.OwnerDown("svc", t0)
	s.OwnerUp("svc")
	if n := s.Sweep(t0.Add(time.Hour), time.Minute); n != 0 {
		t.Fatalf("dropped %d after reconnect", n)
	}

	s.OwnerDown("svc", t0)
	if n := s.Sweep(t0.Add(30*time.Second), time.Minute); n != 0 {
		t.Fatalf("dropped %d before ttl", n)
	}
	if n := s.Sweep(t0.Add(2*time.Minute), time.Minute); n != 1 {
		t.Fatalf("dropped %d after ttl, want 1", n)
	}
	got := s.Replay("f1")
	if len(got) != 1 || string(got[0].Payload) != `{"layer_id":"k","kind":"geojson"}` {
		t.Fatalf("replay after sweep = %+v", got)
	}
}
