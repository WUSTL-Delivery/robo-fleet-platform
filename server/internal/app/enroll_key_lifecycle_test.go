package app_test

import (
	"path/filepath"
	"testing"
	"time"

	"fleetplatform/sdk/go/protocol"
	"fleetplatform/server/internal/app"
	"fleetplatform/server/internal/store"
)

// Revoked and expired enrollment keys fail enroll.request with the same
// terminal auth_failed as an unknown key; tokens issued earlier keep working.
func TestEnrollKeyLifecycleEnrollRefusesDeadKeys(t *testing.T) {
	st, err := store.OpenSqlite(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	f, err := st.CreateFleet("club")
	if err != nil {
		t.Fatal(err)
	}
	a := app.New(app.Config{HeartbeatInterval: time.Minute, LeaseTTL: time.Minute, SweepEvery: time.Second}, st)

	enroll := func(key, name string) (protocol.EnrollResponse, *protocol.ErrorMsg) {
		return a.Enroll(protocol.EnrollRequest{EnrollmentKey: key, Kind: "robot", Name: name})
	}

	revokedKey, rk, err := st.MintEnrollKey(f.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	early, e := enroll(revokedKey, "early")
	if e != nil {
		t.Fatalf("enroll with valid key: %+v", e)
	}
	if _, err := st.RevokeEnrollKey(f.ID, rk.ID); err != nil {
		t.Fatal(err)
	}
	if _, e := enroll(revokedKey, "late"); e == nil || e.Code != protocol.ErrAuthFailed {
		t.Fatalf("enroll with revoked key: %+v, want %s", e, protocol.ErrAuthFailed)
	}

	expiringKey, _, err := st.MintEnrollKey(f.ID, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, e := enroll(expiringKey, "late"); e == nil || e.Code != protocol.ErrAuthFailed {
		t.Fatalf("enroll with expired key: %+v, want %s", e, protocol.ErrAuthFailed)
	}

	if c, e := a.Hello(protocol.Hello{Token: early.Token}); e != nil || c.Name != "early" {
		t.Fatalf("hello with token issued before revocation: %+v %+v", c, e)
	}
}
