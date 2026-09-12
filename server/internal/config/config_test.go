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
