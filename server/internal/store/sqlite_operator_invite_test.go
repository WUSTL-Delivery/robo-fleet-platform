package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openOperatorInviteTestStore(t *testing.T) *Sqlite {
	t.Helper()
	st, err := OpenSqlite(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestOperatorInviteRedeemsOnceAsOperator(t *testing.T) {
	st := openOperatorInviteTestStore(t)
	f, err := st.CreateFleet("club")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	key, expiresAt, err := st.CreateOperatorInvite(f.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if key == "" {
		t.Fatal("empty invite key")
	}
	if expiresAt.Before(before.Add(time.Hour-time.Second)) || expiresAt.After(time.Now().Add(time.Hour)) {
		t.Fatalf("expiresAt %v not ~1h from now", expiresAt)
	}

	// Only the hash is stored.
	var n int
	if err := st.db.QueryRow(`SELECT count(*) FROM operator_invites WHERE key_hash = ?`, key).Scan(&n); err != nil || n != 0 {
		t.Fatalf("plaintext key found in store (n=%d, err=%v)", n, err)
	}

	token, c, err := st.RedeemOperatorInvite(key, "alice")
	if err != nil {
		t.Fatalf("first redeem: %v", err)
	}
	if c.Kind != KindOperator || c.FleetID != f.ID || c.Name != "alice" {
		t.Fatalf("redeemed client = %+v, want operator in fleet %s named alice", c, f.ID)
	}
	got, ok, err := st.AuthToken(token)
	if err != nil || !ok || got.ID != c.ID || got.Kind != KindOperator {
		t.Fatalf("AuthToken(minted) = %+v, %v, %v", got, ok, err)
	}

	if _, _, err := st.RedeemOperatorInvite(key, "mallory"); !errors.Is(err, ErrInviteUsed) {
		t.Fatalf("second redeem err = %v, want ErrInviteUsed", err)
	}
	if _, _, err := st.RedeemOperatorInvite("fp-oi-not-a-real-key", "x"); !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("unknown key err = %v, want ErrInviteInvalid", err)
	}
}

func TestOperatorInviteRefusesExpiredKey(t *testing.T) {
	st := openOperatorInviteTestStore(t)
	f, err := st.CreateFleet("club")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	st.now = func() time.Time { return now }

	key, _, err := st.CreateOperatorInvite(f.ID, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute) // exactly at expiry: refused
	if _, _, err := st.RedeemOperatorInvite(key, "late"); !errors.Is(err, ErrInviteExpired) {
		t.Fatalf("expired redeem err = %v, want ErrInviteExpired", err)
	}
	var clients int
	if err := st.db.QueryRow(`SELECT count(*) FROM clients WHERE kind = 'operator'`).Scan(&clients); err != nil || clients != 0 {
		t.Fatalf("expired redeem minted a client (n=%d, err=%v)", clients, err)
	}

	if _, _, err := st.CreateOperatorInvite(f.ID, 0); err == nil {
		t.Fatal("ttl 0 accepted, want error")
	}
}
