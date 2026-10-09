package ops

import (
	"testing"
	"time"

	"fleetplatform/sdk/go/protocol"
)

const ttl = 15 * time.Second

func newTestOps() (*Ops, *time.Time) {
	current := time.Unix(1_755_100_000, 0)
	o := New(func() time.Time { return current }, ttl)
	return o, &current
}

func TestHelpRequestTransitions(t *testing.T) {
	o, _ := newTestOps()
	if !o.RequestHelp("r1") {
		t.Fatal("AUTONOMOUS → HELP_REQUESTED should transition")
	}
	if o.RequestHelp("r1") {
		t.Fatal("HELP_REQUESTED → HELP_REQUESTED should be a no-op")
	}
	if state, _ := o.StateOf("r1"); state != protocol.StateHelpRequested {
		t.Fatalf("state = %s", state)
	}
}

func TestClaimGrantsExclusiveLease(t *testing.T) {
	o, now := newTestOps()
	o.RequestHelp("r1")
	lease, stolen := o.Claim("r1", "op1")
	if stolen != nil {
		t.Fatal("first claim should not steal")
	}
	if state, l := o.StateOf("r1"); state != protocol.StateTeleop || l == nil || l.ID != lease.ID {
		t.Fatalf("state=%s lease=%v", state, l)
	}
	if got, want := lease.ExpiresAt, now.Add(ttl); !got.Equal(want) {
		t.Fatalf("expiry = %v, want %v", got, want)
	}

	// Proactive claim from AUTONOMOUS is also legal.
	l2, stolen2 := o.Claim("r2", "op1")
	if stolen2 != nil || l2.RobotID != "r2" {
		t.Fatalf("proactive claim failed: %+v %+v", l2, stolen2)
	}
}

func TestStealRevokesAndReissues(t *testing.T) {
	o, _ := newTestOps()
	first, _ := o.Claim("r1", "op1")
	second, stolen := o.Claim("r1", "op2")
	if stolen == nil || stolen.Lease.ID != first.ID || stolen.Reason != protocol.RevokeStolen {
		t.Fatalf("steal must revoke the old lease: %+v", stolen)
	}
	if second.ID == first.ID {
		t.Fatal("steal must reissue, never share")
	}
	if _, err := o.Renew(first.ID, "op1"); err == nil {
		t.Fatal("old lease must be dead after steal")
	}
	if state, l := o.StateOf("r1"); state != protocol.StateTeleop || l.OperatorID != "op2" {
		t.Fatalf("robot should stay TELEOP under new holder, got %s %+v", state, l)
	}
}

func TestRenewExtendsOnlyForHolder(t *testing.T) {
	o, now := newTestOps()
	lease, _ := o.Claim("r1", "op1")
	*now = now.Add(10 * time.Second)
	renewed, err := o.Renew(lease.ID, "op1")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := renewed.ExpiresAt, now.Add(ttl); !got.Equal(want) {
		t.Fatalf("renewed expiry = %v, want %v", got, want)
	}
	if _, err := o.Renew(lease.ID, "op2"); err != ErrNotHolder {
		t.Fatalf("non-holder renew: err = %v", err)
	}
}

func TestReleaseReturnsToAutonomous(t *testing.T) {
	o, _ := newTestOps()
	o.RequestHelp("r1")
	lease, _ := o.Claim("r1", "op1")
	if _, err := o.Release(lease.ID, "op2"); err != ErrNotHolder {
		t.Fatalf("non-holder release: err = %v", err)
	}
	released, err := o.Release(lease.ID, "op1")
	if err != nil || released.RobotID != "r1" {
		t.Fatalf("release: %v %+v", err, released)
	}
	if state, l := o.StateOf("r1"); state != protocol.StateAutonomous || l != nil {
		t.Fatalf("handback should be AUTONOMOUS with no lease, got %s %+v", state, l)
	}
}

func TestExpiryReturnsRobotToQueue(t *testing.T) {
	o, now := newTestOps()
	lease, _ := o.Claim("r1", "op1")
	if expired := o.SweepExpired(); len(expired) != 0 {
		t.Fatal("nothing should expire yet")
	}
	*now = now.Add(ttl + time.Second)
	expired := o.SweepExpired()
	if len(expired) != 1 || expired[0].Lease.ID != lease.ID || expired[0].Reason != protocol.RevokeExpired {
		t.Fatalf("expiry sweep: %+v", expired)
	}
	// Back in the queue, not silently AUTONOMOUS (§7.5).
	if state, l := o.StateOf("r1"); state != protocol.StateHelpRequested || l != nil {
		t.Fatalf("expired lease should leave HELP_REQUESTED, got %s %+v", state, l)
	}
}

func TestDropOperatorRevokesAllTheirLeases(t *testing.T) {
	o, _ := newTestOps()
	o.Claim("r1", "op1")
	o.Claim("r2", "op1")
	o.Claim("r3", "op2")
	revoked := o.DropOperator("op1")
	if len(revoked) != 2 {
		t.Fatalf("expected 2 revocations, got %d", len(revoked))
	}
	for _, rv := range revoked {
		if rv.Reason != protocol.RevokeOperatorLost {
			t.Fatalf("reason = %s", rv.Reason)
		}
	}
	if state, _ := o.StateOf("r1"); state != protocol.StateHelpRequested {
		t.Fatalf("r1 should be back in queue, got %s", state)
	}
	if state, l := o.StateOf("r3"); state != protocol.StateTeleop || l.OperatorID != "op2" {
		t.Fatal("op2's lease must survive op1's loss")
	}
}

func TestRobotForLease(t *testing.T) {
	o, _ := newTestOps()
	lease, _ := o.Claim("r1", "op1")
	if robot, ok := o.RobotForLease(lease.ID, "op1"); !ok || robot != "r1" {
		t.Fatalf("route = %s %v", robot, ok)
	}
	if _, ok := o.RobotForLease(lease.ID, "op2"); ok {
		t.Fatal("twist route must check the holder, not just the lease id")
	}
	if _, ok := o.RobotForLease("ls_nope", "op1"); ok {
		t.Fatal("unknown lease must not route")
	}
}
