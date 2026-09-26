package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"fleetplatform/server/internal/protocol"
)

func enrollKeyLifecycleEnroll(t *testing.T, base, key, name string) protocol.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	data, _ := json.Marshal(protocol.Msg(protocol.TypeEnrollRequest, protocol.EnrollRequest{EnrollmentKey: key, Kind: "robot", Name: name}))
	if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
	_, reply, err := ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var env protocol.Envelope
	if err := json.Unmarshal(reply, &env); err != nil {
		t.Fatal(err)
	}
	return env
}

func requireEnrollKeyLifecycleAuthFailed(t *testing.T, env protocol.Envelope, what string) {
	t.Helper()
	var e protocol.ErrorMsg
	if env.Type != protocol.TypeError || json.Unmarshal(env.Payload, &e) != nil || e.Code != protocol.ErrAuthFailed {
		t.Fatalf("%s: got %s %s, want error %s", what, env.Type, env.Payload, protocol.ErrAuthFailed)
	}
}

var enrollKeyLifecycleID = regexp.MustCompile(`\bek_[0-9a-f]+\b`)

// TestFleetctlEnrollKeyLifecycle: create, enroll, list, revoke with real
// binaries, including revoking the FLEET_BOOTSTRAP_ENROLL_KEY key.
func TestFleetctlEnrollKeyLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs binaries")
	}
	tmp := t.TempDir()
	fleetctlBin, serverBin := buildBinaries(t, tmp)
	base := startServer(t, serverBin, tmp)
	c := cli{bin: fleetctlBin, env: cleanEnv(filepath.Join(tmp, "home"), envServer+"="+base, envToken+"="+e2eAdminToken)}
	const bootstrapKey = "fleetctl-e2e-robot-enroll-key-0123456789" // startServer's FLEET_BOOTSTRAP_ENROLL_KEY

	out, errOut, err := c.run(t, "", "enroll-key", "create", "--fleet", e2eFleet)
	if err != nil {
		t.Fatalf("create: %v\n%s%s", err, out, errOut)
	}
	key := parseKey(t, out)
	id := enrollKeyLifecycleID.FindString(out)
	if !strings.HasPrefix(key, "fp-ek-") || id == "" || !strings.Contains(out, "never expires") {
		t.Fatalf("create output:\n%s", out)
	}
	if out, _, err := c.run(t, "", "enroll-key", "create", "--fleet", e2eFleet, "--ttl", "48h"); err != nil || !strings.Contains(out, "expires ") {
		t.Fatalf("create --ttl: %v\n%s", err, out)
	}

	env := enrollKeyLifecycleEnroll(t, base, key, "early")
	if env.Type != protocol.TypeEnrollResponse {
		t.Fatalf("enroll with fresh key: %s %s", env.Type, env.Payload)
	}

	out, errOut, err = c.run(t, "", "enroll-key", "list", "--fleet", e2eFleet)
	if err != nil {
		t.Fatalf("list: %v\n%s", err, errOut)
	}
	if strings.Contains(out, "fp-ek-") || strings.Count(out, "active") != 3 || !strings.Contains(out, id) {
		t.Fatalf("list output (want 3 active keys incl. bootstrap, no plaintext):\n%s", out)
	}
	bootstrapID := ""
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 && strings.HasPrefix(f[0], "ek_") && f[0] != id && strings.Contains(line, "never") && bootstrapID == "" {
			bootstrapID = f[0]
		}
	}

	// Positional id before the flag works too.
	if out, errOut, err := c.run(t, "", "enroll-key", "revoke", id, "--fleet", e2eFleet); err != nil || !strings.Contains(out, "revoked enrollment key "+id) {
		t.Fatalf("revoke: %v\n%s%s", err, out, errOut)
	}
	requireEnrollKeyLifecycleAuthFailed(t, enrollKeyLifecycleEnroll(t, base, key, "late"), "enroll with revoked key")

	// The bootstrap key can be revoked the same way.
	if bootstrapID == "" {
		t.Fatalf("could not find the bootstrap key in list output:\n%s", out)
	}
	if _, errOut, err := c.run(t, "", "enroll-key", "revoke", "--fleet", e2eFleet, bootstrapID); err != nil {
		t.Fatalf("revoke bootstrap key: %v\n%s", err, errOut)
	}
	requireEnrollKeyLifecycleAuthFailed(t, enrollKeyLifecycleEnroll(t, base, bootstrapKey, "late"), "enroll with revoked bootstrap key")

	out, _, err = c.run(t, "", "enroll-key", "list", "--fleet", e2eFleet)
	if err != nil || strings.Count(out, " revoked ") != 2 {
		t.Fatalf("list after revoke: %v\n%s", err, out)
	}

	// Error paths.
	if _, errOut, err := c.run(t, "", "enroll-key", "revoke", "--fleet", e2eFleet, "ek_nope"); err == nil || !strings.Contains(errOut, "no such enrollment key") {
		t.Fatalf("revoke unknown id: err=%v stderr=%q", err, errOut)
	}
	if _, errOut, err := c.run(t, "", "enroll-key", "list", "--fleet", "nope"); err == nil || !strings.Contains(errOut, "no such fleet") {
		t.Fatalf("list unknown fleet: err=%v stderr=%q", err, errOut)
	}
	if _, _, err := c.run(t, "", "enroll-key", "revoke", "--fleet", e2eFleet); err == nil {
		t.Fatal("revoke without an id succeeded")
	}
	if _, _, err := c.run(t, "", "enroll-key", "rotate"); err == nil {
		t.Fatal("unknown subcommand succeeded")
	}
}
