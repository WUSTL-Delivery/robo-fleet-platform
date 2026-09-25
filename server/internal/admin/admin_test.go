package admin_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fleetplatform/server/internal/admin"
	"fleetplatform/server/internal/app"
	"fleetplatform/server/internal/store"
	"fleetplatform/server/internal/web"
)

const adminInviteTestToken = "fp-admin-test-0123456789abcdef"

type adminInviteHarness struct {
	t     *testing.T
	srv   *httptest.Server
	store store.Store
	fleet store.Fleet
}

// newAdminInviteHarness runs the real HTTP surface (web.Handler with the admin
// API mounted) over a real sqlite store.
func newAdminInviteHarness(t *testing.T) *adminInviteHarness {
	t.Helper()
	st, err := store.OpenSqlite(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fleet, err := st.CreateFleet("club")
	if err != nil {
		t.Fatal(err)
	}
	a := app.New(app.Config{HeartbeatInterval: time.Minute, LeaseTTL: 30 * time.Second, SweepEvery: 50 * time.Millisecond}, st)
	srv := httptest.NewServer(web.Handler(a.Gateway(), admin.Handler(st, adminInviteTestToken)))
	t.Cleanup(srv.Close)
	return &adminInviteHarness{t: t, srv: srv, store: st, fleet: fleet}
}

func (h *adminInviteHarness) mintAdminInvite(fleet, auth, body string) (*http.Response, admin.InviteResponse) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/api/admin/fleets/"+fleet+"/operator-invites", strings.NewReader(body))
	if err != nil {
		h.t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out admin.InviteResponse
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			h.t.Fatal(err)
		}
	}
	return resp, out
}

func TestAdminInviteRejectsMissingOrWrongToken(t *testing.T) {
	h := newAdminInviteHarness(t)
	for _, auth := range []string{
		"",
		"Bearer wrong-token-0123456789",
		"Bearer " + adminInviteTestToken + "x",
		adminInviteTestToken, // no Bearer scheme
		"Basic " + adminInviteTestToken,
	} {
		resp, _ := h.mintAdminInvite("club", auth, "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("auth %q: status %d, want 401", auth, resp.StatusCode)
		}
	}
}

func TestAdminInviteMintsKeyWithDefaultAndCustomTTL(t *testing.T) {
	h := newAdminInviteHarness(t)
	bearer := "Bearer " + adminInviteTestToken

	before := time.Now()
	resp, inv := h.mintAdminInvite("club", bearer, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", resp.StatusCode)
	}
	if !strings.HasPrefix(inv.Key, "fp-oi-") || inv.FleetID != h.fleet.ID {
		t.Fatalf("invite = %+v", inv)
	}
	if d := inv.ExpiresAt.Sub(before); d < 24*time.Hour-time.Minute || d > 24*time.Hour+time.Minute {
		t.Fatalf("default ttl: expires in %s, want ~24h", d)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}

	resp, inv = h.mintAdminInvite("club", bearer, `{"ttl":"90m"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("custom ttl status %d", resp.StatusCode)
	}
	if d := inv.ExpiresAt.Sub(before); d < 89*time.Minute || d > 91*time.Minute {
		t.Fatalf("custom ttl: expires in %s, want ~90m", d)
	}

	for _, body := range []string{`{"ttl":"-1h"}`, `{"ttl":"soon"}`, `{"ttl":"721h"}`, `not json`} {
		if resp, _ := h.mintAdminInvite("club", bearer, body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %s: status %d, want 400", body, resp.StatusCode)
		}
	}
	if resp, _ := h.mintAdminInvite("no-such-fleet", bearer, ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown fleet: status %d, want 404", resp.StatusCode)
	}
}

func TestAdminInviteExpiredKeyIsRefused(t *testing.T) {
	h := newAdminInviteHarness(t)
	resp, inv := h.mintAdminInvite("club", "Bearer "+adminInviteTestToken, `{"ttl":"1ms"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	time.Sleep(20 * time.Millisecond)
	if _, _, err := h.store.RedeemOperatorInvite(inv.Key, "late"); !errors.Is(err, store.ErrInviteExpired) {
		t.Fatalf("redeem expired invite err = %v, want ErrInviteExpired", err)
	}
}

func TestAdminInviteAPIOffWithoutToken(t *testing.T) {
	if admin.Handler(nil, "") != nil {
		t.Fatal("empty admin token must disable the admin API")
	}
	st, err := store.OpenSqlite(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := app.New(app.Config{HeartbeatInterval: time.Minute, LeaseTTL: time.Minute, SweepEvery: time.Second}, st)
	srv := httptest.NewServer(web.Handler(a.Gateway(), admin.Handler(st, "")))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/api/admin/fleets/club/operator-invites", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404 when admin API is off", resp.StatusCode)
	}
}
