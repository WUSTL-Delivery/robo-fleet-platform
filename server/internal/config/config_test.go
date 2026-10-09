package config

import (
	"os"
	"path/filepath"
	"testing"
)

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestPrecedenceDefaultsFileEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.yml")
	if err := os.WriteFile(path, []byte("listen: \":9000\"\nlease_ttl_ms: 5000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvListen, "127.0.0.1:9100")
	t.Setenv(EnvSweepMs, "250")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9100" { // env beats file
		t.Errorf("listen = %q", cfg.Listen)
	}
	if cfg.LeaseTTLMs != 5000 { // file beats default
		t.Errorf("lease_ttl_ms = %d", cfg.LeaseTTLMs)
	}
	if cfg.SweepMs != 250 { // env beats default
		t.Errorf("sweep_ms = %d", cfg.SweepMs)
	}
	if cfg.HeartbeatIntervalMs != Default().HeartbeatIntervalMs { // untouched default
		t.Errorf("heartbeat_interval_ms = %d", cfg.HeartbeatIntervalMs)
	}
}

func TestEnvRejectsNonInteger(t *testing.T) {
	cfg := Default()
	if err := cfg.applyEnv(lookupFrom(map[string]string{EnvLeaseTTLMs: "soon"})); err == nil {
		t.Fatal("expected error for non-integer env value")
	}
}

func TestValidateRejectsNonPositive(t *testing.T) {
	cfg := Default()
	cfg.HeartbeatIntervalMs = 0
	if _, err := cfg.validate(); err == nil {
		t.Fatal("expected error for heartbeat_interval_ms = 0")
	}
}

func TestBootstrapPairValidation(t *testing.T) {
	cfg := Default()
	cfg.BootstrapFleet = "club-fleet"
	if _, err := cfg.validate(); err == nil {
		t.Fatal("fleet without key should fail")
	}
	cfg.BootstrapEnrollKey = "short"
	if _, err := cfg.validate(); err == nil {
		t.Fatal("short key should fail")
	}
	cfg.BootstrapEnrollKey = "0123456789abcdef0123456789abcdef"
	if _, err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	if !cfg.Bootstrap() {
		t.Fatal("Bootstrap() should be true")
	}
}

func TestAdminTokenFromEnvAndLength(t *testing.T) {
	cfg := Default()
	if cfg.AdminEnabled() {
		t.Fatal("admin API must be off by default")
	}
	if err := cfg.applyEnv(lookupFrom(map[string]string{EnvAdminToken: "short"})); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.validate(); err == nil {
		t.Fatal("short admin token should fail")
	}
	if err := cfg.applyEnv(lookupFrom(map[string]string{EnvAdminToken: "fp-admin-0123456789abcdef"})); err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.validate(); err != nil {
		t.Fatal(err)
	}
	if !cfg.AdminEnabled() || cfg.AdminToken != "fp-admin-0123456789abcdef" {
		t.Fatalf("admin token not applied: %+v", cfg)
	}
}

func TestClientRateLimitKeys(t *testing.T) {
	cfg := Default()
	if cfg.ClientMsgsPerSec <= 0 || cfg.ClientMsgsBurst <= 0 {
		t.Fatalf("rate limit defaults must be on: %+v", cfg)
	}
	if err := cfg.applyEnv(lookupFrom(map[string]string{EnvClientMsgsPerSec: "7", EnvClientMsgsBurst: "9"})); err != nil {
		t.Fatal(err)
	}
	if cfg.ClientMsgsPerSec != 7 || cfg.ClientMsgsBurst != 9 {
		t.Fatalf("env not applied: per_sec=%d burst=%d", cfg.ClientMsgsPerSec, cfg.ClientMsgsBurst)
	}
	cfg.ClientMsgsBurst = 0
	if _, err := cfg.validate(); err == nil {
		t.Fatal("expected error for client_msgs_burst = 0")
	}
}

func TestICEServersFromFileAndEnv(t *testing.T) {
	cfg := Default()
	if len(cfg.STUNURLs) != 0 || len(cfg.TURNURLs) != 0 || cfg.TURNSecret != "" {
		t.Fatalf("ICE servers must be unset by default: %+v", cfg)
	}
	if _, err := cfg.validate(); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "fleet.yml")
	yml := "stun_urls: [\"stun:turn.example.org:3478\"]\n" +
		"turn_urls:\n  - \"turn:turn.example.org:3478?transport=udp\"\n  - \"turns:turn.example.org:5349?transport=tcp\"\n" +
		"turn_secret: file-secret-0123456789\nturn_credential_ttl_s: 600\n"
	if err := os.WriteFile(path, []byte(yml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.STUNURLs) != 1 || len(cfg.TURNURLs) != 2 || cfg.TURNURLs[1] != "turns:turn.example.org:5349?transport=tcp" {
		t.Fatalf("file not applied: %+v", cfg)
	}
	if cfg.TURNSecret != "file-secret-0123456789" || cfg.TURNCredentialTTL().Seconds() != 600 {
		t.Fatalf("file not applied: %+v", cfg)
	}

	// The environment replaces the lists whole; they are comma-separated there.
	t.Setenv(EnvSTUNURLs, "")
	t.Setenv(EnvTURNURLs, " turn:10.0.0.5:3478 , turn:10.0.0.5:3478?transport=tcp ")
	t.Setenv(EnvTURNSecret, "env-secret-0123456789")
	t.Setenv(EnvTURNCredentialTTLS, "120")
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.STUNURLs) != 0 || len(cfg.TURNURLs) != 2 || cfg.TURNURLs[0] != "turn:10.0.0.5:3478" || cfg.TURNURLs[1] != "turn:10.0.0.5:3478?transport=tcp" {
		t.Fatalf("env not applied: %+v", cfg)
	}
	if cfg.TURNSecret != "env-secret-0123456789" || cfg.TURNCredentialTTLS != 120 {
		t.Fatalf("env not applied: %+v", cfg)
	}
}

func TestICEServersValidation(t *testing.T) {
	ok := func() Config {
		cfg := Default()
		cfg.STUNURLs = []string{"stun:turn.example.org:3478"}
		cfg.TURNURLs = []string{"turn:turn.example.org:3478"}
		cfg.TURNSecret = "0123456789abcdef"
		return cfg
	}
	if _, err := ok().validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"turn_urls without turn_secret": func(c *Config) { c.TURNSecret = "" },
		"turn_secret without turn_urls": func(c *Config) { c.TURNURLs = nil },
		"short turn_secret":             func(c *Config) { c.TURNSecret = "short" },
		"ttl below the floor":           func(c *Config) { c.TURNCredentialTTLS = 5 },
		"ttl above the ceiling":         func(c *Config) { c.TURNCredentialTTLS = 7 * 24 * 3600 },
		"stun url with a turn scheme":   func(c *Config) { c.STUNURLs = []string{"turn:turn.example.org:3478"} },
		"turn url with a stun scheme":   func(c *Config) { c.TURNURLs = []string{"stun:turn.example.org:3478"} },
		"url with no host":              func(c *Config) { c.TURNURLs = []string{"turn:"} },
		"url written like http":         func(c *Config) { c.STUNURLs = []string{"stun://turn.example.org:3478"} },
		"bare host":                     func(c *Config) { c.STUNURLs = []string{"turn.example.org:3478"} },
		"two urls in one entry":         func(c *Config) { c.TURNURLs = []string{"turn:a:3478 turn:b:3478"} },
	} {
		cfg := ok()
		mutate(&cfg)
		if _, err := cfg.validate(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
