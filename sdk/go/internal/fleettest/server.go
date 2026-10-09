// Package fleettest runs the real fleet-server binary for the Go SDK's tests.
// Every SDK test talks to the built server, never a mock (docs/TESTING.md).
//
// The SDK must not import server packages (the dependency runs the other way),
// so the server is built with `go build` from ../../server, like the TypeScript
// and Python suites do. A test package wires it up once:
//
//	func TestMain(m *testing.M) { fleettest.Main(m) }
//
// and each test starts its own server (own port, own sqlite db, own fleet):
//
//	srv := fleettest.Start(t)
package fleettest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// HeartbeatInterval is what every test server tells clients to heartbeat at.
// Short, so a test can outlast several intervals and prove heartbeats keep a
// client online (the server drops a client after about 2.5 missed ones).
const HeartbeatInterval = 200 * time.Millisecond

var binPath string // set by Main

// Main builds fleet-server once, runs the package's tests, and removes the
// binary. Call it from TestMain.
func Main(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "fleet-sdk-go-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "fleettest:", err)
		return 1
	}
	defer os.RemoveAll(dir)
	bin := filepath.Join(dir, "fleet-server")
	if err := build(bin); err != nil {
		fmt.Fprintln(os.Stderr, "fleettest:", err)
		return 1
	}
	binPath = bin
	return m.Run()
}

func build(out string) error {
	serverDir, err := findServerDir()
	if err != nil {
		return err
	}
	// The server has its own, newer Go floor than the SDK, so the toolchain
	// running these tests may be too old to build it: under
	// `GOTOOLCHAIN=go1.24.0 go test ./...` the pinned toolchain's bin directory
	// is first on PATH. Try every go on PATH as it is installed (no download),
	// and only if none is new enough let the first one fetch what it needs.
	var failures []string
	for _, toolchain := range []string{"local", ""} {
		for _, goBin := range goBinaries() {
			cmd := exec.Command(goBin, "build", "-o", out, "./cmd/fleet-server")
			cmd.Dir = serverDir
			for _, kv := range os.Environ() {
				if !strings.HasPrefix(kv, "GOTOOLCHAIN=") && !strings.HasPrefix(kv, "GOROOT=") {
					cmd.Env = append(cmd.Env, kv)
				}
			}
			if toolchain != "" {
				cmd.Env = append(cmd.Env, "GOTOOLCHAIN="+toolchain)
			}
			b, err := cmd.CombinedOutput()
			if err == nil {
				return nil
			}
			failures = append(failures, fmt.Sprintf("%s (GOTOOLCHAIN=%q): %v\n%s", goBin, toolchain, err, b))
			if toolchain == "" {
				break
			}
		}
	}
	return fmt.Errorf("go build fleet-server in %s failed:\n%s", serverDir, strings.Join(failures, "\n"))
}

// goBinaries lists each go command on PATH, in PATH order.
func goBinaries() []string {
	name := "go"
	if runtime.GOOS == "windows" {
		name = "go.exe"
	}
	var out []string
	seen := map[string]bool{}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		bin := filepath.Join(dir, name)
		info, err := os.Stat(bin)
		if err != nil || info.IsDir() {
			continue
		}
		if real, err := filepath.EvalSymlinks(bin); err == nil {
			bin = real
		}
		if !seen[bin] {
			seen[bin] = true
			out = append(out, bin)
		}
	}
	if len(out) == 0 {
		out = []string{name}
	}
	return out
}

// findServerDir walks up from the test's working directory (the package
// directory) to the repo's server/ module.
func findServerDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		candidate := filepath.Join(dir, "server")
		if _, err := os.Stat(filepath.Join(candidate, "cmd", "fleet-server", "main.go")); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("server/cmd/fleet-server not found above the working directory")
		}
		dir = parent
	}
}

// Server is one running fleet-server.
type Server struct {
	// Addr is the host:port it listens on (loopback, ephemeral).
	Addr string
	// WSURL is the client endpoint, ws://<Addr>/ws.
	WSURL string
	// HTTPURL is http://<Addr>.
	HTTPURL string
	// Fleet is the name of the fleet declared at startup; EnrollKey enrolls into it.
	Fleet     string
	EnrollKey string
	// AdminToken authenticates the admin API (/api/admin/).
	AdminToken string

	log *syncBuffer
}

// Start runs a fresh fleet-server for this test and stops it when the test
// ends. Its log is printed if the test fails.
func Start(t testing.TB) *Server {
	t.Helper()
	if binPath == "" {
		t.Fatal("fleettest: binary not built; add `func TestMain(m *testing.M) { fleettest.Main(m) }` to this package")
	}
	dir := t.TempDir()
	addr := freeAddr(t)
	s := &Server{
		Addr:       addr,
		WSURL:      "ws://" + addr + "/ws",
		HTTPURL:    "http://" + addr,
		Fleet:      "sdk-go",
		EnrollKey:  "sdk-go-enroll-key-0123456789",
		AdminToken: "sdk-go-admin-token-0123456789",
		log:        &syncBuffer{},
	}
	cmd := exec.Command(binPath)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"FLEET_LISTEN="+addr,
		"FLEET_DB="+filepath.Join(dir, "fleet.db"),
		fmt.Sprintf("FLEET_HEARTBEAT_INTERVAL_MS=%d", HeartbeatInterval.Milliseconds()),
		"FLEET_SWEEP_MS=50",
		"FLEET_BOOTSTRAP_FLEET="+s.Fleet,
		"FLEET_BOOTSTRAP_ENROLL_KEY="+s.EnrollKey,
		"FLEET_ADMIN_TOKEN="+s.AdminToken,
	)
	cmd.Stdout = s.log
	cmd.Stderr = s.log
	if err := cmd.Start(); err != nil {
		t.Fatalf("fleettest: start fleet-server: %v", err)
	}
	exited := make(chan struct{})
	go func() {
		cmd.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			cmd.Process.Kill()
			<-exited
		}
		if t.Failed() {
			t.Logf("fleet-server log:\n%s", s.Log())
		}
	})

	deadline := time.Now().Add(15 * time.Second)
	for {
		select {
		case <-exited:
			t.Fatalf("fleettest: fleet-server exited during startup:\n%s", s.Log())
		default:
		}
		if res, err := http.Get(s.HTTPURL + "/healthz"); err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return s
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("fleettest: fleet-server not healthy in 15s:\n%s", s.Log())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Log is everything the server has printed so far.
func (s *Server) Log() string { return s.log.String() }

// ClientInfo is one row of the admin API's client list.
type ClientInfo struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	State string `json:"state"`
}

// Clients lists every client ever enrolled into the fleet, through the admin API.
func (s *Server) Clients(t testing.TB) []ClientInfo {
	t.Helper()
	var out struct {
		Clients []ClientInfo `json:"clients"`
	}
	s.admin(t, http.MethodGet, "/clients", &out)
	return out.Clients
}

// RevokeClient revokes one client's token and reports whether the server had to
// disconnect it.
func (s *Server) RevokeClient(t testing.TB, clientID string) (disconnected bool) {
	t.Helper()
	var out struct {
		Disconnected bool `json:"disconnected"`
	}
	s.admin(t, http.MethodPost, "/clients/"+clientID+"/revoke", &out)
	return out.Disconnected
}

// OperatorInvite mints a single-use operator invite key.
func (s *Server) OperatorInvite(t testing.TB) string {
	t.Helper()
	var out struct {
		Key string `json:"key"`
	}
	s.admin(t, http.MethodPost, "/operator-invites", &out)
	return out.Key
}

func (s *Server) admin(t testing.TB, method, path string, out any) {
	t.Helper()
	req, err := http.NewRequest(method, s.HTTPURL+"/api/admin/fleets/"+s.Fleet+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.AdminToken)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("fleettest: admin %s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("fleettest: admin %s %s: %s", method, path, res.Status)
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		t.Fatalf("fleettest: admin %s %s: %v", method, path, err)
	}
}

func freeAddr(t testing.TB) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
