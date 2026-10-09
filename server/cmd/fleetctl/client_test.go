package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"fleetplatform/sdk/go/protocol"
)

// clientRevokeHello dials /ws, says hello, and returns the socket and the reply.
func clientRevokeHello(t *testing.T, base, token string) (*websocket.Conn, protocol.Envelope) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	data, _ := json.Marshal(protocol.Msg(protocol.TypeHello, protocol.Hello{Token: token}))
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
	return ws, env
}

func clientRevokeEnroll(t *testing.T, base, key, name string) protocol.EnrollResponse {
	t.Helper()
	env := enrollKeyLifecycleEnroll(t, base, key, name)
	var r protocol.EnrollResponse
	if env.Type != protocol.TypeEnrollResponse || json.Unmarshal(env.Payload, &r) != nil {
		t.Fatalf("enroll %s: %s %s", name, env.Type, env.Payload)
	}
	return r
}

// TestFleetctlClientRevoke: list clients, revoke a connected robot with the
// real binaries; its socket gets auth_failed and closes, its token is refused
// on the next hello, and the other robot keeps working.
func TestFleetctlClientRevoke(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs binaries")
	}
	tmp := t.TempDir()
	fleetctlBin, serverBin := buildBinaries(t, tmp)
	base := startServer(t, serverBin, tmp)
	c := cli{bin: fleetctlBin, env: cleanEnv(filepath.Join(tmp, "home"), envServer+"="+base, envToken+"="+e2eAdminToken)}
	const bootstrapKey = "fleetctl-e2e-robot-enroll-key-0123456789" // startServer's FLEET_BOOTSTRAP_ENROLL_KEY

	lost := clientRevokeEnroll(t, base, bootstrapKey, "lost-01")
	kept := clientRevokeEnroll(t, base, bootstrapKey, "kept-02")
	lostWS, env := clientRevokeHello(t, base, lost.Token)
	if env.Type != protocol.TypeWelcome {
		t.Fatalf("lost robot hello: %s %s", env.Type, env.Payload)
	}

	out, errOut, err := c.run(t, "", "client", "list", "--fleet", e2eFleet)
	if err != nil {
		t.Fatalf("list: %v\n%s", err, errOut)
	}
	if !strings.Contains(out, lost.ClientID) || !strings.Contains(out, kept.ClientID) || strings.Count(out, "active") != 2 || strings.Contains(out, "fp-tk-") {
		t.Fatalf("list output (want 2 active robots, no tokens):\n%s", out)
	}

	out, errOut, err = c.run(t, "", "client", "revoke", lost.ClientID, "--fleet", e2eFleet)
	if err != nil || !strings.Contains(out, "revoked robot "+lost.ClientID) || !strings.Contains(out, "live connection was closed") {
		t.Fatalf("revoke: %v\n%s%s", err, out, errOut)
	}

	// The live socket: one auth_failed error, then closed.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sawAuthFailed := false
	for {
		_, data, err := lostWS.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				t.Fatal("revoked robot's socket was not closed")
			}
			break
		}
		var env protocol.Envelope
		var e protocol.ErrorMsg
		if json.Unmarshal(data, &env) == nil && env.Type == protocol.TypeError && json.Unmarshal(env.Payload, &e) == nil && e.Code == protocol.ErrAuthFailed {
			sawAuthFailed = true
		}
	}
	if !sawAuthFailed {
		t.Fatal("revoked robot was closed without error{auth_failed}")
	}

	_, env = clientRevokeHello(t, base, lost.Token)
	requireEnrollKeyLifecycleAuthFailed(t, env, "hello with revoked token")
	if _, env := clientRevokeHello(t, base, kept.Token); env.Type != protocol.TypeWelcome {
		t.Fatalf("other robot refused after revoke: %s %s", env.Type, env.Payload)
	}

	out, _, err = c.run(t, "", "client", "list", "--fleet", e2eFleet)
	if err != nil || strings.Count(out, " revoked ") != 1 || strings.Count(out, " active ") != 1 {
		t.Fatalf("list after revoke: %v\n%s", err, out)
	}
	if out, _, err := c.run(t, "", "client", "revoke", "--fleet", e2eFleet, lost.ClientID); err != nil || strings.Contains(out, "live connection was closed") {
		t.Fatalf("second revoke (idempotent, nothing to close): %v\n%s", err, out)
	}

	// Error paths.
	if _, errOut, err := c.run(t, "", "client", "revoke", "--fleet", e2eFleet, "r_nope"); err == nil || !strings.Contains(errOut, "no such client") {
		t.Fatalf("revoke unknown id: err=%v stderr=%q", err, errOut)
	}
	if _, errOut, err := c.run(t, "", "client", "list", "--fleet", "nope"); err == nil || !strings.Contains(errOut, "no such fleet") {
		t.Fatalf("list unknown fleet: err=%v stderr=%q", err, errOut)
	}
	if _, _, err := c.run(t, "", "client", "revoke", "--fleet", e2eFleet); err == nil {
		t.Fatal("revoke without an id succeeded")
	}
	if _, _, err := c.run(t, "", "client", "kick"); err == nil {
		t.Fatal("unknown subcommand succeeded")
	}
}
