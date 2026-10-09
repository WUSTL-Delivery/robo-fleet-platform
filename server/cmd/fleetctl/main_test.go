package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"fleetplatform/sdk/go/protocol"
)

const (
	e2eFleet      = "club"
	e2eAdminToken = "fleetctl-e2e-admin-token-0123456789"
)

// buildBinaries compiles fleetctl and fleet-server into dir.
func buildBinaries(t *testing.T, dir string) (fleetctl, server string) {
	t.Helper()
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	fleetctl = filepath.Join(dir, "fleetctl"+exe)
	server = filepath.Join(dir, "fleet-server"+exe)
	for bin, pkg := range map[string]string{fleetctl: ".", server: "../fleet-server"} {
		cmd := exec.Command("go", "build", "-ldflags", "-X main.version=e2e-test", "-o", bin, pkg)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", pkg, err, out)
		}
	}
	return fleetctl, server
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// startServer runs fleet-server with a bootstrap fleet and the admin API on,
// and waits until /healthz answers.
func startServer(t *testing.T, bin, dir string) string {
	t.Helper()
	addr := freePort(t)
	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"FLEET_LISTEN="+addr,
		"FLEET_DB="+filepath.Join(dir, "fleet.db"),
		"FLEET_BOOTSTRAP_FLEET="+e2eFleet,
		"FLEET_BOOTSTRAP_ENROLL_KEY=fleetctl-e2e-robot-enroll-key-0123456789",
		"FLEET_ADMIN_TOKEN="+e2eAdminToken,
	)
	var logs bytes.Buffer
	cmd.Stdout, cmd.Stderr = &logs, &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		if t.Failed() {
			t.Logf("fleet-server output:\n%s", logs.String())
		}
	})
	base := "http://" + addr
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(base + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("fleet-server did not come up on %s\n%s", addr, logs.String())
	return ""
}

type cli struct {
	bin string
	env []string
}

func (c cli) run(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := exec.Command(c.bin, args...)
	cmd.Env = c.env
	cmd.Stdin = strings.NewReader(stdin)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err = cmd.Run()
	return o.String(), e.String(), err
}

// cleanEnv is the parent env minus anything that would leak a real login
// into the test, with HOME and XDG_CONFIG_HOME pointed at home.
func cleanEnv(home string, extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		switch k, _, _ := strings.Cut(kv, "="); k {
		case "HOME", "XDG_CONFIG_HOME", envServer, envToken:
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	return append(env, extra...)
}

var keyLine = regexp.MustCompile(`(?m)^\s+(\S+)\s*$`)

func parseKey(t *testing.T, out string) string {
	t.Helper()
	m := keyLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no key line in fleetctl output:\n%s", out)
	}
	return m[1]
}

func enrollOperator(t *testing.T, base, key, name string) protocol.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(base, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	data, err := json.Marshal(protocol.Msg(protocol.TypeEnrollRequest, protocol.EnrollRequest{
		EnrollmentKey: key, Kind: "operator", Name: name,
	}))
	if err != nil {
		t.Fatal(err)
	}
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

// TestFleetctlInviteEnrollsOperatorOnce is the whole operator-access path
// with real binaries: login, mint an invite, redeem it over /ws exactly once.
func TestFleetctlInviteEnrollsOperatorOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs binaries")
	}
	tmp := t.TempDir()
	fleetctlBin, serverBin := buildBinaries(t, tmp)
	base := startServer(t, serverBin, tmp)
	home := filepath.Join(tmp, "home")
	c := cli{bin: fleetctlBin, env: cleanEnv(home)}

	if out, _, err := c.run(t, "", "--version"); err != nil || strings.TrimSpace(out) != "e2e-test" {
		t.Fatalf("--version = %q err=%v", out, err)
	}

	// Before login, invite fails with a clear hint.
	if _, errOut, err := c.run(t, "", "invite", "operator", "--fleet", e2eFleet); err == nil || !strings.Contains(errOut, "not logged in") {
		t.Fatalf("invite before login: err=%v stderr=%q", err, errOut)
	}

	// Token on stdin, trailing slash on the server URL.
	if out, errOut, err := c.run(t, e2eAdminToken+"\n", "login", "--server", base+"/"); err != nil {
		t.Fatalf("login: %v\n%s%s", err, out, errOut)
	}
	cfgPath := filepath.Join(home, ".config", "fleetctl", "config.json")
	fi, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, want 0600", fi.Mode().Perm())
	}
	var saved config
	raw, _ := os.ReadFile(cfgPath)
	if err := json.Unmarshal(raw, &saved); err != nil || saved.Server != base || saved.Token != e2eAdminToken {
		t.Fatalf("saved config = %s (err %v)", raw, err)
	}

	out, errOut, err := c.run(t, "", "invite", "operator", "--fleet", e2eFleet, "--ttl", "2h")
	if err != nil {
		t.Fatalf("invite: %v\n%s%s", err, out, errOut)
	}
	if !strings.Contains(out, base+"/") {
		t.Fatalf("invite output lacks console URL %s/:\n%s", base, out)
	}
	key := parseKey(t, out)

	env := enrollOperator(t, base, key, "alice")
	if env.Type != protocol.TypeEnrollResponse {
		t.Fatalf("first redeem: got %s %s", env.Type, env.Payload)
	}
	var enrolled protocol.EnrollResponse
	if err := json.Unmarshal(env.Payload, &enrolled); err != nil || enrolled.Token == "" {
		t.Fatalf("enroll response %s (err %v)", env.Payload, err)
	}

	env = enrollOperator(t, base, key, "mallory")
	if env.Type != protocol.TypeError {
		t.Fatalf("second redeem succeeded: %s %s", env.Type, env.Payload)
	}
	var e protocol.ErrorMsg
	if err := json.Unmarshal(env.Payload, &e); err != nil || e.Code != protocol.ErrConflict {
		t.Fatalf("second redeem error = %s, want code %q", env.Payload, protocol.ErrConflict)
	}

	// Error paths: unknown fleet, bad ttl.
	if _, errOut, err := c.run(t, "", "invite", "operator", "--fleet", "nope"); err == nil || !strings.Contains(errOut, "no such fleet") {
		t.Fatalf("unknown fleet: err=%v stderr=%q", err, errOut)
	}
	if _, errOut, err := c.run(t, "", "invite", "operator", "--fleet", e2eFleet, "--ttl", "10000h"); err == nil || !strings.Contains(errOut, "ttl") {
		t.Fatalf("oversized ttl: err=%v stderr=%q", err, errOut)
	}
}

// TestFleetctlEnvOverrides: FLEETCTL_SERVER / FLEETCTL_TOKEN work with no
// config file, and a wrong token is reported as such.
func TestFleetctlEnvOverrides(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs binaries")
	}
	tmp := t.TempDir()
	fleetctlBin, serverBin := buildBinaries(t, tmp)
	base := startServer(t, serverBin, tmp)
	home := filepath.Join(tmp, "home")

	c := cli{bin: fleetctlBin, env: cleanEnv(home, envServer+"="+base, envToken+"="+e2eAdminToken)}
	out, errOut, err := c.run(t, "", "invite", "operator", "--fleet", e2eFleet)
	if err != nil {
		t.Fatalf("invite via env: %v\n%s%s", err, out, errOut)
	}
	if key := parseKey(t, out); !strings.HasPrefix(key, "fp-oi-") {
		t.Fatalf("key %q lacks fp-oi- prefix", key)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "fleetctl")); !os.IsNotExist(err) {
		t.Fatalf("env-only run touched the config dir: %v", err)
	}

	bad := cli{bin: fleetctlBin, env: cleanEnv(home, envServer+"="+base, envToken+"=wrong-token-wrong-token")}
	if _, errOut, err := bad.run(t, "", "invite", "operator", "--fleet", e2eFleet); err == nil || !strings.Contains(errOut, "admin token rejected") {
		t.Fatalf("wrong token: err=%v stderr=%q", err, errOut)
	}
}

func TestNormalizeServer(t *testing.T) {
	for in, want := range map[string]string{
		"https://fleet.example.org/": "https://fleet.example.org",
		"http://127.0.0.1:8080":      "http://127.0.0.1:8080",
		"https://h/sub/path/":        "https://h/sub/path",
	} {
		if got, err := normalizeServer(in); err != nil || got != want {
			t.Errorf("normalizeServer(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "fleet.example.org", "ftp://h", "https://"} {
		if _, err := normalizeServer(in); err == nil {
			t.Errorf("normalizeServer(%q) accepted", in)
		}
	}
}
