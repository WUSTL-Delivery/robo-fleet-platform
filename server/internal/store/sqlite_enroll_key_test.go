package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openEnrollKeyLifecycleStore(t *testing.T) (*Sqlite, Fleet) {
	t.Helper()
	st, err := OpenSqlite(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f, err := st.CreateFleet("club")
	if err != nil {
		t.Fatal(err)
	}
	return st, f
}

func TestEnrollKeyLifecycleValidKeyAuthenticates(t *testing.T) {
	st, f := openEnrollKeyLifecycleStore(t)

	forever, err := st.CreateEnrollKey(f.ID)
	if err != nil {
		t.Fatal(err)
	}
	timed, k, err := st.MintEnrollKey(f.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if k.ExpiresAt.IsZero() || k.Revoked() || k.FleetID != f.ID {
		t.Fatalf("minted key metadata = %+v", k)
	}
	for _, key := range []string{forever, timed} {
		if fleetID, ok, err := st.AuthEnroll(key); err != nil || !ok || fleetID != f.ID {
			t.Fatalf("AuthEnroll(valid) = %q, %v, %v", fleetID, ok, err)
		}
	}

	keys, err := st.ListEnrollKeys(f.ID)
	if err != nil || len(keys) != 2 {
		t.Fatalf("ListEnrollKeys = %+v, %v; want 2 keys", keys, err)
	}
	if !keys[0].ExpiresAt.IsZero() {
		t.Errorf("CreateEnrollKey key has expiry %v, want never", keys[0].ExpiresAt)
	}
	if !keys[1].ExpiresAt.Equal(k.ExpiresAt) {
		t.Errorf("listed expiry %v != minted %v", keys[1].ExpiresAt, k.ExpiresAt)
	}
	if _, _, err := st.MintEnrollKey(f.ID, -time.Second); err == nil {
		t.Fatal("negative ttl accepted")
	}
}

func TestEnrollKeyLifecycleExpiredKeyRefused(t *testing.T) {
	st, f := openEnrollKeyLifecycleStore(t)
	now := time.Now()
	st.now = func() time.Time { return now }

	key, k, err := st.MintEnrollKey(f.ID, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// A client enrolled while the key was valid.
	token, _, err := st.CreateToken(f.ID, KindRobot, "early")
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(10 * time.Minute) // exactly at expiry: refused
	if _, ok, err := st.AuthEnroll(key); err != nil || ok {
		t.Fatalf("AuthEnroll(expired) ok=%v err=%v, want refused", ok, err)
	}
	got, found, err := st.LookupEnrollKey(key)
	if err != nil || !found || got.ID != k.ID || !got.Expired(now) || got.Revoked() {
		t.Fatalf("LookupEnrollKey(expired) = %+v, %v, %v", got, found, err)
	}
	if _, ok, err := st.AuthToken(token); err != nil || !ok {
		t.Fatalf("token minted before expiry stopped working: ok=%v err=%v", ok, err)
	}
}

func TestEnrollKeyLifecycleRevokedKeyRefused(t *testing.T) {
	st, f := openEnrollKeyLifecycleStore(t)
	other, err := st.CreateFleet("home")
	if err != nil {
		t.Fatal(err)
	}
	key, k, err := st.MintEnrollKey(f.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := st.CreateToken(f.ID, KindService, "brain")
	if err != nil {
		t.Fatal(err)
	}

	// Another fleet's id space: not found, and nothing revoked.
	if _, err := st.RevokeEnrollKey(other.ID, k.ID); !errors.Is(err, ErrEnrollKeyNotFound) {
		t.Fatalf("cross-fleet revoke err = %v, want ErrEnrollKeyNotFound", err)
	}
	if _, ok, _ := st.AuthEnroll(key); !ok {
		t.Fatal("cross-fleet revoke revoked the key")
	}

	rk, err := st.RevokeEnrollKey(f.ID, k.ID)
	if err != nil || !rk.Revoked() {
		t.Fatalf("RevokeEnrollKey = %+v, %v", rk, err)
	}
	if _, ok, err := st.AuthEnroll(key); err != nil || ok {
		t.Fatalf("AuthEnroll(revoked) ok=%v err=%v, want refused", ok, err)
	}
	again, err := st.RevokeEnrollKey(f.ID, k.ID)
	if err != nil || !again.RevokedAt.Equal(rk.RevokedAt) {
		t.Fatalf("second revoke = %+v, %v; want unchanged revoked_at %v", again, err, rk.RevokedAt)
	}
	if _, err := st.RevokeEnrollKey(f.ID, "ek_nope"); !errors.Is(err, ErrEnrollKeyNotFound) {
		t.Fatalf("unknown id err = %v", err)
	}

	// Re-seeding the same plaintext (the bootstrap path) does not un-revoke it.
	if err := st.SeedEnrollKey(f.ID, key); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.AuthEnroll(key); ok {
		t.Fatal("SeedEnrollKey un-revoked a revoked key")
	}

	// Already-enrolled clients keep working.
	if c, ok, err := st.AuthToken(token); err != nil || !ok || c.Name != "brain" {
		t.Fatalf("token minted before revocation: %+v ok=%v err=%v", c, ok, err)
	}
}

// v010Schema is the store schema exactly as released in v0.1.0. Do not edit:
// it stands in for the club deployment's live database.
const v010Schema = `
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
`

func TestEnrollKeyLifecycleMigratesV010Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet-v010.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	const liveKey, deadKey, liveToken = "fp-ek-live-v010", "fp-ek-dead-v010", "fp-tk-live-v010"
	for _, stmt := range []string{
		v010Schema,
		`INSERT INTO fleets (id, name) VALUES ('f_v010', 'club')`,
		`INSERT INTO enroll_keys (id, fleet_id, key_hash) VALUES ('ek_live', 'f_v010', '` + hashSecret(liveKey) + `')`,
		`INSERT INTO enroll_keys (id, fleet_id, key_hash, revoked) VALUES ('ek_dead', 'f_v010', '` + hashSecret(deadKey) + `', 1)`,
		`INSERT INTO clients (id, fleet_id, kind, name, token_hash) VALUES ('r_v010', 'f_v010', 'robot', 'bot', '` + hashSecret(liveToken) + `')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	raw.Close()

	for round := 0; round < 2; round++ { // migration is idempotent across restarts
		st, err := OpenSqlite(path)
		if err != nil {
			t.Fatalf("round %d: open v0.1.0 db: %v", round, err)
		}
		if fleetID, ok, err := st.AuthEnroll(liveKey); round == 0 && (err != nil || !ok || fleetID != "f_v010") {
			t.Fatalf("v0.1.0 key no longer enrolls after migration: %q %v %v", fleetID, ok, err)
		}
		if _, ok, _ := st.AuthEnroll(deadKey); ok {
			t.Fatalf("round %d: key revoked under v0.1.0 enrolls again", round)
		}
		if _, ok, err := st.AuthToken(liveToken); err != nil || !ok {
			t.Fatalf("round %d: v0.1.0 token rejected: %v %v", round, ok, err)
		}
		keys, err := st.ListEnrollKeys("f_v010")
		if err != nil || len(keys) != 2 {
			t.Fatalf("round %d: ListEnrollKeys = %+v, %v", round, keys, err)
		}
		for _, k := range keys {
			if !k.ExpiresAt.IsZero() {
				t.Errorf("round %d: %s gained an expiry %v; v0.1.0 keys never expire", round, k.ID, k.ExpiresAt)
			}
			if want := k.ID == "ek_dead" || round > 0; k.Revoked() != want {
				t.Errorf("round %d: %s revoked=%v, want %v", round, k.ID, k.Revoked(), want)
			}
		}
		if round == 0 {
			// New features work on the migrated database.
			if _, err := st.RevokeEnrollKey("f_v010", "ek_live"); err != nil {
				t.Fatal(err)
			}
			if _, ok, _ := st.AuthEnroll(liveKey); ok {
				t.Fatal("revoked migrated key still enrolls")
			}
			if _, ok, _ := st.AuthToken(liveToken); !ok {
				t.Fatal("revoking the key revoked its client's token")
			}
			if _, _, err := st.CreateOperatorInvite("f_v010", time.Hour); err != nil {
				t.Fatalf("operator invites on migrated db: %v", err)
			}
		} else if _, ok, _ := st.AuthEnroll(liveKey); ok {
			t.Fatal("revocation did not survive a restart")
		}
		st.Close()
	}
}
