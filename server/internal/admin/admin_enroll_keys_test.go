package admin_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"fleetplatform/server/internal/admin"
)

func (h *adminInviteHarness) enrollKeyLifecycleCall(method, path, auth, body string) (int, []byte) {
	h.t.Helper()
	req, err := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
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
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func TestAdminEnrollKeyLifecycleMintListRevoke(t *testing.T) {
	h := newAdminInviteHarness(t)
	bearer := "Bearer " + adminInviteTestToken
	base := "/api/admin/fleets/club/enroll-keys"

	// Mint: no ttl = never expires; ttl = expiring.
	status, raw := h.enrollKeyLifecycleCall(http.MethodPost, base, bearer, "")
	var forever admin.EnrollKeyInfo
	if status != http.StatusOK || json.Unmarshal(raw, &forever) != nil {
		t.Fatalf("mint: %d %s", status, raw)
	}
	if !strings.HasPrefix(forever.Key, "fp-ek-") || forever.ExpiresAt != nil || forever.State != admin.EnrollKeyActive || forever.FleetID != h.fleet.ID {
		t.Fatalf("mint forever = %+v", forever)
	}
	status, raw = h.enrollKeyLifecycleCall(http.MethodPost, base, bearer, `{"ttl":"48h"}`)
	var timed admin.EnrollKeyInfo
	if status != http.StatusOK || json.Unmarshal(raw, &timed) != nil || timed.ExpiresAt == nil {
		t.Fatalf("mint 48h: %d %s", status, raw)
	}
	for _, body := range []string{`{"ttl":"-1h"}`, `{"ttl":"0s"}`, `{"ttl":"soon"}`, `nope`} {
		if status, _ := h.enrollKeyLifecycleCall(http.MethodPost, base, bearer, body); status != http.StatusBadRequest {
			t.Errorf("mint %s: %d, want 400", body, status)
		}
	}

	// Enrolls until revoked.
	if _, ok, _ := h.store.AuthEnroll(forever.Key); !ok {
		t.Fatal("minted key does not enroll")
	}
	status, raw = h.enrollKeyLifecycleCall(http.MethodPost, base+"/"+forever.ID+"/revoke", bearer, "")
	var revoked admin.EnrollKeyInfo
	if status != http.StatusOK || json.Unmarshal(raw, &revoked) != nil || revoked.State != admin.EnrollKeyRevoked || revoked.RevokedAt == nil || revoked.Key != "" {
		t.Fatalf("revoke: %d %s", status, raw)
	}
	if _, ok, _ := h.store.AuthEnroll(forever.Key); ok {
		t.Fatal("revoked key still enrolls")
	}
	if _, ok, _ := h.store.AuthEnroll(timed.Key); !ok {
		t.Fatal("revoking one key revoked another")
	}
	if status, _ := h.enrollKeyLifecycleCall(http.MethodPost, base+"/"+forever.ID+"/revoke", bearer, ""); status != http.StatusOK {
		t.Errorf("second revoke: %d, want 200 (idempotent)", status)
	}
	if status, _ := h.enrollKeyLifecycleCall(http.MethodPost, base+"/ek_nope/revoke", bearer, ""); status != http.StatusNotFound {
		t.Errorf("unknown id: %d, want 404", status)
	}

	// List never carries plaintext.
	status, raw = h.enrollKeyLifecycleCall(http.MethodGet, base, bearer, "")
	var list admin.EnrollKeyList
	if status != http.StatusOK || json.Unmarshal(raw, &list) != nil || len(list.EnrollKeys) != 2 {
		t.Fatalf("list: %d %s", status, raw)
	}
	if strings.Contains(string(raw), "fp-ek-") {
		t.Fatalf("list leaks a plaintext key: %s", raw)
	}
	states := map[string]string{}
	for _, k := range list.EnrollKeys {
		states[k.ID] = k.State
	}
	if states[forever.ID] != admin.EnrollKeyRevoked || states[timed.ID] != admin.EnrollKeyActive {
		t.Fatalf("list states = %v", states)
	}

	// Fleet scoping and auth.
	if status, _ := h.enrollKeyLifecycleCall(http.MethodGet, "/api/admin/fleets/nope/enroll-keys", bearer, ""); status != http.StatusNotFound {
		t.Errorf("unknown fleet list: %d", status)
	}
	if _, err := h.store.CreateFleet("home"); err != nil {
		t.Fatal(err)
	}
	if status, _ := h.enrollKeyLifecycleCall(http.MethodPost, "/api/admin/fleets/home/enroll-keys/"+timed.ID+"/revoke", bearer, ""); status != http.StatusNotFound {
		t.Errorf("cross-fleet revoke: %d, want 404", status)
	}
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		if status, _ := h.enrollKeyLifecycleCall(m, base, "Bearer wrong", ""); status != http.StatusUnauthorized {
			t.Errorf("%s without admin token: %d, want 401", m, status)
		}
	}
}
