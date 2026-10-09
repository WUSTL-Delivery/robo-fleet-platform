package app_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"

	"fleetplatform/sdk/go/protocol"
	"fleetplatform/server/internal/store"
)

// expectLayerReplay reads the snapshot and then the retained declare + update
// that follow it.
func expectLayerReplay(t *testing.T, c *client, layerID, wantData string) {
	t.Helper()
	c.expect(protocol.TypeSnapshot)
	var decl protocol.LayerDeclare
	mustUnmarshal(t, c.expect(protocol.TypeLayerDeclare).Payload, &decl)
	if decl.LayerID != layerID || decl.Kind != "geojson" || decl.Title != "Test layer" {
		t.Fatalf("replayed declare: %+v", decl)
	}
	var upd protocol.LayerUpdate
	mustUnmarshal(t, c.expect(protocol.TypeLayerUpdate).Payload, &upd)
	if upd.LayerID != layerID || string(upd.Data) != wantData {
		t.Fatalf("replayed update: %+v data=%s", upd, upd.Data)
	}
}

// TestIntegrationLayerRetentionLateSubscriber: a service declares and updates a
// layer before anyone listens; a client that subscribes to `layers` later gets
// the declare and the latest update right after its snapshot, without the
// service re-sending.
func TestIntegrationLayerRetentionLateSubscriber(t *testing.T) {
	h := newHarness(t, defaultConfig())
	svcToken, _, err := h.store.CreateToken(h.fleet.ID, store.KindService, "path-service")
	if err != nil {
		t.Fatal(err)
	}
	opToken, _, err := h.store.CreateToken(h.fleet.ID, store.KindOperator, "op-1")
	if err != nil {
		t.Fatal(err)
	}

	svc := h.connect(svcToken)
	svc.send(protocol.TypeLayerDeclare, protocol.LayerDeclare{
		LayerID: "test-graph", Kind: "geojson", Title: "Test layer",
		Style: map[string]any{"line-color-by": "properties.band"},
	})
	svc.send(protocol.TypeLayerUpdate, protocol.LayerUpdate{
		LayerID: "test-graph", Data: json.RawMessage(`{"type":"FeatureCollection","features":[]}`),
	})
	latest := `{"type":"FeatureCollection","features":[{"type":"Feature","geometry":null,"properties":{"band":2}}]}`
	svc.send(protocol.TypeLayerUpdate, protocol.LayerUpdate{LayerID: "test-graph", Data: json.RawMessage(latest)})

	// The service's messages are handled in order on its own read loop, so its
	// own subscribe is a barrier: once it answers, all three have been retained.
	svc.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: []string{"layers"}})
	expectLayerReplay(t, svc, "test-graph", latest)

	// A late operator subscribes and gets both, with nothing re-sent.
	op := h.connect(opToken)
	op.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: []string{"presence", "layers"}})
	expectLayerReplay(t, op, "test-graph", latest)

	// A subscribe without `layers` gets no replay: straight after its snapshot
	// comes the snapshot of the next subscribe.
	op2Token, _, err := h.store.CreateToken(h.fleet.ID, store.KindOperator, "op-2")
	if err != nil {
		t.Fatal(err)
	}
	op2 := h.connect(op2Token)
	op2.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: []string{"presence"}})
	op2.expect(protocol.TypeSnapshot)
	op2.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: []string{"events"}})
	if env, err := op2.recv(); err != nil || env.Type != protocol.TypeSnapshot {
		t.Fatalf("non-layers subscriber got %v %v", env.Type, err)
	}
}

// TestIntegrationLayerRetentionOwnerTTL: layers survive their service's
// disconnect within the TTL and are dropped after it.
func TestIntegrationLayerRetentionOwnerTTL(t *testing.T) {
	cfg := defaultConfig()
	cfg.LayerTTL = 200 * time.Millisecond
	h := newHarness(t, cfg)
	svcToken, _, err := h.store.CreateToken(h.fleet.ID, store.KindService, "path-service")
	if err != nil {
		t.Fatal(err)
	}
	opToken, _, err := h.store.CreateToken(h.fleet.ID, store.KindOperator, "op-1")
	if err != nil {
		t.Fatal(err)
	}

	svc := h.connect(svcToken)
	svc.send(protocol.TypeLayerDeclare, protocol.LayerDeclare{LayerID: "test-graph", Kind: "geojson", Title: "Test layer"})
	svc.send(protocol.TypeLayerUpdate, protocol.LayerUpdate{LayerID: "test-graph", Data: json.RawMessage(`{"n":1}`)})
	svc.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: []string{"layers"}})
	expectLayerReplay(t, svc, "test-graph", `{"n":1}`)
	svc.ws.Close(websocket.StatusNormalClosure, "restart")

	// Inside the TTL a new subscriber still sees the layer.
	op := h.connect(opToken)
	op.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: []string{"layers"}})
	expectLayerReplay(t, op, "test-graph", `{"n":1}`)

	// Past the TTL the layer is gone: the replay after the snapshot is empty,
	// so the next envelope is the snapshot of a second subscribe.
	time.Sleep(cfg.LayerTTL + 5*cfg.SweepEvery)
	op.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: []string{"layers"}})
	op.expect(protocol.TypeSnapshot)
	op.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: []string{"presence"}})
	env, err := op.recv()
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != protocol.TypeSnapshot {
		t.Fatalf("layer outlived its TTL: got %s %s", env.Type, env.Payload)
	}
}
