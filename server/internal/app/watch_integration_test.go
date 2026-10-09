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
	"fleetplatform/server/internal/gateway"
	"fleetplatform/server/internal/store"
	"fleetplatform/server/internal/web"
)

func watchMsg(robotID string) protocol.Watch { return protocol.Watch{RobotID: &robotID} }

var watchNone = protocol.Watch{}

// sendRaw writes an envelope whose payload is given as JSON text, with an id.
func (c *client) sendRaw(typ, id, payload string) {
	c.t.Helper()
	if err := rawSendRateLimit(c, typ, id, json.RawMessage(payload)); err != nil {
		c.t.Fatal(err)
	}
}

// nextWatching reads an operator.watching event and returns the robot it names
// ("" when the operator stopped watching).
func (c *client) nextWatching(operatorID string) string {
	c.t.Helper()
	return c.nextOperatorEvent(protocol.EventOperatorWatching, operatorID).Watching
}

// TestIntegrationWatch: operator B hears which robot operator A is looking at,
// a late subscriber finds it in its snapshot, and A going away clears it.
func TestIntegrationWatch(t *testing.T) {
	h := newHarness(t, defaultConfig())
	adaTok, adaID := h.token(store.KindOperator, "ada")
	boTok, boID := h.token(store.KindOperator, "bo")
	svcTok, _ := h.token(store.KindService, "brain")
	robotTok, robotID := h.token(store.KindRobot, "bot-1")
	_, offlineRobotID := h.token(store.KindRobot, "bot-2") // never connects
	eventsOnlyTok, _ := h.token(store.KindService, "events-only")

	eventsOnly := h.connect(eventsOnlyTok)
	eventsOnly.subscribeTo("events", "telemetry")
	robot := h.connect(robotTok)
	ada := h.connect(adaTok)
	onlyWatching(t, ada.subscribeTo("presence"), nil)
	bo := h.connect(boTok)
	bo.subscribeTo("presence")
	ada.nextOperatorEvent(protocol.EventOperatorOnline, boID)

	// A watches a robot: B hears it. Check the wire form once, key by key: the
	// same shape as the other operator events, the entry now with `watching`.
	ada.send(protocol.TypeWatch, watchMsg(robotID))
	env := bo.nextOf(protocol.TypeEvent)
	var raw map[string]json.RawMessage
	mustUnmarshal(t, env.Payload, &raw)
	wantRaw := map[string]string{
		"event":       `"operator.watching"`,
		"operator_id": `"` + adaID + `"`,
		"data":        `{"operator_id":"` + adaID + `","name":"ada","online":true,"watching":"` + robotID + `"}`,
	}
	if len(raw) != len(wantRaw) {
		t.Fatalf("operator.watching payload: %s", env.Payload)
	}
	for k, want := range wantRaw {
		if string(raw[k]) != want {
			t.Fatalf("operator.watching payload %s: %s is %s, want %s", env.Payload, k, raw[k], want)
		}
	}
	// A is a presence subscriber too, so it hears its own change; that event
	// is the only acknowledgement a watch gets.
	if got := ada.nextWatching(adaID); got != robotID {
		t.Fatalf("own operator.watching: %q", got)
	}
	onlyWatching(t, ada.quiet(), map[string]string{adaID: robotID})

	// A late subscriber of any kind finds it in the snapshot.
	svc := h.connect(svcTok)
	onlyWatching(t, svc.subscribeTo("presence"), map[string]string{adaID: robotID})

	// Saying it again is not news.
	ada.send(protocol.TypeWatch, watchMsg(robotID))
	ada.quiet()
	bo.quiet()

	// Two operators may watch the same robot; a robot does not have to be
	// online to be watched.
	bo.send(protocol.TypeWatch, watchMsg(robotID))
	for _, c := range []*client{ada, bo, svc} {
		if got := c.nextWatching(boID); got != robotID {
			t.Fatalf("second watcher: %q", got)
		}
	}
	ada.send(protocol.TypeWatch, watchMsg(offlineRobotID))
	for _, c := range []*client{ada, bo, svc} {
		if got := c.nextWatching(adaID); got != offlineRobotID {
			t.Fatalf("watch of an offline robot: %q", got)
		}
	}
	onlyWatching(t, svc.quiet(), map[string]string{adaID: offlineRobotID, boID: robotID})

	// null stops watching: the entry comes back without `watching`.
	bo.send(protocol.TypeWatch, watchNone)
	env = ada.nextOf(protocol.TypeEvent)
	var ev protocol.Event
	mustUnmarshal(t, env.Payload, &ev)
	if ev.Event != protocol.EventOperatorWatching || string(ev.Data) != `{"operator_id":"`+boID+`","name":"bo","online":true}` {
		t.Fatalf("operator.watching after watch null: %s", env.Payload)
	}
	bo.nextWatching(boID)
	svc.nextWatching(boID)
	bo.send(protocol.TypeWatch, watchNone) // already watching nothing
	bo.quiet()
	ada.quiet()

	// The robot going offline changes nothing about who is looking at it.
	ada.send(protocol.TypeWatch, watchMsg(robotID))
	for _, c := range []*client{ada, bo, svc} {
		c.nextWatching(adaID)
	}
	robot.quiet()
	robot.ws.Close(websocket.StatusNormalClosure, "gone")
	bo.nextEvent(protocol.EventRobotOffline, robotID)
	onlyWatching(t, bo.quiet(), map[string]string{adaID: robotID})
	ada.nextEvent(protocol.EventRobotOffline, robotID)
	svc.nextEvent(protocol.EventRobotOffline, robotID)

	// A disconnects: one operator.offline, whose entry has no `watching`.
	ada.ws.Close(websocket.StatusNormalClosure, "gone")
	env = bo.nextOf(protocol.TypeEvent)
	mustUnmarshal(t, env.Payload, &ev)
	if ev.Event != protocol.EventOperatorOffline || string(ev.Data) != `{"operator_id":"`+adaID+`","name":"ada","online":false}` {
		t.Fatalf("clearing event after disconnect: %s", env.Payload)
	}
	onlyWatching(t, bo.quiet(), nil)
	svc.nextOperatorEvent(protocol.EventOperatorOffline, adaID)
	svc.quiet()

	// Back online, A is watching nothing until it says so again.
	ada = h.connect(adaTok)
	if sum := bo.nextOperatorEvent(protocol.EventOperatorOnline, adaID); sum.Watching != "" {
		t.Fatalf("operator.online after a reconnect: %+v", sum)
	}
	onlyWatching(t, ada.subscribeTo("presence"), nil)

	// None of it reached a client that did not subscribe to presence.
	eventsOnly.quiet()
}

// TestIntegrationWatchRefused: who may send watch and what it may name. A
// refusal changes nothing and tells nobody else.
func TestIntegrationWatchRefused(t *testing.T) {
	h := newHarness(t, defaultConfig())
	adaTok, adaID := h.token(store.KindOperator, "ada")
	boTok, boID := h.token(store.KindOperator, "bo")
	svcTok, svcID := h.token(store.KindService, "brain")
	robotTok, robotID := h.token(store.KindRobot, "bot-1")

	bo := h.connect(boTok)
	bo.subscribeTo("presence")
	robot := h.connect(robotTok)
	bo.nextEvent(protocol.EventRobotOnline, robotID)
	svc := h.connect(svcTok)
	ada := h.connect(adaTok)
	bo.nextOperatorEvent(protocol.EventOperatorOnline, adaID)

	// Only operators watch.
	for _, c := range []*client{robot, svc} {
		c.sendRaw(protocol.TypeWatch, "w-kind", `{"robot_id":"`+robotID+`"}`)
		if e := c.nextError(); e.Code != protocol.ErrNotAuthorized || e.Ref != "w-kind" {
			t.Fatalf("watch from a non-operator: %+v", e)
		}
	}

	// robot_id must be there, and be a non-empty string or null.
	for _, payload := range []string{`{}`, `{"robot_id":""}`, `{"robot_id":7}`, `{"robot_id":["` + robotID + `"]}`} {
		ada.sendRaw(protocol.TypeWatch, "w-bad", payload)
		if e := ada.nextError(); e.Code != protocol.ErrInvalidMessage || e.Ref != "w-bad" {
			t.Fatalf("watch %s: %+v", payload, e)
		}
	}

	// It must name a robot: unknown ids and ids of other kinds are not_found,
	// all with the same answer.
	ada.sendRaw(protocol.TypeWatch, "w-nf", `{"robot_id":"r_0000000000000000"}`)
	unknown := ada.nextError()
	if unknown.Code != protocol.ErrNotFound || unknown.Ref != "w-nf" {
		t.Fatalf("watch of an unknown robot: %+v", unknown)
	}
	for _, id := range []string{svcID, boID, adaID} {
		ada.sendRaw(protocol.TypeWatch, "w-nf", `{"robot_id":"`+id+`"}`)
		if got := ada.nextError(); got != unknown {
			t.Fatalf("watch of non-robot %s: %+v, want %+v", id, got, unknown)
		}
	}

	// A refused watch leaves what the operator was watching alone.
	ada.send(protocol.TypeWatch, watchMsg(robotID))
	bo.nextWatching(adaID)
	ada.sendRaw(protocol.TypeWatch, "w-nf", `{"robot_id":"r_0000000000000000"}`)
	ada.nextError()
	onlyWatching(t, ada.quiet(), map[string]string{adaID: robotID})
	bo.quiet()
	robot.quiet()
	svc.quiet()
}

// TestIntegrationWatchSecondConnection: what an operator watches belongs to
// the connection that said so. A second connection replaces the first without
// the operator going offline, so the fleet gets an operator.watching that
// clears it, and nothing at all when there was nothing to clear.
func TestIntegrationWatchSecondConnection(t *testing.T) {
	h := newHarness(t, defaultConfig())
	adaTok, adaID := h.token(store.KindOperator, "ada")
	boTok, boID := h.token(store.KindOperator, "bo")
	_, robotID := h.token(store.KindRobot, "bot-1")

	bo := h.connect(boTok)
	bo.subscribeTo("presence")
	tab1 := h.connect(adaTok)
	bo.nextOperatorEvent(protocol.EventOperatorOnline, adaID)
	tab1.send(protocol.TypeWatch, watchMsg(robotID))
	if got := bo.nextWatching(adaID); got != robotID {
		t.Fatalf("operator.watching: %q", got)
	}

	tab2 := h.connect(adaTok)
	if e := tab1.nextError(); e.Code != protocol.ErrConflict {
		t.Fatalf("superseded connection: %+v", e)
	}
	if got := bo.nextWatching(adaID); got != "" {
		t.Fatalf("replacement should clear watching, got %q", got)
	}
	snap := bo.quiet()
	onlyOperators(t, snap, map[string]bool{adaID: true, boID: true})
	onlyWatching(t, snap, nil)

	// The new connection speaks for the operator from here on.
	tab2.send(protocol.TypeWatch, watchMsg(robotID))
	if got := bo.nextWatching(adaID); got != robotID {
		t.Fatalf("operator.watching from the new connection: %q", got)
	}
	tab2.ws.Close(websocket.StatusNormalClosure, "gone")
	bo.nextOperatorEvent(protocol.EventOperatorOffline, adaID)
	onlyWatching(t, bo.quiet(), nil)
}

// TestIntegrationWatchRevoke: revoking a watching operator clears it with the
// operator.offline; revoking a watched robot stops everyone watching it.
func TestIntegrationWatchRevoke(t *testing.T) {
	const adminToken = "watch-admin-token"
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
	revoke := func(id string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/admin/fleets/test-fleet/clients/"+id+"/revoke", nil)
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
			t.Fatalf("revoke %s: status %d", id, resp.StatusCode)
		}
	}

	adaTok, adaID := h.token(store.KindOperator, "ada")
	boTok, boID := h.token(store.KindOperator, "bo")
	_, robot1 := h.token(store.KindRobot, "bot-1") // offline throughout
	_, robot2 := h.token(store.KindRobot, "bot-2")

	bo := h.connect(boTok)
	bo.subscribeTo("presence")
	ada := h.connect(adaTok)
	bo.nextOperatorEvent(protocol.EventOperatorOnline, adaID)
	ada.send(protocol.TypeWatch, watchMsg(robot1))
	bo.nextWatching(adaID)
	bo.send(protocol.TypeWatch, watchMsg(robot2))
	bo.nextWatching(boID)

	// The watched robot is revoked: its watcher is told it stopped, the other
	// operator's watch is untouched, and the robot can no longer be watched.
	revoke(robot1)
	if got := bo.nextWatching(adaID); got != "" {
		t.Fatalf("watcher of a revoked robot: %q", got)
	}
	onlyWatching(t, bo.quiet(), map[string]string{boID: robot2})
	ada.send(protocol.TypeWatch, watchMsg(robot1))
	if e := ada.nextError(); e.Code != protocol.ErrNotFound {
		t.Fatalf("watch of a revoked robot: %+v", e)
	}

	// The watching operator is revoked: operator.offline, nothing left behind.
	ada.send(protocol.TypeWatch, watchMsg(robot2))
	if got := bo.nextWatching(adaID); got != robot2 {
		t.Fatalf("operator.watching: %q", got)
	}
	revoke(adaID)
	bo.nextOperatorEvent(protocol.EventOperatorOffline, adaID)
	snap := bo.quiet()
	onlyOperators(t, snap, map[string]bool{boID: true})
	onlyWatching(t, snap, map[string]string{boID: robot2})
}

// TestIntegrationWatchRateLimited: watch is metered per connection like
// telemetry. Over the limit the message is dropped, the sender is told which
// one, and the fleet keeps the last value that got through.
func TestIntegrationWatchRateLimited(t *testing.T) {
	// Two back to back, then effectively none for the length of the test.
	h := newRateLimitedHarness(t, defaultConfig(), gateway.RateLimit{PerSec: 0.001, Burst: 2})
	adaTok, adaID := h.token(store.KindOperator, "ada")
	boTok, _ := h.token(store.KindOperator, "bo")
	_, robot1 := h.token(store.KindRobot, "bot-1")
	_, robot2 := h.token(store.KindRobot, "bot-2")

	bo := h.connect(boTok)
	bo.subscribeTo("presence")
	ada := h.connect(adaTok)
	bo.nextOperatorEvent(protocol.EventOperatorOnline, adaID)

	ada.sendRaw(protocol.TypeWatch, "w-1", `{"robot_id":"`+robot1+`"}`)
	ada.sendRaw(protocol.TypeWatch, "w-2", `{"robot_id":"`+robot2+`"}`)
	ada.sendRaw(protocol.TypeWatch, "w-3", `{"robot_id":"`+robot1+`"}`)
	if e := ada.nextError(); e.Code != protocol.ErrRateLimited || e.Ref != "w-3" {
		t.Fatalf("third watch: %+v", e)
	}
	if got := bo.nextWatching(adaID); got != robot1 {
		t.Fatalf("first watch: %q", got)
	}
	if got := bo.nextWatching(adaID); got != robot2 {
		t.Fatalf("second watch: %q", got)
	}
	// The dropped one changed nothing, and the connection is still usable.
	onlyWatching(t, bo.quiet(), map[string]string{adaID: robot2})
	onlyWatching(t, ada.quiet(), map[string]string{adaID: robot2})
}
