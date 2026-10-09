// Package ops is the atomic core (DESIGN.md D10): the per-robot intervention FSM
// and lease table under ONE lock domain. Exactly-one-driver is enforced here and
// nowhere else; no network hop ever happens inside a transition. This package is
// why the server is a monolith.
package ops

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"fleetplatform/sdk/go/protocol"
)

var (
	ErrNotFound  = errors.New("ops: no such lease")
	ErrNotHolder = errors.New("ops: caller does not hold this lease")
)

type Lease struct {
	ID         string
	RobotID    string
	OperatorID string
	ExpiresAt  time.Time
}

func (l Lease) Proto() protocol.Lease {
	return protocol.Lease{LeaseID: l.ID, RobotID: l.RobotID, OperatorID: l.OperatorID, ExpiresAtMs: l.ExpiresAt.UnixMilli()}
}

// Help is a robot's open intervention queue entry. It is opened when the robot
// enters HELP_REQUESTED and closed only by a handback, so a robot that is
// claimed and then lost returns to the queue with its original reason and time.
type Help struct {
	Reason      string
	Context     map[string]any
	RequestedAt time.Time
}

func (h Help) Proto() protocol.HelpDetails {
	return protocol.HelpDetails{Reason: h.Reason, Context: h.Context, RequestedAtMs: h.RequestedAt.UnixMilli()}
}

// Revoked pairs a dead lease with why it died. Help is the robot's queue entry
// when the revocation returned it to HELP_REQUESTED, nil otherwise (a steal).
type Revoked struct {
	Lease  Lease
	Reason string // protocol.Revoke*
	Help   *Help
}

type robotOps struct {
	state string
	lease *Lease
	help  *Help // open queue entry; survives TELEOP, cleared on handback
}

// requeue returns a robot whose lease died to HELP_REQUESTED and reports its
// queue entry. A robot that never asked (claimed proactively) gets an entry
// opened now, with the revocation reason standing in for the robot's.
func (r *robotOps) requeue(reason string, now time.Time) *Help {
	r.lease = nil
	r.state = protocol.StateHelpRequested
	if r.help == nil {
		r.help = &Help{Reason: reason, RequestedAt: now}
	}
	h := *r.help
	return &h
}

type Ops struct {
	mu     sync.Mutex
	now    func() time.Time
	ttl    time.Duration
	robots map[string]*robotOps
}

func New(now func() time.Time, leaseTTL time.Duration) *Ops {
	return &Ops{now: now, ttl: leaseTTL, robots: make(map[string]*robotOps)}
}

func (o *Ops) get(robotID string) *robotOps {
	r, ok := o.robots[robotID]
	if !ok {
		r = &robotOps{state: protocol.StateAutonomous}
		o.robots[robotID] = r
	}
	return r
}

// StateOf returns the robot's FSM state, its current lease (nil if none) and,
// only while HELP_REQUESTED, its queue entry.
func (o *Ops) StateOf(robotID string) (string, *Lease, *Help) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r := o.get(robotID)
	var lease *Lease
	if r.lease != nil {
		l := *r.lease
		lease = &l
	}
	var help *Help
	if r.state == protocol.StateHelpRequested && r.help != nil {
		h := *r.help
		help = &h
	}
	return r.state, lease, help
}

// RequestHelp moves AUTONOMOUS → HELP_REQUESTED and opens the queue entry,
// stamped with the server clock. Idempotent: a repeat request changes nothing
// (the first reason and time stand) and returns nil. A robot already in TELEOP
// cannot re-queue itself (§7.5: operator loss is the server's transition).
func (o *Ops) RequestHelp(robotID, reason string, context map[string]any) *Help {
	o.mu.Lock()
	defer o.mu.Unlock()
	r := o.get(robotID)
	if r.state != protocol.StateAutonomous {
		return nil
	}
	r.state = protocol.StateHelpRequested
	r.help = &Help{Reason: reason, Context: context, RequestedAt: o.now()}
	h := *r.help
	return &h
}

// Claim grants an exclusive lease, from HELP_REQUESTED or proactively from
// AUTONOMOUS. Claiming a robot already in TELEOP is a steal: revoke + reissue,
// never share — the revoked lease is returned so the old operator can be told.
func (o *Ops) Claim(robotID, operatorID string) (Lease, *Revoked) {
	o.mu.Lock()
	defer o.mu.Unlock()
	r := o.get(robotID)
	var stolen *Revoked
	if r.lease != nil {
		stolen = &Revoked{Lease: *r.lease, Reason: protocol.RevokeStolen}
	}
	lease := Lease{
		ID:         "ls_" + randHex(8),
		RobotID:    robotID,
		OperatorID: operatorID,
		ExpiresAt:  o.now().Add(o.ttl),
	}
	r.lease = &lease
	r.state = protocol.StateTeleop
	return lease, stolen
}

// Renew extends the holder's lease TTL.
func (o *Ops) Renew(leaseID, operatorID string) (Lease, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, r := range o.robots {
		if r.lease != nil && r.lease.ID == leaseID {
			if r.lease.OperatorID != operatorID {
				return Lease{}, ErrNotHolder
			}
			r.lease.ExpiresAt = o.now().Add(o.ttl)
			return *r.lease, nil
		}
	}
	return Lease{}, ErrNotFound
}

// Release is the operator handing back: TELEOP → AUTONOMOUS. (Handback always
// replans — but replanning is the domain's job, not ours.)
func (o *Ops) Release(leaseID, operatorID string) (Lease, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, r := range o.robots {
		if r.lease != nil && r.lease.ID == leaseID {
			if r.lease.OperatorID != operatorID {
				return Lease{}, ErrNotHolder
			}
			l := *r.lease
			r.lease = nil
			r.help = nil
			r.state = protocol.StateAutonomous
			return l, nil
		}
	}
	return Lease{}, ErrNotFound
}

// DropOperator revokes every lease an operator holds (their presence lapsed or
// socket dropped). Robots go back to HELP_REQUESTED — front of the queue for the
// next operator, per §7.5, because they keep their original request time; they
// never re-queue themselves.
func (o *Ops) DropOperator(operatorID string) []Revoked {
	o.mu.Lock()
	defer o.mu.Unlock()
	var out []Revoked
	for _, r := range o.robots {
		if r.lease != nil && r.lease.OperatorID == operatorID {
			lease := *r.lease
			out = append(out, Revoked{Lease: lease, Reason: protocol.RevokeOperatorLost, Help: r.requeue(protocol.RevokeOperatorLost, o.now())})
		}
	}
	return out
}

// SweepExpired revokes leases past their TTL; robots return to HELP_REQUESTED.
func (o *Ops) SweepExpired() []Revoked {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.now()
	var out []Revoked
	for _, r := range o.robots {
		if r.lease != nil && now.After(r.lease.ExpiresAt) {
			lease := *r.lease
			out = append(out, Revoked{Lease: lease, Reason: protocol.RevokeExpired, Help: r.requeue(protocol.RevokeExpired, now)})
		}
	}
	return out
}

// RobotForLease resolves a live lease held by operatorID to its robot. Used to
// route control-plane twist; the robot re-checks the lease id on its side
// (defense in depth — the server just refuses to relay garbage).
func (o *Ops) RobotForLease(leaseID, operatorID string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for robotID, r := range o.robots {
		if r.lease != nil && r.lease.ID == leaseID && r.lease.OperatorID == operatorID {
			return robotID, true
		}
	}
	return "", false
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
