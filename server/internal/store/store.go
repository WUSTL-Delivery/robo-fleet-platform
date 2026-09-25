// Package store is the persistence seam (DESIGN.md D10): sqlite today, postgres
// at tier 3. Runtime state only — fleets, clients, credentials. The wire
// describes the robots; nothing domain-shaped lands here.
package store

import (
	"errors"
	"time"
)

type Kind string

const (
	KindRobot    Kind = "robot"
	KindService  Kind = "service"
	KindOperator Kind = "operator"
)

type Fleet struct {
	ID   string
	Name string
}

type Client struct {
	ID      string
	FleetID string
	Kind    Kind
	Name    string
}

// Operator invite redemption failures. Callers map all three to one generic
// auth error on the wire; the distinction is for logs and admin tooling.
var (
	ErrInviteInvalid = errors.New("store: operator invite not found")
	ErrInviteUsed    = errors.New("store: operator invite already redeemed")
	ErrInviteExpired = errors.New("store: operator invite expired")
)

type Store interface {
	CreateFleet(name string) (Fleet, error)
	FleetByName(name string) (Fleet, bool, error)

	// CreateEnrollKey mints a fleet enrollment key; the plaintext is returned
	// exactly once and only its hash is stored.
	CreateEnrollKey(fleetID string) (string, error)
	// AuthEnroll resolves an enrollment key to its fleet.
	AuthEnroll(key string) (fleetID string, ok bool, err error)
	// SeedEnrollKey registers a caller-chosen enrollment key (stored hashed).
	// Idempotent: seeding a key that already exists is a no-op, and a revoked
	// key stays revoked.
	SeedEnrollKey(fleetID, key string) error

	// CreateToken mints an opaque per-client token (returned once, stored hashed).
	CreateToken(fleetID string, kind Kind, name string) (string, Client, error)
	// AuthToken resolves a presented token to its client; identity is always
	// derived server-side from the credential, never claimed.
	AuthToken(token string) (Client, bool, error)

	// CreateOperatorInvite mints a single-use operator invite key for a fleet,
	// valid for ttl (must be > 0). The plaintext is returned exactly once and
	// only its hash is stored.
	CreateOperatorInvite(fleetID string, ttl time.Duration) (key string, expiresAt time.Time, err error)
	// RedeemOperatorInvite consumes an invite key and mints an operator client
	// plus its token, atomically. A key redeems at most once; an unknown, used,
	// or expired key returns ErrInviteInvalid, ErrInviteUsed, or ErrInviteExpired.
	RedeemOperatorInvite(key, name string) (token string, c Client, err error)

	RobotsInFleet(fleetID string) ([]Client, error)

	Close() error
}
