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
	// CreatedAt and RevokedAt are filled by ListClients and RevokeClient only.
	// A non-zero RevokedAt means the client's token no longer authenticates.
	CreatedAt time.Time
	RevokedAt time.Time
}

// Revoked reports whether the client's token has been revoked.
func (c Client) Revoked() bool { return !c.RevokedAt.IsZero() }

// ErrClientNotFound: no client with that id in that fleet.
var ErrClientNotFound = errors.New("store: client not found")

// EnrollKey is an enrollment key's metadata; the plaintext is never stored.
// A zero ExpiresAt means the key never expires (every key minted before
// expiry existed, and the FLEET_BOOTSTRAP_ENROLL_KEY key). A non-zero
// RevokedAt means the key is revoked.
type EnrollKey struct {
	ID        string
	FleetID   string
	CreatedAt time.Time
	ExpiresAt time.Time
	RevokedAt time.Time
}

// Revoked reports whether the key has been revoked.
func (k EnrollKey) Revoked() bool { return !k.RevokedAt.IsZero() }

// Expired reports whether the key has an expiry that is at or before now.
func (k EnrollKey) Expired(now time.Time) bool {
	return !k.ExpiresAt.IsZero() && !now.Before(k.ExpiresAt)
}

// ErrEnrollKeyNotFound: no enrollment key with that id in that fleet.
var ErrEnrollKeyNotFound = errors.New("store: enrollment key not found")

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

	// CreateEnrollKey mints a fleet enrollment key that never expires; the
	// plaintext is returned exactly once and only its hash is stored.
	CreateEnrollKey(fleetID string) (string, error)
	// MintEnrollKey mints a fleet enrollment key valid for ttl, or forever
	// when ttl is 0 (negative is an error). The plaintext is returned exactly
	// once and only its hash is stored.
	MintEnrollKey(fleetID string, ttl time.Duration) (key string, k EnrollKey, err error)
	// AuthEnroll resolves an enrollment key to its fleet. Revoked and expired
	// keys do not authenticate. Tokens already minted through a key are not
	// affected by that key's revocation or expiry.
	AuthEnroll(key string) (fleetID string, ok bool, err error)
	// LookupEnrollKey returns a key's metadata whatever its state (revoked,
	// expired, or valid), for callers that must tell those apart.
	LookupEnrollKey(key string) (EnrollKey, bool, error)
	// SeedEnrollKey registers a caller-chosen enrollment key (stored hashed)
	// that never expires. Idempotent: seeding a key that already exists is a
	// no-op, and a revoked key stays revoked.
	SeedEnrollKey(fleetID, key string) error
	// ListEnrollKeys returns every enrollment key of a fleet, revoked and
	// expired ones included, oldest first.
	ListEnrollKeys(fleetID string) ([]EnrollKey, error)
	// RevokeEnrollKey revokes the key with that id in that fleet so it can no
	// longer enroll new clients. Idempotent: revoking a revoked key returns it
	// unchanged. An id not in the fleet returns ErrEnrollKeyNotFound.
	RevokeEnrollKey(fleetID, id string) (EnrollKey, error)

	// CreateToken mints an opaque per-client token (returned once, stored hashed).
	CreateToken(fleetID string, kind Kind, name string) (string, Client, error)
	// AuthToken resolves a presented token to its client; identity is always
	// derived server-side from the credential, never claimed. A revoked
	// client's token does not authenticate.
	AuthToken(token string) (Client, bool, error)
	// ListClients returns every client of a fleet (robots, services,
	// operators), revoked ones included, oldest first. Tokens are never listed.
	ListClients(fleetID string) ([]Client, error)
	// RevokeClient revokes the token of the client with that id in that fleet.
	// Idempotent: revoking a revoked client returns it unchanged. An id not in
	// the fleet returns ErrClientNotFound. Closing a live connection is the
	// caller's job (the store does not know about connections).
	RevokeClient(fleetID, id string) (Client, error)

	// CreateOperatorInvite mints a single-use operator invite key for a fleet,
	// valid for ttl (must be > 0). The plaintext is returned exactly once and
	// only its hash is stored.
	CreateOperatorInvite(fleetID string, ttl time.Duration) (key string, expiresAt time.Time, err error)
	// RedeemOperatorInvite consumes an invite key and mints an operator client
	// plus its token, atomically. A key redeems at most once; an unknown, used,
	// or expired key returns ErrInviteInvalid, ErrInviteUsed, or ErrInviteExpired.
	RedeemOperatorInvite(key, name string) (token string, c Client, err error)

	// RobotsInFleet returns the fleet's robots that are not revoked.
	RobotsInFleet(fleetID string) ([]Client, error)

	Close() error
}
