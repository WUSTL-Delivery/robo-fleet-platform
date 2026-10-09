package app_test

import (
	"context"
	"encoding/json"
	"io"
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

const isolationAdminToken = "isolation-admin-token"

// allTopics is every well-known topic plus the domain channel the test uses.
var allTopics = []string{"presence", "events", "telemetry", "layers", "channel:jobs"}

// next reads exactly one envelope. The isolation test never skips: anything
// unexpected on a socket is a leak.
func (c *client) next() protocol.Envelope {
	c.t.Helper()
	env, err := c.recv()
	if err != nil {
		c.t.Fatalf("reading next envelope: %v", err)
	}
	return env
}

func (c *client) nextOf(typ string) protocol.Envelope {
	c.t.Helper()
	env := c.next()
	if env.Type != typ {
		c.t.Fatalf("want %s next, got %s %s", typ, env.Type, env.Payload)
	}
	return env
}

func (c *client) nextEvent(name, robotID string) protocol.Event {
	c.t.Helper()
	var ev protocol.Event
	mustUnmarshal(c.t, c.nextOf(protocol.TypeEvent).Payload, &ev)
	if ev.Event != name || ev.RobotID != robotID {
		c.t.Fatalf("want event %s for %s next, got %+v", name, robotID, ev)
	}
	return ev
}

// nextOperatorEvent reads an operator.* presence event and checks all of it:
// the subject is operator_id (never robot_id), and data is the operator's entry.
func (c *client) nextOperatorEvent(name, operatorID string) protocol.OperatorSummary {
	c.t.Helper()
	var ev protocol.Event
	mustUnmarshal(c.t, c.nextOf(protocol.TypeEvent).Payload, &ev)
	if ev.Event != name || ev.OperatorID != operatorID || ev.RobotID != "" {
		c.t.Fatalf("want event %s for %s next, got %+v", name, operatorID, ev)
	}
	var sum protocol.OperatorSummary
	mustUnmarshal(c.t, ev.Data, &sum)
	if sum.OperatorID != operatorID || sum.Online != (name == protocol.EventOperatorOnline) {
		c.t.Fatalf("%s data for %s: %+v", name, operatorID, sum)
	}
	return sum
}

func (c *client) nextError() protocol.ErrorMsg {
	c.t.Helper()
	var e protocol.ErrorMsg
	mustUnmarshal(c.t, c.nextOf(protocol.TypeError).Payload, &e)
	return e
}

// quiet proves nothing is waiting on the socket. A client's messages are
// handled in order on its own read loop, so a subscribe with no topics is a
// barrier: its snapshot must be the very next envelope. Called on the sender
// of a message, it also proves the server finished handling that message.
func (c *client) quiet() protocol.Snapshot {
	c.t.Helper()
	c.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: []string{}})
	env := c.next()
	if env.Type != protocol.TypeSnapshot {
		c.t.Fatalf("socket not quiet: got %s %s", env.Type, env.Payload)
	}
	var snap protocol.Snapshot
	mustUnmarshal(c.t, env.Payload, &snap)
	return snap
}

// onlyRobot asserts the snapshot lists exactly one robot, the given one.
func onlyRobot(t *testing.T, snap protocol.Snapshot, robotID string) protocol.RobotSummary {
	t.Helper()
	if len(snap.Robots) != 1 || snap.Robots[0].RobotID != robotID {
		t.Fatalf("snapshot should list only %s: %+v", robotID, snap.Robots)
	}
	return snap.Robots[0]
}

// onlyOperators asserts the snapshot lists exactly the given operators (id →
// online), in any order.
func onlyOperators(t *testing.T, snap protocol.Snapshot, want map[string]bool) {
	t.Helper()
	if snap.Operators == nil {
		t.Fatalf("snapshot has no operators array: %+v", snap)
	}
	got := map[string]bool{}
	for _, o := range snap.Operators {
		got[o.OperatorID] = o.Online
	}
	if len(got) != len(snap.Operators) || len(got) != len(want) {
		t.Fatalf("snapshot operators %+v, want %v", snap.Operators, want)
	}
	for id, online := range want {
		if g, ok := got[id]; !ok || g != online {
			t.Fatalf("snapshot operators %+v, want %v", snap.Operators, want)
		}
	}
}

// TestIntegrationTwoFleetIsolation runs two unrelated fleets on one server and
// shows that nothing crosses between them: snapshots (robots and operators),
// robot and operator presence, telemetry,
// help and lease events, lease claim/renew/release, twist, signaling, channel
// messages, layers and their retained replay, and the admin API's client
// revoke. Every cross-fleet id is answered exactly like an id that does not
// exist, so ids do not leak across tenants.
func TestIntegrationTwoFleetIsolation(t *testing.T) {
	st, err := store.OpenSqlite(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := app.New(defaultConfig(), st)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Run(ctx)
	srv := httptest.NewServer(web.Handler(a.Gateway(), admin.Handler(st, isolationAdminToken, a)))
	t.Cleanup(srv.Close)
	h := &harness{t: t, url: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws", store: st}

	fleetA, err := st.CreateFleet("fleet-a")
	if err != nil {
		t.Fatal(err)
	}
	fleetB, err := st.CreateFleet("fleet-b")
	if err != nil {
		t.Fatal(err)
	}
	enroll := func(fleet store.Fleet, kind store.Kind, name string) (string, string) {
		t.Helper()
		tok, c, err := st.CreateToken(fleet.ID, kind, name)
		if err != nil {
			t.Fatal(err)
		}
		return tok, c.ID
	}
	// Same names in both fleets on purpose: nothing may key on a name.
	opATok, opAID := enroll(fleetA, store.KindOperator, "op")
	opBTok, opBID := enroll(fleetB, store.KindOperator, "op")
	robotATok, robotAID := enroll(fleetA, store.KindRobot, "bot")
	robotBTok, robotBID := enroll(fleetB, store.KindRobot, "bot")
	svcATok, svcAID := enroll(fleetA, store.KindService, "brain")
	svcBTok, svcBID := enroll(fleetB, store.KindService, "brain")
	lateATok, lateAID := enroll(fleetA, store.KindOperator, "late")
	lateBTok, lateBID := enroll(fleetB, store.KindOperator, "late")

	const ghost = "r_0000000000000000" // an id nobody was ever issued

	// --- snapshot + presence ---
	opA := h.connect(opATok)
	opA.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: allTopics})
	var snap protocol.Snapshot
	mustUnmarshal(t, opA.nextOf(protocol.TypeSnapshot).Payload, &snap)
	if sum := onlyRobot(t, snap, robotAID); sum.Presence != "offline" {
		t.Fatalf("fleet A snapshot before connect: %+v", sum)
	}
	onlyOperators(t, snap, map[string]bool{opAID: true, lateAID: false})
	opB := h.connect(opBTok)
	opB.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: allTopics})
	mustUnmarshal(t, opB.nextOf(protocol.TypeSnapshot).Payload, &snap)
	onlyRobot(t, snap, robotBID)
	onlyOperators(t, snap, map[string]bool{opBID: true, lateBID: false})
	// Fleet B's operator coming online is not fleet A's news.
	onlyOperators(t, opA.quiet(), map[string]bool{opAID: true, lateAID: false})

	manifest := protocol.Manifest{
		Drive:    &protocol.Drive{Type: "twist", MaxVMps: 1, MaxWRadps: 1},
		Channels: []string{"jobs"},
	}
	robotA := h.connect(robotATok)
	robotA.send(protocol.TypeManifest, manifest)
	robotA.quiet()
	robotB := h.connect(robotBTok)
	robotB.send(protocol.TypeManifest, manifest)
	robotB.quiet()
	svcA := h.connect(svcATok)
	svcB := h.connect(svcBTok)

	opA.nextEvent(protocol.EventRobotOnline, robotAID)
	opB.nextEvent(protocol.EventRobotOnline, robotBID)
	if sum := onlyRobot(t, opA.quiet(), robotAID); sum.Presence != "online" || sum.Manifest == nil {
		t.Fatalf("fleet A snapshot: %+v", sum)
	}
	onlyRobot(t, opB.quiet(), robotBID)

	// --- telemetry ---
	robotB.send(protocol.TypeTelemetry, protocol.Telemetry{
		Pose: &protocol.Pose{Frame: "local", FrameID: "map", XM: f(1), YM: f(2)},
	})
	robotB.quiet()
	opB.nextEvent(protocol.EventRobotTelemetry, robotBID)
	opA.quiet()

	// --- help (the events topic, and the queue entry in the snapshot) ---
	robotB.send(protocol.TypeHelpRequest, protocol.HelpRequest{Reason: "stuck"})
	robotB.quiet()
	opB.nextEvent(protocol.EventRobotHelpRequested, robotBID)
	if sum := onlyRobot(t, opA.quiet(), robotAID); sum.State != protocol.StateAutonomous || sum.Help != nil {
		t.Fatalf("fleet B's help request showed up in fleet A: %+v", sum)
	}

	// --- lease.claim across fleets: not_found, exactly like an unknown robot ---
	opA.send(protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: ghost})
	unknownClaim := opA.nextError()
	if unknownClaim.Code != protocol.ErrNotFound {
		t.Fatalf("claim of an unknown robot: %+v", unknownClaim)
	}
	opA.send(protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotBID})
	if env := opA.next(); env.Type != protocol.TypeError {
		t.Fatalf("cross-fleet claim was not refused: got %s %s", env.Type, env.Payload)
	} else {
		var got protocol.ErrorMsg
		mustUnmarshal(t, env.Payload, &got)
		if got != unknownClaim {
			t.Fatalf("cross-fleet claim %+v differs from unknown-robot claim %+v", got, unknownClaim)
		}
	}
	// Asking to steal changes nothing about that: still not_found, with no
	// lease in the answer.
	opA.send(protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotBID, Steal: true})
	if got := opA.nextError(); got != unknownClaim {
		t.Fatalf("cross-fleet claim with steal: %+v, want %+v", got, unknownClaim)
	}
	opA.quiet()
	robotB.quiet() // no lease.granted reached the robot
	if sum := onlyRobot(t, opB.quiet(), robotBID); sum.State != protocol.StateHelpRequested || sum.Lease != nil {
		t.Fatalf("cross-fleet claim changed fleet B's robot: %+v", sum)
	}

	// A claim names a robot: other client ids are not_found too, even in the
	// caller's own fleet.
	for _, id := range []string{svcAID, opAID, svcBID, opBID} {
		opA.send(protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: id})
		if got := opA.nextError(); got != unknownClaim {
			t.Fatalf("claim of non-robot %s: %+v, want %+v", id, got, unknownClaim)
		}
	}
	opA.quiet()
	svcA.quiet()
	svcB.quiet()
	opB.quiet()

	// --- fleet B's own operator takes the lease; fleet A sees none of it ---
	opB.send(protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotBID})
	var leaseB protocol.Lease
	mustUnmarshal(t, opB.nextOf(protocol.TypeLeaseGranted).Payload, &leaseB)
	opB.nextEvent(protocol.EventRobotLeaseGranted, robotBID)
	robotB.nextOf(protocol.TypeLeaseGranted)
	if leaseB.OperatorID != opBID || leaseB.RobotID != robotBID {
		t.Fatalf("fleet B lease: %+v", leaseB)
	}
	opA.quiet()

	// Fleet A cannot take it, with or without steal, and is not told it is
	// held: the answer is not_found, never the conflict (and the lease it
	// carries) that fleet B's own operators would get.
	for _, steal := range []bool{false, true} {
		opA.send(protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotBID, Steal: steal})
		if got := opA.nextError(); got != unknownClaim {
			t.Fatalf("cross-fleet claim of a held robot (steal=%v): %+v, want %+v", steal, got, unknownClaim)
		}
	}

	// --- lease.renew / lease.release / twist with fleet B's lease id ---
	const ghostLease = "ls_0000000000000000"
	crossLease := func(typ string, mk func(leaseID string) any, wantCode string) {
		t.Helper()
		opA.send(typ, mk(ghostLease))
		unknown := opA.nextError()
		if unknown.Code != wantCode {
			t.Fatalf("%s with an unknown lease: %+v, want code %s", typ, unknown, wantCode)
		}
		opA.send(typ, mk(leaseB.LeaseID))
		if got := opA.nextError(); got != unknown {
			t.Fatalf("%s with fleet B's lease %+v differs from an unknown lease %+v", typ, got, unknown)
		}
	}
	crossLease(protocol.TypeLeaseRenew, func(id string) any { return protocol.LeaseRenew{LeaseID: id} }, protocol.ErrNotFound)
	crossLease(protocol.TypeLeaseRelease, func(id string) any {
		return protocol.LeaseRelease{LeaseID: id, Resolution: "resolved"}
	}, protocol.ErrNotFound)
	crossLease(protocol.TypeTwist, func(id string) any {
		return protocol.Twist{LeaseID: id, Linear: protocol.TwistLinear{XMps: 1}}
	}, protocol.ErrNotAuthorized)
	opA.quiet()
	robotB.quiet() // no lease.granted, lease.revoked or twist
	sum := onlyRobot(t, opB.quiet(), robotBID)
	if sum.State != protocol.StateTeleop || sum.Lease == nil || *sum.Lease != leaseB {
		t.Fatalf("fleet A disturbed fleet B's lease %+v: %+v lease=%+v", leaseB, sum, sum.Lease)
	}

	// The lease still drives: twist from its holder reaches fleet B's robot.
	opB.send(protocol.TypeTwist, protocol.Twist{LeaseID: leaseB.LeaseID, Linear: protocol.TwistLinear{XMps: 0.5}})
	robotB.nextOf(protocol.TypeTwist)
	robotA.quiet()

	// --- signal ---
	sig := func(to string) protocol.Signal {
		return protocol.Signal{To: to, Kind: "offer", Data: json.RawMessage(`{"sdp":"x"}`)}
	}
	for _, tc := range []struct {
		name   string
		from   *client
		target string
		peer   *client
	}{
		{"operator A to robot B", opA, robotBID, robotB},
		{"robot A to operator B", robotA, opBID, opB},
		{"service A to service B", svcA, svcBID, svcB},
	} {
		tc.from.send(protocol.TypeSignal, sig(ghost))
		unknown := tc.from.nextError()
		if unknown.Code != protocol.ErrNotFound {
			t.Fatalf("signal %s, unknown target: %+v", tc.name, unknown)
		}
		tc.from.send(protocol.TypeSignal, sig(tc.target))
		if got := tc.from.nextError(); got != unknown {
			t.Fatalf("signal %s: %+v differs from unknown target %+v", tc.name, got, unknown)
		}
		tc.from.quiet()
		tc.peer.quiet()
	}

	// --- channel.publish: direct and broadcast ---
	svcA.send(protocol.TypeChannelPublish, protocol.ChannelPublish{Channel: "jobs", To: ghost, Data: json.RawMessage(`{"n":0}`)})
	unknownPub := svcA.nextError()
	if unknownPub.Code != protocol.ErrNotFound {
		t.Fatalf("publish to an unknown client: %+v", unknownPub)
	}
	for _, id := range []string{robotBID, opBID, svcBID} {
		svcA.send(protocol.TypeChannelPublish, protocol.ChannelPublish{Channel: "jobs", To: id, Data: json.RawMessage(`{"n":1}`)})
		if got := svcA.nextError(); got != unknownPub {
			t.Fatalf("publish to fleet B's %s: %+v differs from unknown %+v", id, got, unknownPub)
		}
	}
	// Both robots declare "jobs" and both operators subscribe to it; a fleet A
	// broadcast reaches fleet A only.
	svcA.send(protocol.TypeChannelPublish, protocol.ChannelPublish{Channel: "jobs", Broadcast: true, Data: json.RawMessage(`{"n":2}`)})
	svcA.quiet()
	for _, c := range []*client{robotA, opA} {
		var msg protocol.ChannelMessage
		mustUnmarshal(t, c.nextOf(protocol.TypeChannelMessage).Payload, &msg)
		if msg.From != svcAID || msg.Channel != "jobs" || string(msg.Data) != `{"n":2}` {
			t.Fatalf("fleet A broadcast: %+v", msg)
		}
	}
	robotB.quiet()
	opB.quiet()
	svcB.quiet()

	// --- layers: the same layer id in both fleets stays two layers ---
	declare := func(svc *client, title, data string) {
		svc.send(protocol.TypeLayerDeclare, protocol.LayerDeclare{LayerID: "shared", Kind: "geojson", Title: title})
		svc.send(protocol.TypeLayerUpdate, protocol.LayerUpdate{LayerID: "shared", Data: json.RawMessage(data)})
		svc.quiet()
	}
	expectLayer := func(c *client, title, data string) {
		t.Helper()
		var decl protocol.LayerDeclare
		mustUnmarshal(t, c.nextOf(protocol.TypeLayerDeclare).Payload, &decl)
		var upd protocol.LayerUpdate
		mustUnmarshal(t, c.nextOf(protocol.TypeLayerUpdate).Payload, &upd)
		if decl.LayerID != "shared" || decl.Title != title || upd.LayerID != "shared" || string(upd.Data) != data {
			t.Fatalf("want layer %q %s, got declare %+v update %s", title, data, decl, upd.Data)
		}
	}
	declare(svcA, "Fleet A layer", `{"fleet":"a"}`)
	expectLayer(opA, "Fleet A layer", `{"fleet":"a"}`)
	opB.quiet()
	declare(svcB, "Fleet B layer", `{"fleet":"b"}`)
	expectLayer(opB, "Fleet B layer", `{"fleet":"b"}`)
	opA.quiet()

	// Retained replay for a late subscriber is its own fleet's copy, even
	// though fleet B wrote the same layer id last.
	lateA := h.connect(lateATok)
	lateA.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: allTopics})
	mustUnmarshal(t, lateA.nextOf(protocol.TypeSnapshot).Payload, &snap)
	onlyRobot(t, snap, robotAID)
	onlyOperators(t, snap, map[string]bool{opAID: true, lateAID: true})
	expectLayer(lateA, "Fleet A layer", `{"fleet":"a"}`)
	lateA.quiet()
	// Operator presence: fleet A hears its own operator arrive, fleet B does not.
	if sum := opA.nextOperatorEvent(protocol.EventOperatorOnline, lateAID); sum.Name != "late" {
		t.Fatalf("operator.online data: %+v", sum)
	}
	opA.quiet()
	onlyOperators(t, opB.quiet(), map[string]bool{opBID: true, lateBID: false})
	lateB := h.connect(lateBTok)
	lateB.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: allTopics})
	mustUnmarshal(t, lateB.nextOf(protocol.TypeSnapshot).Payload, &snap)
	if sum := onlyRobot(t, snap, robotBID); sum.Lease == nil || *sum.Lease != leaseB {
		t.Fatalf("late fleet B snapshot: %+v", sum)
	}
	onlyOperators(t, snap, map[string]bool{opBID: true, lateBID: true})
	expectLayer(lateB, "Fleet B layer", `{"fleet":"b"}`)
	lateB.quiet()
	opB.nextOperatorEvent(protocol.EventOperatorOnline, lateBID)
	opB.quiet()
	opA.quiet()
	lateA.quiet()

	// --- admin API: revoking through fleet A cannot reach fleet B's clients ---
	revoke := func(fleet, id string) (int, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/api/admin/fleets/"+fleet+"/clients/"+id+"/revoke", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+isolationAdminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(body)
	}
	unknownStatus, unknownBody := revoke("fleet-a", ghost)
	if unknownStatus != http.StatusNotFound {
		t.Fatalf("revoke of an unknown client: %d %s", unknownStatus, unknownBody)
	}
	for _, id := range []string{robotBID, opBID, svcBID} {
		if status, body := revoke("fleet-a", id); status != unknownStatus || body != unknownBody {
			t.Fatalf("revoke of fleet B's %s through fleet A: %d %s, want %d %s", id, status, body, unknownStatus, unknownBody)
		}
	}
	// Fleet B's clients are still connected, still enrolled, lease intact.
	robotB.quiet()
	svcB.quiet()
	if sum := onlyRobot(t, opB.quiet(), robotBID); sum.Presence != "online" || sum.Lease == nil || *sum.Lease != leaseB {
		t.Fatalf("fleet B after cross-fleet revoke attempts: %+v", sum)
	}
	if c, ok, err := st.AuthToken(robotBTok); err != nil || !ok || c.ID != robotBID {
		t.Fatalf("fleet B robot token after cross-fleet revoke attempts: ok=%v err=%v", ok, err)
	}

	// --- operator loss and robot loss in fleet B stay in fleet B ---
	opB.ws.Close(websocket.StatusNormalClosure, "gone")
	var revoked protocol.LeaseRevoked
	mustUnmarshal(t, robotB.nextOf(protocol.TypeLeaseRevoked).Payload, &revoked)
	if revoked.LeaseID != leaseB.LeaseID || revoked.Reason != protocol.RevokeOperatorLost {
		t.Fatalf("operator loss revocation: %+v", revoked)
	}
	lateB.nextEvent(protocol.EventRobotLeaseRevoked, robotBID)
	lateB.nextOperatorEvent(protocol.EventOperatorOffline, opBID)
	onlyOperators(t, lateB.quiet(), map[string]bool{opBID: false, lateBID: true})
	robotB.ws.Close(websocket.StatusNormalClosure, "gone")
	lateB.nextEvent(protocol.EventRobotOffline, robotBID)
	lateB.quiet()

	// Fleet A heard none of it, and its own flow still works end to end.
	onlyOperators(t, opA.quiet(), map[string]bool{opAID: true, lateAID: true})
	lateA.quiet()
	svcA.quiet()
	if sum := onlyRobot(t, robotA.quiet(), robotAID); sum.State != protocol.StateAutonomous || sum.Lease != nil {
		t.Fatalf("fleet A robot after fleet B's story: %+v", sum)
	}
	opA.send(protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotAID})
	var leaseA protocol.Lease
	mustUnmarshal(t, opA.nextOf(protocol.TypeLeaseGranted).Payload, &leaseA)
	robotA.nextOf(protocol.TypeLeaseGranted)
	if leaseA.OperatorID != opAID || leaseA.RobotID != robotAID {
		t.Fatalf("fleet A lease: %+v", leaseA)
	}
	lateB.quiet()
}
