package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS fleets (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE TABLE IF NOT EXISTS clients (
	id         TEXT PRIMARY KEY,
	fleet_id   TEXT NOT NULL REFERENCES fleets(id),
	kind       TEXT NOT NULL CHECK (kind IN ('robot','service','operator')),
	name       TEXT NOT NULL DEFAULT '',
	token_hash TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE TABLE IF NOT EXISTS enroll_keys (
	id         TEXT PRIMARY KEY,
	fleet_id   TEXT NOT NULL REFERENCES fleets(id),
	key_hash   TEXT NOT NULL UNIQUE,
	revoked    INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE TABLE IF NOT EXISTS operator_invites (
	id         TEXT PRIMARY KEY,
	fleet_id   TEXT NOT NULL REFERENCES fleets(id),
	key_hash   TEXT NOT NULL UNIQUE,
	expires_at INTEGER NOT NULL,          -- unix millis
	used_at    INTEGER,                   -- unix millis; NULL until redeemed
	client_id  TEXT REFERENCES clients(id),
	created_at INTEGER NOT NULL DEFAULT (unixepoch())
);
`

type Sqlite struct {
	db  *sql.DB
	now func() time.Time // injectable clock for invite expiry
}

func OpenSqlite(path string) (*Sqlite, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// The server is a single process; one connection sidesteps sqlite write locking.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: init schema: %w", err)
	}
	return &Sqlite{db: db, now: time.Now}, nil
}

func (s *Sqlite) Close() error { return s.db.Close() }

func (s *Sqlite) CreateFleet(name string) (Fleet, error) {
	f := Fleet{ID: "f_" + randHex(8), Name: name}
	_, err := s.db.Exec(`INSERT INTO fleets (id, name) VALUES (?, ?)`, f.ID, f.Name)
	return f, err
}

func (s *Sqlite) FleetByName(name string) (Fleet, bool, error) {
	var f Fleet
	err := s.db.QueryRow(`SELECT id, name FROM fleets WHERE name = ?`, name).Scan(&f.ID, &f.Name)
	if err == sql.ErrNoRows {
		return Fleet{}, false, nil
	}
	return f, err == nil, err
}

func (s *Sqlite) CreateEnrollKey(fleetID string) (string, error) {
	key := "fp-ek-" + randHex(16)
	_, err := s.db.Exec(`INSERT INTO enroll_keys (id, fleet_id, key_hash) VALUES (?, ?, ?)`,
		"ek_"+randHex(8), fleetID, hashSecret(key))
	if err != nil {
		return "", err
	}
	return key, nil
}

func (s *Sqlite) SeedEnrollKey(fleetID, key string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO enroll_keys (id, fleet_id, key_hash) VALUES (?, ?, ?)`,
		"ek_"+randHex(8), fleetID, hashSecret(key))
	return err
}

func (s *Sqlite) AuthEnroll(key string) (string, bool, error) {
	var fleetID string
	err := s.db.QueryRow(`SELECT fleet_id FROM enroll_keys WHERE key_hash = ? AND revoked = 0`,
		hashSecret(key)).Scan(&fleetID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return fleetID, err == nil, err
}

func (s *Sqlite) CreateToken(fleetID string, kind Kind, name string) (string, Client, error) {
	token := "fp-tk-" + randHex(24)
	c := Client{ID: idPrefix(kind) + randHex(8), FleetID: fleetID, Kind: kind, Name: name}
	_, err := s.db.Exec(`INSERT INTO clients (id, fleet_id, kind, name, token_hash) VALUES (?, ?, ?, ?, ?)`,
		c.ID, c.FleetID, string(c.Kind), c.Name, hashSecret(token))
	if err != nil {
		return "", Client{}, err
	}
	return token, c, nil
}

func (s *Sqlite) AuthToken(token string) (Client, bool, error) {
	var c Client
	var kind string
	err := s.db.QueryRow(`SELECT id, fleet_id, kind, name FROM clients WHERE token_hash = ?`,
		hashSecret(token)).Scan(&c.ID, &c.FleetID, &kind, &c.Name)
	if err == sql.ErrNoRows {
		return Client{}, false, nil
	}
	c.Kind = Kind(kind)
	return c, err == nil, err
}

func (s *Sqlite) CreateOperatorInvite(fleetID string, ttl time.Duration) (string, time.Time, error) {
	if ttl <= 0 {
		return "", time.Time{}, fmt.Errorf("store: operator invite ttl must be positive, got %s", ttl)
	}
	key := "fp-oi-" + randHex(16)
	expiresAt := s.now().Add(ttl).Truncate(time.Millisecond)
	_, err := s.db.Exec(`INSERT INTO operator_invites (id, fleet_id, key_hash, expires_at) VALUES (?, ?, ?, ?)`,
		"oi_"+randHex(8), fleetID, hashSecret(key), expiresAt.UnixMilli())
	if err != nil {
		return "", time.Time{}, err
	}
	return key, expiresAt, nil
}

func (s *Sqlite) RedeemOperatorInvite(key, name string) (string, Client, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", Client{}, err
	}
	defer tx.Rollback()

	var id, fleetID string
	var expiresAt int64
	var usedAt sql.NullInt64
	err = tx.QueryRow(`SELECT id, fleet_id, expires_at, used_at FROM operator_invites WHERE key_hash = ?`,
		hashSecret(key)).Scan(&id, &fleetID, &expiresAt, &usedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", Client{}, ErrInviteInvalid
	}
	if err != nil {
		return "", Client{}, err
	}
	now := s.now().UnixMilli()
	if usedAt.Valid {
		return "", Client{}, ErrInviteUsed
	}
	if now >= expiresAt {
		return "", Client{}, ErrInviteExpired
	}

	token := "fp-tk-" + randHex(24)
	c := Client{ID: idPrefix(KindOperator) + randHex(8), FleetID: fleetID, Kind: KindOperator, Name: name}
	if _, err := tx.Exec(`INSERT INTO clients (id, fleet_id, kind, name, token_hash) VALUES (?, ?, ?, ?, ?)`,
		c.ID, c.FleetID, string(c.Kind), c.Name, hashSecret(token)); err != nil {
		return "", Client{}, err
	}
	// Guarded on used_at IS NULL so a racing redeem can never consume it twice.
	res, err := tx.Exec(`UPDATE operator_invites SET used_at = ?, client_id = ? WHERE id = ? AND used_at IS NULL`,
		now, c.ID, id)
	if err != nil {
		return "", Client{}, err
	}
	if n, err := res.RowsAffected(); err != nil {
		return "", Client{}, err
	} else if n != 1 {
		return "", Client{}, ErrInviteUsed
	}
	if err := tx.Commit(); err != nil {
		return "", Client{}, err
	}
	return token, c, nil
}

func (s *Sqlite) RobotsInFleet(fleetID string) ([]Client, error) {
	rows, err := s.db.Query(`SELECT id, fleet_id, kind, name FROM clients WHERE fleet_id = ? AND kind = 'robot' ORDER BY created_at`, fleetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Client
	for rows.Next() {
		var c Client
		var kind string
		if err := rows.Scan(&c.ID, &c.FleetID, &kind, &c.Name); err != nil {
			return nil, err
		}
		c.Kind = Kind(kind)
		out = append(out, c)
	}
	return out, rows.Err()
}

func idPrefix(kind Kind) string {
	switch kind {
	case KindRobot:
		return "r_"
	case KindService:
		return "s_"
	default:
		return "o_"
	}
}

func hashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
