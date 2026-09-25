// fleetctl: the admin CLI for a fleet-server installation (DESIGN.md D14).
//
//	fleetctl login --server https://fleet.example.org --token <admin token>
//	fleetctl login --server https://fleet.example.org   # token read from stdin
//	fleetctl invite operator --fleet club [--ttl 24h]
//	fleetctl enroll-key create --fleet club [--ttl 720h]
//	fleetctl enroll-key list --fleet club
//	fleetctl enroll-key revoke --fleet club <key id>
//	fleetctl --version
//
// fleetctl only talks HTTP to the server's admin API (FLEET_ADMIN_TOKEN on the
// server side); it never touches the database. login saves the server URL and
// admin token to $XDG_CONFIG_HOME/fleetctl/config.json (default
// ~/.config/fleetctl/config.json) with mode 0600. FLEETCTL_SERVER and
// FLEETCTL_TOKEN override the saved values, so scripts and CI need no login.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// version is overridden at build time: -ldflags "-X main.version=0.1.0".
var version = "dev"

const (
	envServer = "FLEETCTL_SERVER"
	envToken  = "FLEETCTL_TOKEN"
)

const usage = `fleetctl: admin CLI for a fleet-server installation

Usage:
  fleetctl login --server <url> [--token <admin token>]
      Save the server URL and admin token (token read from stdin if omitted).
  fleetctl invite operator --fleet <name> [--ttl 24h]
      Mint a single-use operator invite key and print it with the console URL.
  fleetctl enroll-key create --fleet <name> [--ttl 720h]
      Mint an enrollment key for robots and services (never expires without --ttl).
  fleetctl enroll-key list --fleet <name>
      List a fleet's enrollment keys: id, state, created, expires, revoked.
  fleetctl enroll-key revoke --fleet <name> <key id>
      Stop new enrollments with a key. Clients already enrolled keep their tokens.
  fleetctl version | --version
      Print the fleetctl version.

Environment:
  FLEETCTL_SERVER, FLEETCTL_TOKEN   override the saved login (no login needed)
  XDG_CONFIG_HOME                   config dir (default ~/.config)
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "--version", "-version", "version":
		fmt.Fprintln(stdout, version)
		return 0
	case "-h", "--help", "-help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "login":
		err = cmdLogin(args[1:], stdin, stdout, stderr)
	case "invite":
		err = cmdInvite(args[1:], stdout, stderr)
	case "enroll-key":
		err = cmdEnrollKey(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "fleetctl: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		var ue usageError
		if errors.As(err, &ue) {
			fmt.Fprintln(stderr, "fleetctl:", err)
			return 2
		}
		fmt.Fprintln(stderr, "fleetctl:", err)
		return 1
	}
	return 0
}

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// config is the on-disk login state.
type config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

func configPath() (string, error) {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot locate config dir: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "fleetctl", "config.json"), nil
}

func loadConfig() (config, error) {
	var c config
	path, err := configPath()
	if err != nil {
		return c, err
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	if err == nil {
		if err := json.Unmarshal(data, &c); err != nil {
			return c, fmt.Errorf("%s: %w", path, err)
		}
	}
	if v := os.Getenv(envServer); v != "" {
		c.Server = v
	}
	if v := os.Getenv(envToken); v != "" {
		c.Token = v
	}
	if c.Server == "" || c.Token == "" {
		return c, errors.New("not logged in: run `fleetctl login --server <url>` or set FLEETCTL_SERVER and FLEETCTL_TOKEN")
	}
	c.Server, err = normalizeServer(c.Server)
	return c, err
}

func saveConfig(c config) (string, error) {
	path, err := configPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return "", err
	}
	// Write to a temp file in the same dir, then rename, so a crash never
	// leaves a half-written token file. CreateTemp uses mode 0600.
	f, err := os.CreateTemp(filepath.Dir(path), ".config-*.json")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return "", err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

// normalizeServer checks the URL and strips any trailing slash.
func normalizeServer(s string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("server %q must be an http(s) URL like https://fleet.example.org", s)
	}
	u.RawQuery, u.Fragment = "", ""
	return strings.TrimRight(u.String(), "/"), nil
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

func cmdLogin(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	server := fs.String("server", "", "fleet-server URL, e.g. https://fleet.example.org")
	token := fs.String("token", "", "admin token (FLEET_ADMIN_TOKEN on the server); read from stdin if omitted")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("login: unexpected argument %q", fs.Arg(0))}
	}
	if *server == "" {
		return usageError{"login: --server is required"}
	}
	srv, err := normalizeServer(*server)
	if err != nil {
		return err
	}
	tok := *token
	if tok == "" {
		fmt.Fprint(stderr, "admin token: ")
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("reading token: %w", err)
		}
		tok = strings.TrimSpace(line)
	}
	if tok == "" {
		return usageError{"login: empty admin token"}
	}

	// Reachability check: a typo in --server should fail here, not at the
	// first invite. The token itself is checked by the first admin call.
	resp, err := httpClient.Get(srv + "/healthz")
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", srv, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s/healthz returned %s; is this a fleet-server?", srv, resp.Status)
	}

	path, err := saveConfig(config{Server: srv, Token: tok})
	if err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	fmt.Fprintf(stdout, "logged in to %s (saved to %s)\n", srv, path)
	return nil
}

type inviteResponse struct {
	Key       string    `json:"key"`
	FleetID   string    `json:"fleet_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func cmdInvite(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "operator" {
		return usageError{"invite: usage: fleetctl invite operator --fleet <name> [--ttl 24h]"}
	}
	fs := flag.NewFlagSet("invite operator", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fleet := fs.String("fleet", "", "fleet name (as created with -bootstrap / FLEET_BOOTSTRAP_FLEET)")
	ttl := fs.Duration("ttl", 0, "how long the invite stays redeemable (server default 24h, max 720h)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Sprintf("invite operator: unexpected argument %q", fs.Arg(0))}
	}
	if *fleet == "" {
		return usageError{"invite operator: --fleet is required"}
	}
	if *ttl < 0 {
		return usageError{"invite operator: --ttl must be positive"}
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	var body any
	if *ttl > 0 {
		body = map[string]string{"ttl": ttl.String()}
	}
	raw, err := adminCall(cfg, http.MethodPost, "/api/admin/fleets/"+url.PathEscape(*fleet)+"/operator-invites", body, *fleet)
	if err != nil {
		return err
	}
	var inv inviteResponse
	if err := json.Unmarshal(raw, &inv); err != nil || inv.Key == "" {
		return fmt.Errorf("unexpected response from server: %s", strings.TrimSpace(string(raw)))
	}

	fmt.Fprintf(stdout, "Operator invite for fleet %q (single use, expires %s):\n\n", *fleet, inv.ExpiresAt.Local().Format(time.RFC1123))
	fmt.Fprintf(stdout, "    %s\n\n", inv.Key)
	fmt.Fprintf(stdout, "Open the console at %s/ and paste the key on the login screen.\n", cfg.Server)
	return nil
}

// adminCall sends one admin API request (body is JSON-encoded unless nil) and
// returns the raw 200 response body, or an error worded for the terminal.
// fleet names the fleet in the path, for error messages.
func adminCall(cfg config, method, path string, body any, fleet string) ([]byte, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, cfg.Server+path, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach %s: %w", cfg.Server, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp, raw, fleet)
	}
	return raw, nil
}

func apiError(resp *http.Response, raw []byte, fleet string) error {
	var e struct {
		Error string `json:"error"`
	}
	msg := strings.TrimSpace(string(raw))
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		msg = e.Error
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf("admin token rejected (%s); run `fleetctl login` again", msg)
	case http.StatusNotFound:
		if e.Error == "" {
			// Plain 404 from the mux: the admin API is not mounted.
			return errors.New("admin API not found: is FLEET_ADMIN_TOKEN set on the server?")
		}
		return fmt.Errorf("fleet %q: %s", fleet, msg)
	default:
		return fmt.Errorf("server returned %s: %s", resp.Status, msg)
	}
}
