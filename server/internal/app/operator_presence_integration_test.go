package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"fleetplatform/sdk/go/protocol"
	"fleetplatform/server/internal/admin"
	"fleetplatform/server/internal/app"
	"fleetplatform/server/internal/store"
	"fleetplatform/server/internal/web"
)

func (h *harness) token(kind store.Kind, name string) (token, id string) {
	h.t.Helper()
	tok, c, err := h.store.CreateToken(h.fleet.ID, kind, name)
	if err != nil {
		h.t.Fatal(err)
	}
	return tok, c.ID
}

// subscribeTo subscribes and returns the snapshot, which must be the very next
// envelope.
func (c *client) subscribeTo(topics ...string) protocol.Snapshot {
	c.t.Helper()
	c.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: topics})
	var snap protocol.Snapshot
	mustUnmarshal(c.t, c.nextOf(protocol.TypeSnapshot).Payload, &snap)
	return snap
}

// TestIntegrationOperatorPresence: two operators each hear the other come
// online on the presence topic, a late subscriber finds both in its snapshot,
// and going offline is announced the same way.
func TestIntegrationOperatorPresence(t *testing.T) {
	h := newHarness(t, defaultConfig())
	adaTok, adaID := h.token(store.KindOperator, "ada")
	boTok, boID := h.token(store.KindOperator, "") // an operator with no name
	svcTok, _ := h.token(store.KindService, "brain")
	eventsOnlyTok, _ := h.token(store.KindService, "events-only")

	// A client that did not ask for the presence topic never hears operators.
	eventsOnly := h.connect(eventsOnlyTok)
	eventsOnly.subscribeTo("events", "telemetry")

	// The snapshot lists every operator of the fleet, online or not, oldest
	// first, and nothing that is not an operator.
	ada := h.connect(adaTok)
	snap := ada.subscribeTo("presence")
	if len(snap.Operators) != 2 ||
		snap.Operators[0] != (protocol.OperatorSummary{OperatorID: adaID, Name: "ada", Online: true}) ||
		snap.Operators[1] != (protocol.OperatorSummary{OperatorID: boID, Online: false}) {
		t.Fatalf("first operator's snapshot: %+v", snap.Operators)
	}

	// The second operator arrives: the first hears it. Check the wire form once,
	// key by key: an operator event has operator_id and no robot_id.
	bo := h.connect(boTok)
	env := ada.nextOf(protocol.TypeEvent)
	var raw map[string]json.RawMessage
	mustUnmarshal(t, env.Payload, &raw)
	wantRaw := map[string]string{
		"event":       `"operator.online"`,
		"operator_id": `"` + boID + `"`,
		"data":        `{"operator_id":"` + boID + `","online":true}`,
	}
	if len(raw) != len(wantRaw) {
		t.Fatalf("operator.online payload: %s", env.Payload)
	}
	for k, want := range wantRaw {
		if string(raw[k]) != want {
			t.Fatalf("operator.online payload %s: %s is %s, want %s", env.Payload, k, raw[k], want)
		}
	}
	onlyOperators(t, bo.subscribeTo("presence"), map[string]bool{adaID: true, boID: true})
	bo.quiet() // an operator is not sent its own operator.online

	// The first operator leaves and comes back: the second hears both.
	ada.ws.Close(websocket.StatusNormalClosure, "gone")
	if sum := bo.nextOperatorEvent(protocol.EventOperatorOffline, adaID); sum.Name != "ada" {
		t.Fatalf("operator.offline data: %+v", sum)
	}
	onlyOperators(t, bo.quiet(), map[string]bool{adaID: false, boID: true})
	ada = h.connect(adaTok)
	if sum := bo.nextOperatorEvent(protocol.EventOperatorOnline, adaID); sum.Name != "ada" {
		t.Fatalf("operator.online data: %+v", sum)
	}
	ada.quiet()

	// A late subscriber, of any kind, sees both in its snapshot.
	svc := h.connect(svcTok)
	onlyOperators(t, svc.subscribeTo("presence"), map[string]bool{adaID: true, boID: true})
	bo.quiet() // a service coming online is not operator presence

	bo.ws.Close(websocket.StatusNormalClosure, "gone")
	svc.nextOperatorEvent(protocol.EventOperatorOffline, boID)
	svc.quiet()
	eventsOnly.quiet()
}

// TestIntegrationOperatorSecondConnection: a second connection with the same
// operator token replaces the first. The operator stays online throughout, so
// the fleet hears nothing until the surviving connection ends.
func TestIntegrationOperatorSecondConnection(t *testing.T) {
	h := newHarness(t, defaultConfig())
	adaTok, adaID := h.token(store.KindOperator, "ada")
	watcherTok, watcherID := h.token(store.KindOperator, "watcher")

	watcher := h.connect(watcherTok)
	watcher.subscribeTo("presence")

	tab1 := h.connect(adaTok)
	watcher.nextOperatorEvent(protocol.EventOperatorOnline, adaID)

	tab2 := h.connect(adaTok)
	// The first connection is told why and then closed.
	if e := tab1.nextError(); e.Code != protocol.ErrConflict {
		t.Fatalf("superseded connection: %+v", e)
	}
	if env, err := tab1.recv(); err == nil {
		t.Fatalf("superseded connection still open: got %s %s", env.Type, env.Payload)
	}
	// By now the old connection's teardown has run. No offline, no second online.
	onlyOperators(t, watcher.quiet(), map[string]bool{adaID: true, watcherID: true})
	tab2.quiet()

	tab2.ws.Close(websocket.StatusNormalClosure, "gone")
	watcher.nextOperatorEvent(protocol.EventOperatorOffline, adaID)
	onlyOperators(t, watcher.quiet(), map[string]bool{adaID: false, watcherID: true})
}

// TestIntegrationOperatorRevokedGoesOffline: revoking an operator's token
// closes its connection, which the fleet sees as operator.offline; afterwards
// the operator is not listed at all.
func TestIntegrationOperatorRevokedGoesOffline(t *testing.T) {
	const adminToken = "presence-admin-token"
	st, err := store.OpenSqlite(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fleet, err := st.CreateFleet("test-fleet")
	if err != nil {
		t.Fatal(err)
	}
	a := app.New(defaultConfig(), st)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Run(ctx)
	srv := httptest.NewServer(web.Handler(a.Gateway(), admin.Handler(st, adminToken, a)))
	t.Cleanup(srv.Close)
	h := &harness{t: t, url: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws", store: st, fleet: fleet}

	adaTok, adaID := h.token(store.KindOperator, "ada")
	watcherTok, watcherID := h.token(store.KindOperator, "watcher")
	watcher := h.connect(watcherTok)
	watcher.subscribeTo("presence")
	h.connect(adaTok)
	watcher.nextOperatorEvent(protocol.EventOperatorOnline, adaID)

	req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/admin/fleets/test-fleet/clients/"+adaID+"/revoke", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+adminToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke: status %d", resp.StatusCode)
	}

	watcher.nextOperatorEvent(protocol.EventOperatorOffline, adaID)
	onlyOperators(t, watcher.quiet(), map[string]bool{watcherID: true})
}
