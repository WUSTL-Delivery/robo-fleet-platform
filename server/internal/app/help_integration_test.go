package app_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"fleetplatform/sdk/go/protocol"
	"fleetplatform/server/internal/store"
)

// snapshotOf subscribes (again) and returns the one robot's summary.
func (c *client) snapshotOf(robotID string) protocol.RobotSummary {
	c.t.Helper()
	c.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: []string{"events"}})
	var snap protocol.Snapshot
	mustUnmarshal(c.t, c.expect(protocol.TypeSnapshot).Payload, &snap)
	for _, r := range snap.Robots {
		if r.RobotID == robotID {
			return r
		}
	}
	c.t.Fatalf("robot %s not in snapshot: %+v", robotID, snap)
	return protocol.RobotSummary{}
}

// TestIntegrationHelpDetails: the intervention queue entry (reason, context,
// requested_at_ms) reaches a subscriber that connects after the help.request,
// arrives live on the events that put a robot in the queue, and survives a
// claim followed by lease expiry unchanged.
func TestIntegrationHelpDetails(t *testing.T) {
	cfg := defaultConfig()
	cfg.LeaseTTL = 150 * time.Millisecond
	h := newHarness(t, cfg)

	robotToken, robotClient, err := h.store.CreateToken(h.fleet.ID, store.KindRobot, "sim-01")
	if err != nil {
		t.Fatal(err)
	}
	tokenFor := func(kind store.Kind, name string) string {
		tok, _, err := h.store.CreateToken(h.fleet.ID, kind, name)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	robotID := robotClient.ID

	// A watcher is subscribed before anything happens: it sees the live events.
	watcher := h.connect(tokenFor(store.KindOperator, "op-watch"))
	if sum := watcher.snapshotOf(robotID); sum.State != protocol.StateAutonomous || sum.Help != nil {
		t.Fatalf("before help: %+v", sum)
	}

	robot := h.connect(robotToken)
	before := time.Now().UnixMilli()
	robot.send(protocol.TypeHelpRequest, protocol.HelpRequest{
		Reason: "nav_goal_failed", Context: map[string]any{"attempts": 3, "leg": "b7"},
	})

	// The live event carries the whole entry, including requested_at_ms.
	ev := watcher.expectEvent(protocol.EventRobotHelpRequested)
	after := time.Now().UnixMilli()
	var live protocol.HelpDetails
	mustUnmarshal(t, ev.Data, &live)
	wantContext := map[string]any{"attempts": float64(3), "leg": "b7"}
	if ev.RobotID != robotID || live.Reason != "nav_goal_failed" || !reflect.DeepEqual(live.Context, wantContext) {
		t.Fatalf("help event: robot=%s data=%s", ev.RobotID, ev.Data)
	}
	if live.RequestedAtMs < before || live.RequestedAtMs > after {
		t.Fatalf("requested_at_ms = %d, want server time within [%d, %d]", live.RequestedAtMs, before, after)
	}

	// A repeat request does not reset the entry.
	robot.send(protocol.TypeHelpRequest, protocol.HelpRequest{Reason: "asked again"})

	// A subscriber that connects only now gets the same entry in its snapshot.
	late := h.connect(tokenFor(store.KindService, "late-console"))
	sum := late.snapshotOf(robotID)
	if sum.State != protocol.StateHelpRequested || sum.Help == nil || !reflect.DeepEqual(*sum.Help, live) {
		t.Fatalf("late snapshot: state=%s help=%+v, want %+v", sum.State, sum.Help, live)
	}

	// An operator claims: the robot leaves the queue, so the snapshot drops help.
	claimer := h.connect(tokenFor(store.KindOperator, "op-claim"))
	claimer.send(protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotID})
	claimer.expect(protocol.TypeLeaseGranted)
	watcher.expectEvent(protocol.EventRobotLeaseGranted)
	if sum := late.snapshotOf(robotID); sum.State != protocol.StateTeleop || sum.Help != nil {
		t.Fatalf("teleop snapshot: state=%s help=%+v", sum.State, sum.Help)
	}

	// No renewals: the lease expires and the robot is back in the queue. The
	// revocation carries the ORIGINAL entry, both live and direct.
	var evRevoked protocol.LeaseRevoked
	mustUnmarshal(t, watcher.expectEvent(protocol.EventRobotLeaseRevoked).Data, &evRevoked)
	if evRevoked.Reason != protocol.RevokeExpired || evRevoked.Help == nil || !reflect.DeepEqual(*evRevoked.Help, live) {
		t.Fatalf("lease_revoked event: %+v help=%+v, want help %+v", evRevoked, evRevoked.Help, live)
	}
	var direct protocol.LeaseRevoked
	mustUnmarshal(t, claimer.expect(protocol.TypeLeaseRevoked).Payload, &direct)
	if direct.Help == nil || !reflect.DeepEqual(*direct.Help, live) {
		t.Fatalf("operator's lease.revoked help = %+v, want %+v", direct.Help, live)
	}
	mustUnmarshal(t, robot.expect(protocol.TypeLeaseRevoked).Payload, &direct)
	if direct.Help == nil || !reflect.DeepEqual(*direct.Help, live) {
		t.Fatalf("robot's lease.revoked help = %+v, want %+v", direct.Help, live)
	}

	// And a fresh snapshot after the expiry still shows it.
	sum = late.snapshotOf(robotID)
	if sum.State != protocol.StateHelpRequested || sum.Help == nil || !reflect.DeepEqual(*sum.Help, live) {
		t.Fatalf("snapshot after expiry: state=%s help=%+v, want %+v", sum.State, sum.Help, live)
	}
}

// TestIntegrationHelpDetailsSurviveOperatorLoss: the other way back into the
// queue. The operator's socket dies; the entry is the original one.
func TestIntegrationHelpDetailsSurviveOperatorLoss(t *testing.T) {
	h := newHarness(t, defaultConfig())
	robotToken, robotClient, err := h.store.CreateToken(h.fleet.ID, store.KindRobot, "sim-01")
	if err != nil {
		t.Fatal(err)
	}
	opToken, _, err := h.store.CreateToken(h.fleet.ID, store.KindOperator, "op-1")
	if err != nil {
		t.Fatal(err)
	}
	watchToken, _, err := h.store.CreateToken(h.fleet.ID, store.KindOperator, "op-watch")
	if err != nil {
		t.Fatal(err)
	}

	watcher := h.connect(watchToken)
	watcher.snapshotOf(robotClient.ID)
	robot := h.connect(robotToken)
	robot.send(protocol.TypeHelpRequest, protocol.HelpRequest{Reason: "stuck"})
	var asked protocol.HelpDetails
	mustUnmarshal(t, watcher.expectEvent(protocol.EventRobotHelpRequested).Data, &asked)

	operator := h.connect(opToken)
	operator.send(protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotClient.ID})
	operator.expect(protocol.TypeLeaseGranted)
	operator.ws.CloseNow()

	var revoked protocol.LeaseRevoked
	mustUnmarshal(t, watcher.expectEvent(protocol.EventRobotLeaseRevoked).Data, &revoked)
	if revoked.Reason != protocol.RevokeOperatorLost || revoked.Help == nil || !reflect.DeepEqual(*revoked.Help, asked) {
		t.Fatalf("lease_revoked: %+v help=%+v, want help %+v", revoked, revoked.Help, asked)
	}
	if sum := watcher.snapshotOf(robotClient.ID); sum.Help == nil || !reflect.DeepEqual(*sum.Help, asked) || asked.Context != nil {
		t.Fatalf("snapshot after operator loss: %+v, want %+v with no context", sum.Help, asked)
	}
}

// TestIntegrationHelpRequestReasonBounds: a reason the snapshot schema could
// not carry is refused instead of queued.
func TestIntegrationHelpRequestReasonBounds(t *testing.T) {
	h := newHarness(t, defaultConfig())
	robotToken, robotClient, err := h.store.CreateToken(h.fleet.ID, store.KindRobot, "sim-01")
	if err != nil {
		t.Fatal(err)
	}
	robot := h.connect(robotToken)
	for _, reason := range []string{"", strings.Repeat("x", 257)} {
		robot.send(protocol.TypeHelpRequest, protocol.HelpRequest{Reason: reason})
		env, err := robot.recv()
		if err != nil {
			t.Fatal(err)
		}
		var e protocol.ErrorMsg
		mustUnmarshal(t, env.Payload, &e)
		if env.Type != protocol.TypeError || e.Code != protocol.ErrInvalidMessage {
			t.Fatalf("reason of length %d: got %s %s", len(reason), env.Type, env.Payload)
		}
	}
	if sum := robot.snapshotOf(robotClient.ID); sum.State != protocol.StateAutonomous || sum.Help != nil {
		t.Fatalf("refused requests must not queue the robot: %+v", sum)
	}
}
