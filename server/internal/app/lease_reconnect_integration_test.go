// A robot's lease across its own disconnects (protocol/README.md, "A robot's
// lease at connect"): the lease survives the robot dropping, the welcome of
// every robot connection says which lease the server holds for it, and a lease
// that dies while the robot is away is still announced to the fleet.
package app_test

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"fleetplatform/sdk/go/protocol"
	"fleetplatform/server/internal/store"
)

// connectWelcome dials, says hello and returns the welcome payload as raw
// members, so a test can tell "lease": null from no lease member at all.
func (h *harness) connectWelcome(token string) (*client, map[string]json.RawMessage) {
	h.t.Helper()
	c := h.dial()
	c.send(protocol.TypeHello, protocol.Hello{Token: token})
	var w map[string]json.RawMessage
	mustUnmarshal(h.t, c.nextOf(protocol.TypeWelcome).Payload, &w)
	return c, w
}

// welcomeLease reads welcome.lease: the lease, or nil for null. It fails the
// test when the member is absent: every robot welcome must state it.
func welcomeLease(t *testing.T, w map[string]json.RawMessage) *protocol.Lease {
	t.Helper()
	raw, ok := w["lease"]
	if !ok {
		t.Fatalf("robot welcome has no lease member: %v", w)
	}
	if string(raw) == "null" {
		return nil
	}
	var l protocol.Lease
	mustUnmarshal(t, raw, &l)
	return &l
}

func (c *client) drop() {
	c.ws.CloseNow()
}

// leasedRobot is the shared opening: a watcher subscribed to everything, a
// robot, and an operator who has claimed it.
type leasedRobot struct {
	robotTok, robotID string
	robot             *client
	op                *client
	opID              string
	watcher           *client
	lease             protocol.Lease
}

func (h *harness) leasedRobot() leasedRobot {
	h.t.Helper()
	var s leasedRobot
	s.robotTok, s.robotID = h.token(store.KindRobot, "bot")
	opTok, opID := h.token(store.KindOperator, "driver")
	s.opID = opID
	s.watcher = h.connectAs(store.KindOperator, "watcher")
	s.watcher.subscribeTo("presence", "events")

	var w map[string]json.RawMessage
	s.robot, w = h.connectWelcome(s.robotTok)
	if l := welcomeLease(h.t, w); l != nil {
		h.t.Fatalf("a robot nobody has claimed was welcomed with lease %+v", l)
	}
	s.watcher.expectEvent(protocol.EventRobotOnline)

	s.op = h.connect(opTok)
	s.op.send(protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: s.robotID})
	mustUnmarshal(h.t, s.op.expect(protocol.TypeLeaseGranted).Payload, &s.lease)
	s.robot.nextOf(protocol.TypeLeaseGranted)
	s.watcher.expectEvent(protocol.EventRobotLeaseGranted)
	return s
}

// Only a robot's welcome states a lease; operators and services hold none.
func TestIntegrationWelcomeLeaseIsForRobotsOnly(t *testing.T) {
	h := newHarness(t, defaultConfig())
	for _, kind := range []store.Kind{store.KindOperator, store.KindService} {
		tok, _ := h.token(kind, "not-a-robot")
		_, w := h.connectWelcome(tok)
		if raw, ok := w["lease"]; ok {
			t.Fatalf("%s welcome carries lease %s", kind, raw)
		}
	}
}

// The server does not touch a lease when the robot's connection drops. A robot
// that is back inside the TTL is welcomed with that same lease and is driven
// again.
func TestIntegrationRobotReconnectsInsideLeaseTTL(t *testing.T) {
	h := newHarness(t, defaultConfig())
	s := h.leasedRobot()

	s.robot.drop()
	s.watcher.expectEvent(protocol.EventRobotOffline)
	sum := s.watcher.snapshotOf(s.robotID)
	if sum.Presence != "offline" || sum.State != protocol.StateTeleop || sum.Lease == nil || sum.Lease.LeaseID != s.lease.LeaseID {
		t.Fatalf("offline robot should keep its lease: %+v", sum)
	}

	robot, w := h.connectWelcome(s.robotTok)
	got := welcomeLease(t, w)
	if got == nil || *got != s.lease {
		t.Fatalf("welcome lease = %+v, want %+v", got, s.lease)
	}
	// Nothing else is waiting: no revocation, no second grant.
	robot.quiet()

	s.op.send(protocol.TypeTwist, protocol.Twist{LeaseID: s.lease.LeaseID, Linear: protocol.TwistLinear{XMps: 0.4}})
	var tw protocol.Twist
	mustUnmarshal(t, robot.nextOf(protocol.TypeTwist).Payload, &tw)
	if tw.LeaseID != s.lease.LeaseID {
		t.Fatalf("relayed twist bears %s", tw.LeaseID)
	}
}

// A lease that expires while its robot is offline is announced to the fleet
// like any other, and the robot is told it holds nothing when it returns.
func TestIntegrationLeaseExpiresWhileRobotOffline(t *testing.T) {
	cfg := defaultConfig()
	cfg.LeaseTTL = 300 * time.Millisecond
	h := newHarness(t, cfg)
	s := h.leasedRobot()

	s.robot.drop()
	s.watcher.expectEvent(protocol.EventRobotOffline)

	// No renewals. The fleet hears the expiry although the robot is not there.
	ev := s.watcher.expectEvent(protocol.EventRobotLeaseRevoked)
	var rv protocol.LeaseRevoked
	mustUnmarshal(t, ev.Data, &rv)
	if ev.RobotID != s.robotID || ev.OperatorID != "" || rv.LeaseID != s.lease.LeaseID || rv.RobotID != s.robotID || rv.Reason != protocol.RevokeExpired {
		t.Fatalf("robot.lease_revoked for an offline robot: %+v data %+v", ev, rv)
	}
	// The robot never asked, so the server opened the queue entry itself.
	if rv.Help == nil || rv.Help.Reason != protocol.RevokeExpired || rv.Help.RequestedAtMs == 0 {
		t.Fatalf("revocation should carry the queue entry: %+v", rv.Help)
	}
	var opRv protocol.LeaseRevoked
	mustUnmarshal(t, s.op.expect(protocol.TypeLeaseRevoked).Payload, &opRv)
	if opRv.Reason != protocol.RevokeExpired || opRv.LeaseID != s.lease.LeaseID {
		t.Fatalf("operator revocation: %+v", opRv)
	}
	sum := s.watcher.snapshotOf(s.robotID)
	if sum.Presence != "offline" || sum.State != protocol.StateHelpRequested || sum.Lease != nil || sum.Help == nil {
		t.Fatalf("offline robot after expiry: %+v", sum)
	}

	robot, w := h.connectWelcome(s.robotTok)
	if l := welcomeLease(t, w); l != nil {
		t.Fatalf("robot welcomed with a lease the server revoked: %+v", l)
	}
	// The old lease id moves nothing on the bus either: the server refuses it.
	s.op.sendWithID("late", protocol.TypeTwist, protocol.Twist{LeaseID: s.lease.LeaseID, Linear: protocol.TwistLinear{XMps: 0.4}})
	var e protocol.ErrorMsg
	mustUnmarshal(t, s.op.expectError().Payload, &e)
	if e.Code != protocol.ErrNotAuthorized || e.Ref != "late" {
		t.Fatalf("twist on the expired lease: %+v", e)
	}
	robot.quiet()
}

// The operator leaving while the robot is offline is announced the same way.
func TestIntegrationOperatorLostWhileRobotOffline(t *testing.T) {
	h := newHarness(t, defaultConfig())
	s := h.leasedRobot()

	s.robot.drop()
	s.watcher.expectEvent(protocol.EventRobotOffline)
	s.op.drop()

	ev := s.watcher.expectEvent(protocol.EventRobotLeaseRevoked)
	var rv protocol.LeaseRevoked
	mustUnmarshal(t, ev.Data, &rv)
	if ev.RobotID != s.robotID || rv.LeaseID != s.lease.LeaseID || rv.Reason != protocol.RevokeOperatorLost || rv.Help == nil {
		t.Fatalf("robot.lease_revoked for an offline robot: %+v data %+v", ev, rv)
	}

	_, w := h.connectWelcome(s.robotTok)
	if l := welcomeLease(t, w); l != nil {
		t.Fatalf("robot welcomed with a lease the server revoked: %+v", l)
	}
}

// A handback while the robot is offline: the release event was always sent;
// the robot now learns of it from its next welcome.
func TestIntegrationLeaseReleasedWhileRobotOffline(t *testing.T) {
	h := newHarness(t, defaultConfig())
	s := h.leasedRobot()

	s.robot.drop()
	s.watcher.expectEvent(protocol.EventRobotOffline)
	s.op.send(protocol.TypeLeaseRelease, protocol.LeaseRelease{LeaseID: s.lease.LeaseID, Resolution: "resolved"})
	s.watcher.expectEvent(protocol.EventRobotLeaseReleased)

	_, w := h.connectWelcome(s.robotTok)
	if l := welcomeLease(t, w); l != nil {
		t.Fatalf("robot welcomed with a released lease: %+v", l)
	}
}

// expectError reads until an error envelope arrives.
func (c *client) expectError() protocol.Envelope {
	c.t.Helper()
	for i := 0; i < 50; i++ {
		env, err := c.recv()
		if err != nil {
			c.t.Fatalf("waiting for an error: %v", err)
		}
		if env.Type == protocol.TypeError {
			return env
		}
	}
	c.t.Fatal("gave up waiting for an error")
	return protocol.Envelope{}
}

// TestIntegrationLeaseStateOrderedWithClaims: while an operator claims and
// hands back as fast as it can, the robot reconnects over and over. Whatever
// interleaving happens, the lease a robot arrives at by reading its newest
// connection in order (welcome.lease, then every lease.granted and
// lease.revoked after it) must be the lease the server holds. A welcome that
// could cross a grant or a revocation would leave the two apart.
func TestIntegrationLeaseStateOrderedWithClaims(t *testing.T) {
	h := newHarness(t, defaultConfig())
	robotTok, robotID := h.token(store.KindRobot, "bot")
	opTok, _ := h.token(store.KindOperator, "driver")
	op := h.connect(opTok)

	for round := 0; round < 30; round++ {
		var wg sync.WaitGroup
		stop := make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			held := ""
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				if held != "" && i%2 == 0 {
					// A successful release is not answered.
					if err := op.write("", protocol.TypeLeaseRelease, protocol.LeaseRelease{LeaseID: held, Resolution: "resolved"}); err != nil {
						t.Errorf("operator write: %v", err)
						return
					}
					held = ""
					continue
				}
				if err := op.write("", protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotID}); err != nil {
					t.Errorf("operator write: %v", err)
					return
				}
				for {
					env, err := op.recv()
					if err != nil {
						t.Errorf("operator read: %v", err)
						return
					}
					if env.Type == protocol.TypeLeaseGranted {
						var l protocol.Lease
						if json.Unmarshal(env.Payload, &l) == nil {
							held = l.LeaseID
						}
						break
					}
					if env.Type == protocol.TypeError {
						break // the robot was between connections: not_found
					}
				}
			}
		}()

		var robot *client
		var w map[string]json.RawMessage
		for i := 0; i < 8; i++ {
			robot, w = h.connectWelcome(robotTok)
		}
		close(stop)
		wg.Wait()
		// Barriers: the server has finished everything the operator sent, and
		// everything it sent the robot before the robot's snapshot is read here.
		op.quiet()

		view := ""
		if l := welcomeLease(t, w); l != nil {
			view = l.LeaseID
		}
		robot.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: []string{}})
		var snap protocol.Snapshot
		for snap.Robots == nil {
			env := robot.next()
			switch env.Type {
			case protocol.TypeLeaseGranted:
				var l protocol.Lease
				mustUnmarshal(t, env.Payload, &l)
				view = l.LeaseID
			case protocol.TypeLeaseRevoked:
				var rv protocol.LeaseRevoked
				mustUnmarshal(t, env.Payload, &rv)
				if rv.LeaseID == view {
					view = ""
				}
			case protocol.TypeSnapshot:
				mustUnmarshal(t, env.Payload, &snap)
			default:
				t.Fatalf("round %d: unexpected %s on the robot's socket: %s", round, env.Type, env.Payload)
			}
		}
		server := ""
		for _, r := range snap.Robots {
			if r.RobotID == robotID && r.Lease != nil {
				server = r.Lease.LeaseID
			}
		}
		if view != server {
			t.Fatalf("round %d: the robot believes it holds %q, the server holds %q", round, view, server)
		}
	}
}
