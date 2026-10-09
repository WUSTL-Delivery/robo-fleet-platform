package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/coder/websocket"

	"fleetplatform/sdk/go/protocol"
	"fleetplatform/server/internal/store"
)

// sendWithID is send with a correlation id, so the reply's ref can be checked.
func (c *client) sendWithID(id, typ string, payload any) {
	c.t.Helper()
	if err := c.write(id, typ, payload); err != nil {
		c.t.Fatal(err)
	}
}

// write is safe to call from a goroutine other than the test's.
func (c *client) write(id, typ string, payload any) error {
	env := protocol.Msg(typ, payload)
	env.ID = id
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, data)
}

// TestIntegrationClaimOnHeldLease: a plain claim on a robot another operator is
// driving is refused with a conflict that names the lease in the way, and
// nobody else hears about it; the same claim with steal revokes the holder
// (reason stolen) and reissues.
func TestIntegrationClaimOnHeldLease(t *testing.T) {
	h := newHarness(t, defaultConfig())
	robotTok, robotID := h.token(store.KindRobot, "sim-01")
	adaTok, adaID := h.token(store.KindOperator, "ada")
	bobTok, bobID := h.token(store.KindOperator, "bob")

	robot := h.connect(robotTok)
	ada := h.connect(adaTok)
	bob := h.connect(bobTok)
	ada.subscribeTo("events")
	bob.subscribeTo("events")

	robot.send(protocol.TypeHelpRequest, protocol.HelpRequest{Reason: "stuck"})
	ada.nextEvent(protocol.EventRobotHelpRequested, robotID)
	bob.nextEvent(protocol.EventRobotHelpRequested, robotID)

	// Ada claims from the queue.
	ada.sendWithID("claim-ada", protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotID})
	var adaLease protocol.Lease
	mustUnmarshal(t, ada.nextOf(protocol.TypeLeaseGranted).Payload, &adaLease)
	ada.nextEvent(protocol.EventRobotLeaseGranted, robotID)
	bob.nextEvent(protocol.EventRobotLeaseGranted, robotID)
	robot.nextOf(protocol.TypeLeaseGranted)
	if adaLease.OperatorID != adaID {
		t.Fatalf("ada's lease: %+v", adaLease)
	}

	// Bob's plain claim is refused. The conflict carries ada's lease, so his
	// console can say who is driving without a fresh snapshot.
	bob.sendWithID("claim-bob", protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotID})
	refused := bob.nextError()
	if refused.Code != protocol.ErrConflict || refused.Ref != "claim-bob" {
		t.Fatalf("plain claim on a held lease: %+v, want conflict with ref claim-bob", refused)
	}
	if refused.Lease == nil || *refused.Lease != adaLease {
		t.Fatalf("conflict carries lease %+v, want ada's %+v", refused.Lease, adaLease)
	}
	// Nothing else happened: no lease.revoked, no lease.granted, no event, to
	// anyone, and the robot is still ada's.
	bob.quiet()
	robot.quiet()
	if sum := onlyRobot(t, ada.quiet(), robotID); sum.State != protocol.StateTeleop || sum.Lease == nil || *sum.Lease != adaLease {
		t.Fatalf("a refused claim disturbed the lease: %+v lease=%+v", sum, sum.Lease)
	}
	// Ada's lease still drives and still renews.
	ada.send(protocol.TypeTwist, protocol.Twist{LeaseID: adaLease.LeaseID, Linear: protocol.TwistLinear{XMps: 0.5}})
	robot.nextOf(protocol.TypeTwist)
	ada.send(protocol.TypeLeaseRenew, protocol.LeaseRenew{LeaseID: adaLease.LeaseID})
	mustUnmarshal(t, ada.nextOf(protocol.TypeLeaseGranted).Payload, &adaLease)

	// An explicit steal: false is the same as leaving it out.
	bob.sendWithID("claim-bob-2", protocol.TypeLeaseClaim, json.RawMessage(`{"robot_id":"`+robotID+`","steal":false}`))
	if got := bob.nextError(); got.Code != protocol.ErrConflict || got.Lease == nil || *got.Lease != adaLease {
		t.Fatalf("claim with steal:false on a held lease: %+v lease=%+v", got, got.Lease)
	}

	// With steal, bob takes the wheel: ada's lease is revoked (stolen, no queue
	// entry, the robot stays in TELEOP) and a new one is issued.
	bob.sendWithID("steal-bob", protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotID, Steal: true})
	var bobLease protocol.Lease
	mustUnmarshal(t, bob.nextOf(protocol.TypeLeaseGranted).Payload, &bobLease)
	if bobLease.OperatorID != bobID || bobLease.LeaseID == adaLease.LeaseID {
		t.Fatalf("steal must reissue to bob: %+v (ada had %+v)", bobLease, adaLease)
	}
	var robotLease protocol.Lease
	mustUnmarshal(t, robot.nextOf(protocol.TypeLeaseGranted).Payload, &robotLease)
	if robotLease != bobLease {
		t.Fatalf("robot was granted %+v, want %+v", robotLease, bobLease)
	}
	var direct protocol.LeaseRevoked
	mustUnmarshal(t, ada.nextOf(protocol.TypeLeaseRevoked).Payload, &direct)
	if direct.LeaseID != adaLease.LeaseID || direct.Reason != protocol.RevokeStolen || direct.Help != nil {
		t.Fatalf("ada's lease.revoked: %+v", direct)
	}
	for _, c := range []*client{ada, bob} {
		var ev protocol.LeaseRevoked
		mustUnmarshal(t, c.nextEvent(protocol.EventRobotLeaseRevoked, robotID).Data, &ev)
		if ev.LeaseID != adaLease.LeaseID || ev.Reason != protocol.RevokeStolen {
			t.Fatalf("robot.lease_revoked event: %+v", ev)
		}
		c.nextEvent(protocol.EventRobotLeaseGranted, robotID)
	}

	// Ada's old lease is dead; her plain claim to get it back is refused too.
	ada.send(protocol.TypeTwist, protocol.Twist{LeaseID: adaLease.LeaseID, Linear: protocol.TwistLinear{XMps: 0.5}})
	if got := ada.nextError(); got.Code != protocol.ErrNotAuthorized {
		t.Fatalf("twist on a stolen lease: %+v", got)
	}
	ada.send(protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotID})
	if got := ada.nextError(); got.Code != protocol.ErrConflict || got.Lease == nil || *got.Lease != bobLease {
		t.Fatalf("ada's plain claim after the steal: %+v lease=%+v", got, got.Lease)
	}

	// Re-claiming a robot you already hold needs no steal.
	bob.send(protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotID})
	var again protocol.Lease
	mustUnmarshal(t, bob.nextOf(protocol.TypeLeaseGranted).Payload, &again)
	if again.OperatorID != bobID {
		t.Fatalf("bob's re-claim: %+v", again)
	}
}

// TestIntegrationRacingPlainClaims: two operators claim the same queued robot
// at the same moment, neither asking to steal. Exactly one is granted; the
// other gets conflict naming the winner's lease; no lease.revoked is sent to
// anyone.
func TestIntegrationRacingPlainClaims(t *testing.T) {
	h := newHarness(t, defaultConfig())
	robotTok, robotID := h.token(store.KindRobot, "sim-01")
	robot := h.connect(robotTok)
	ops := make([]*client, 2)
	ids := make([]string, 2)
	for i := range ops {
		tok, id := h.token(store.KindOperator, fmt.Sprintf("op-%d", i))
		ops[i], ids[i] = h.connect(tok), id
	}
	// A third operator watches the event stream: it must show one grant and no
	// revocation per round.
	watcherTok, _ := h.token(store.KindOperator, "watcher")
	watcher := h.connect(watcherTok)
	watcher.subscribeTo("events")

	wins := make([]int, 2)
	for round := 0; round < 25; round++ {
		robot.send(protocol.TypeHelpRequest, protocol.HelpRequest{Reason: "stuck"})
		watcher.nextEvent(protocol.EventRobotHelpRequested, robotID)

		start := make(chan struct{})
		errs := make(chan error, len(ops))
		for i, c := range ops {
			go func() {
				<-start
				errs <- c.write(fmt.Sprintf("claim-%d-%d", round, i), protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotID})
			}()
		}
		close(start)
		for range ops {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
		}

		// Each operator gets exactly one answer: its grant or its refusal.
		var winner *protocol.Lease
		var refusals []protocol.ErrorMsg
		for i, c := range ops {
			switch env := c.next(); env.Type {
			case protocol.TypeLeaseGranted:
				if winner != nil {
					t.Fatalf("round %d: both claims were granted", round)
				}
				var l protocol.Lease
				mustUnmarshal(t, env.Payload, &l)
				if l.OperatorID != ids[i] || l.RobotID != robotID {
					t.Fatalf("round %d: grant %+v sent to %s", round, l, ids[i])
				}
				winner = &l
				wins[i]++
			case protocol.TypeError:
				var e protocol.ErrorMsg
				mustUnmarshal(t, env.Payload, &e)
				if e.Code != protocol.ErrConflict || e.Ref != fmt.Sprintf("claim-%d-%d", round, i) {
					t.Fatalf("round %d: refusal %+v", round, e)
				}
				refusals = append(refusals, e)
			default:
				t.Fatalf("round %d: operator %d got %s %s", round, i, env.Type, env.Payload)
			}
		}
		if winner == nil || len(refusals) != 1 {
			t.Fatalf("round %d: winner=%+v refusals=%+v, want exactly one of each", round, winner, refusals)
		}
		if refusals[0].Lease == nil || *refusals[0].Lease != *winner {
			t.Fatalf("round %d: conflict names %+v, want the winner's %+v", round, refusals[0].Lease, *winner)
		}

		// The robot heard one grant, the winner's, and nothing else; neither
		// operator has a lease.revoked (or anything) waiting.
		var robotLease protocol.Lease
		mustUnmarshal(t, robot.nextOf(protocol.TypeLeaseGranted).Payload, &robotLease)
		if robotLease != *winner {
			t.Fatalf("round %d: robot was granted %+v, want %+v", round, robotLease, *winner)
		}
		robot.quiet()
		for _, c := range ops {
			c.quiet()
		}
		// The event stream shows the one grant and then nothing until the
		// handback below: a lease_revoked here would fail nextEvent.
		watcher.nextEvent(protocol.EventRobotLeaseGranted, robotID)

		// Hand back so the next round starts from a free robot.
		for i, c := range ops {
			if winner.OperatorID == ids[i] {
				c.send(protocol.TypeLeaseRelease, protocol.LeaseRelease{LeaseID: winner.LeaseID, Resolution: "resolved"})
				c.quiet()
			}
		}
		var released protocol.LeaseRevoked
		mustUnmarshal(t, robot.nextOf(protocol.TypeLeaseRevoked).Payload, &released)
		if released.Reason != protocol.RevokeReleased {
			t.Fatalf("round %d: robot's lease.revoked = %+v, want the handback", round, released)
		}
		watcher.nextEvent(protocol.EventRobotLeaseReleased, robotID)
	}
	t.Logf("wins per operator over 25 rounds: %v", wins)
}
