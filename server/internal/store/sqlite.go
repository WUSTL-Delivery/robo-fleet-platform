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
	revoked    INTEGER NOT NULL DEFAULT 0,  -- v0.1.0; kept in step with revoked_at
	created_at INTEGER NOT NULL DEFAULT (unixepoch())
	-- expires_at, revoked_at: added by migrations below
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
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return &Sqlite{db: db, now: time.Now}, nil
}

// columnMigrations are additive: each adds one nullable column when it is
// missing, so a database created by any earlier release opens unchanged and
// its existing rows keep their meaning (NULL expires_at = never expires, NULL
// revoked_at = not revoked unless the v0.1.0 revoked flag says otherwise).
// Never edit or reorder an entry; append new ones.
var columnMigrations = []struct{ table, column, ddl string }{
	{"enroll_keys", "expires_at", `ALTER TABLE enroll_keys ADD COLUMN expires_at INTEGER`}, // unix millis; NULL = never
	{"enroll_keys", "revoked_at", `ALTER TABLE enroll_keys ADD COLUMN revoked_at INTEGER`}, // unix millis; NULL = not revoked
}

func migrate(db *sql.DB) error {
	for _, m := range columnMigrations {
		has, err := hasColumn(db, m.table, m.column)
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := db.Exec(m.ddl); err != nil {
			return fmt.Errorf("add %s.%s: %w", m.table, m.column, err)
		}
	}
	return nil
}

func hasColumn(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
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
	key, _, err := s.MintEnrollKey(fleetID, 0)
	return key, err
}

func (s *Sqlite) MintEnrollKey(fleetID string, ttl time.Duration) (string, EnrollKey, error) {
	if ttl < 0 {
		return "", EnrollKey{}, fmt.Errorf("store: enrollment key ttl must not be negative, got %s", ttl)
	}
	key := "fp-ek-" + randHex(16)
	now := s.now()
	k := EnrollKey{ID: "ek_" + randHex(8), FleetID: fleetID, CreatedAt: now.Truncate(time.Second)}
	var expires sql.NullInt64
	if ttl > 0 {
		k.ExpiresAt = now.Add(ttl).Truncate(time.Millisecond)
		expires = sql.NullInt64{Int64: k.ExpiresAt.UnixMilli(), Valid: true}
	}
	_, err := s.db.Exec(`INSERT INTO enroll_keys (id, fleet_id, key_hash, expires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		k.ID, fleetID, hashSecret(key), expires, k.CreatedAt.Unix())
	if err != nil {
		return "", EnrollKey{}, err
	}
	return key, k, nil
}

func (s *Sqlite) SeedEnrollKey(fleetID, key string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO enroll_keys (id, fleet_id, key_hash) VALUES (?, ?, ?)`,
		"ek_"+randHex(8), fleetID, hashSecret(key))
	return err
}

func (s *Sqlite) AuthEnroll(key string) (string, bool, error) {
	var fleetID string
	err := s.db.QueryRow(`SELECT fleet_id FROM enroll_keys
		WHERE key_hash = ? AND revoked = 0 AND revoked_at IS NULL
		  AND (expires_at IS NULL OR expires_at > ?)`,
		hashSecret(key), s.now().UnixMilli()).Scan(&fleetID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	return fleetID, err == nil, err
}

const enrollKeyCols = `id, fleet_id, created_at, expires_at, revoked, revoked_at`

func scanEnrollKey(row interface{ Scan(...any) error }) (EnrollKey, error) {
	var k EnrollKey
	var created int64
	var revoked int
	var expires, revokedAt sql.NullInt64
	if err := row.Scan(&k.ID, &k.FleetID, &created, &expires, &revoked, &revokedAt); err != nil {
		return EnrollKey{}, err
	}
	k.CreatedAt = time.Unix(created, 0)
	if expires.Valid {
		k.ExpiresAt = time.UnixMilli(expires.Int64)
	}
	switch {
	case revokedAt.Valid:
		k.RevokedAt = time.UnixMilli(revokedAt.Int64)
	case revoked != 0:
		// Revoked by hand under v0.1.0 (no timestamp recorded); the creation
		// time is the only honest lower bound.
		k.RevokedAt = k.CreatedAt
	}
	return k, nil
}

func (s *Sqlite) LookupEnrollKey(key string) (EnrollKey, bool, error) {
	k, err := scanEnrollKey(s.db.QueryRow(`SELECT `+enrollKeyCols+` FROM enroll_keys WHERE key_hash = ?`, hashSecret(key)))
	if errors.Is(err, sql.ErrNoRows) {
		return EnrollKey{}, false, nil
	}
	return k, err == nil, err
}

func (s *Sqlite) ListEnrollKeys(fleetID string) ([]EnrollKey, error) {
	rows, err := s.db.Query(`SELECT `+enrollKeyCols+` FROM enroll_keys WHERE fleet_id = ? ORDER BY created_at, rowid`, fleetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EnrollKey{}
	for rows.Next() {
		k, err := scanEnrollKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Sqlite) RevokeEnrollKey(fleetID, id string) (EnrollKey, error) {
	// Only enroll_keys changes: clients already enrolled through this key keep
	// their tokens (revoking a client is a separate, per-client operation).
	if _, err := s.db.Exec(`UPDATE enroll_keys SET revoked = 1, revoked_at = ?
		WHERE id = ? AND fleet_id = ? AND revoked_at IS NULL AND revoked = 0`,
		s.now().UnixMilli(), id, fleetID); err != nil {
		return EnrollKey{}, err
	}
	k, err := scanEnrollKey(s.db.QueryRow(`SELECT `+enrollKeyCols+` FROM enroll_keys WHERE id = ? AND fleet_id = ?`, id, fleetID))
	if errors.Is(err, sql.ErrNoRows) {
		return EnrollKey{}, ErrEnrollKeyNotFound
	}
	return k, err
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
