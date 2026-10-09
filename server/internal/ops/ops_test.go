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
	if o.RequestHelp("r1", "stuck", nil) == nil {
		t.Fatal("AUTONOMOUS → HELP_REQUESTED should transition")
	}
	if o.RequestHelp("r1", "stuck again", nil) != nil {
		t.Fatal("HELP_REQUESTED → HELP_REQUESTED should be a no-op")
	}
	if state, _, _ := o.StateOf("r1"); state != protocol.StateHelpRequested {
		t.Fatalf("state = %s", state)
	}
}

func TestClaimGrantsExclusiveLease(t *testing.T) {
	o, now := newTestOps()
	o.RequestHelp("r1", "stuck", nil)
	lease, stolen := o.Claim("r1", "op1")
	if stolen != nil {
		t.Fatal("first claim should not steal")
	}
	if state, l, _ := o.StateOf("r1"); state != protocol.StateTeleop || l == nil || l.ID != lease.ID {
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
	if state, l, _ := o.StateOf("r1"); state != protocol.StateTeleop || l.OperatorID != "op2" {
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
	o.RequestHelp("r1", "stuck", nil)
	lease, _ := o.Claim("r1", "op1")
	if _, err := o.Release(lease.ID, "op2"); err != ErrNotHolder {
		t.Fatalf("non-holder release: err = %v", err)
	}
	released, err := o.Release(lease.ID, "op1")
	if err != nil || released.RobotID != "r1" {
		t.Fatalf("release: %v %+v", err, released)
	}
	if state, l, _ := o.StateOf("r1"); state != protocol.StateAutonomous || l != nil {
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
	if state, l, _ := o.StateOf("r1"); state != protocol.StateHelpRequested || l != nil {
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
	if state, _, _ := o.StateOf("r1"); state != protocol.StateHelpRequested {
		t.Fatalf("r1 should be back in queue, got %s", state)
	}
	if state, l, _ := o.StateOf("r3"); state != protocol.StateTeleop || l.OperatorID != "op2" {
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

// The queue entry is opened by the first help request, hidden (not lost) while
// an operator drives, and returned intact when the lease expires.
func TestHelpDetailsSurviveClaimAndExpiry(t *testing.T) {
	o, now := newTestOps()
	requestedAt := *now
	ctx := map[string]any{"attempts": float64(3)}
	if h := o.RequestHelp("r1", "nav_goal_failed", ctx); h == nil || !h.RequestedAt.Equal(requestedAt) {
		t.Fatalf("RequestHelp = %+v, want an entry stamped %v", h, requestedAt)
	}

	// A repeat request keeps the first reason and time.
	*now = now.Add(2 * time.Second)
	o.RequestHelp("r1", "something else", nil)
	_, _, h := o.StateOf("r1")
	if h == nil || h.Reason != "nav_goal_failed" || !h.RequestedAt.Equal(requestedAt) || h.Context["attempts"] != float64(3) {
		t.Fatalf("queued entry = %+v", h)
	}

	o.Claim("r1", "op1")
	if state, _, h := o.StateOf("r1"); state != protocol.StateTeleop || h != nil {
		t.Fatalf("TELEOP should report no help entry, got %s %+v", state, h)
	}

	*now = now.Add(ttl + time.Second)
	revoked := o.SweepExpired()
	if len(revoked) != 1 || revoked[0].Help == nil {
		t.Fatalf("expiry should report the queue entry: %+v", revoked)
	}
	for _, got := range []*Help{revoked[0].Help, third(o.StateOf("r1"))} {
		if got == nil || got.Reason != "nav_goal_failed" || !got.RequestedAt.Equal(requestedAt) || got.Context["attempts"] != float64(3) {
			t.Fatalf("after expiry entry = %+v, want the original", got)
		}
	}

	// Operator loss keeps it too; a steal reports none (the robot stays in TELEOP).
	o.Claim("r1", "op1")
	if _, stolen := o.Claim("r1", "op2"); stolen == nil || stolen.Help != nil {
		t.Fatalf("steal should carry no help entry: %+v", stolen)
	}
	dropped := o.DropOperator("op2")
	if len(dropped) != 1 || dropped[0].Help == nil || !dropped[0].Help.RequestedAt.Equal(requestedAt) {
		t.Fatalf("operator loss should report the original entry: %+v", dropped)
	}
}

// Handback closes the entry: the next request is a new one with a new time.
func TestHandbackClosesHelpEntry(t *testing.T) {
	o, now := newTestOps()
	o.RequestHelp("r1", "first", nil)
	lease, _ := o.Claim("r1", "op1")
	if _, err := o.Release(lease.ID, "op1"); err != nil {
		t.Fatal(err)
	}
	if _, _, h := o.StateOf("r1"); h != nil {
		t.Fatalf("AUTONOMOUS should have no help entry, got %+v", h)
	}
	*now = now.Add(time.Minute)
	h := o.RequestHelp("r1", "second", nil)
	if h == nil || h.Reason != "second" || !h.RequestedAt.Equal(*now) {
		t.Fatalf("new entry = %+v", h)
	}
}

// A robot that never asked (claimed proactively, then lost) still lands in the
// queue with an entry: the revocation reason and the time it was requeued.
func TestRequeueWithoutRequestOpensEntry(t *testing.T) {
	o, now := newTestOps()
	o.Claim("r1", "op1")
	*now = now.Add(ttl + time.Second)
	revoked := o.SweepExpired()
	if len(revoked) != 1 || revoked[0].Help == nil {
		t.Fatalf("revoked = %+v", revoked)
	}
	if h := revoked[0].Help; h.Reason != protocol.RevokeExpired || !h.RequestedAt.Equal(*now) || h.Context != nil {
		t.Fatalf("server-opened entry = %+v", h)
	}
	if h := third(o.StateOf("r1")); h == nil || h.Reason != protocol.RevokeExpired {
		t.Fatalf("StateOf entry = %+v", h)
	}
}

func third(_ string, _ *Lease, h *Help) *Help { return h }
