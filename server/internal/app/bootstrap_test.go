package app_test

import (
	"path/filepath"
	"testing"

	"fleetplatform/server/internal/app"
	"fleetplatform/server/internal/store"
)

func TestBootstrapIsIdempotentAndAuthenticates(t *testing.T) {
	st, err := store.OpenSqlite(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const key = "0123456789abcdef0123456789abcdef"

	for i := 0; i < 3; i++ { // every startup re-runs it
		if err := app.Bootstrap(st, "club-fleet", key); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	fleet, ok, err := st.FleetByName("club-fleet")
	if err != nil || !ok {
		t.Fatalf("fleet missing: ok=%v err=%v", ok, err)
	}
	fleetID, ok, err := st.AuthEnroll(key)
	if err != nil || !ok || fleetID != fleet.ID {
		t.Fatalf("seeded key does not authenticate: ok=%v fleet=%q err=%v", ok, fleetID, err)
	}
	if _, ok, _ := st.AuthEnroll("0123456789abcdef0123456789abcdee"); ok {
		t.Fatal("wrong key authenticated")
	}

	// Rotation: a second key for the same fleet is added; the first stays valid.
	const key2 = "fedcba9876543210fedcba9876543210"
	if err := app.Bootstrap(st, "club-fleet", key2); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{key, key2} {
		if _, ok, _ := st.AuthEnroll(k); !ok {
			t.Fatalf("key %q should authenticate", k[:4])
		}
	}

	// Same key, different fleet name: refused rather than silently re-homed.
	if err := app.Bootstrap(st, "other-fleet", key); err == nil {
		t.Fatal("expected error re-homing a key to another fleet")
	}
}

// A revoked bootstrap key must not be resurrected by the next startup's
// Bootstrap run, and clients enrolled with it keep their tokens.
func TestBootstrapKeepsRevokedEnrollKeyRevoked(t *testing.T) {
	st, err := store.OpenSqlite(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const key = "bootstrap-revoke-test-key-0123456789"
	if err := app.Bootstrap(st, "club-fleet", key); err != nil {
		t.Fatal(err)
	}
	k, found, err := st.LookupEnrollKey(key)
	if err != nil || !found {
		t.Fatalf("lookup: %v %v", found, err)
	}
	token, _, err := st.CreateToken(k.FleetID, store.KindRobot, "bot")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RevokeEnrollKey(k.FleetID, k.ID); err != nil {
		t.Fatal(err)
	}
	if err := app.Bootstrap(st, "club-fleet", key); err != nil { // restart with the same env
		t.Fatalf("bootstrap after revoke: %v", err)
	}
	if _, ok, _ := st.AuthEnroll(key); ok {
		t.Fatal("bootstrap re-enabled a revoked enrollment key")
	}
	if _, ok, _ := st.AuthToken(token); !ok {
		t.Fatal("client enrolled before revocation lost its token")
	}
	if err := app.Bootstrap(st, "other-fleet", key); err == nil {
		t.Fatal("revoked key re-homed to another fleet")
	}
}
