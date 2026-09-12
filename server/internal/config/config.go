// Package config is the ~10-key bootstrap file (DESIGN.md D1): it describes the
// installation. The database describes the world; the wire describes the robots.
//
// Precedence: defaults < yaml file (-config) < FLEET_* environment variables.
// The env layer exists for the container image: compose sets a handful of
// FLEET_* vars and nothing has to be bind-mounted.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen              string `yaml:"listen"`
	DB                  string `yaml:"db"`
	HeartbeatIntervalMs int    `yaml:"heartbeat_interval_ms"`
	LeaseTTLMs          int    `yaml:"lease_ttl_ms"`
	SweepMs             int    `yaml:"sweep_ms"`

	// Optional declarative bootstrap: on startup, ensure this fleet exists and
	// that this enrollment key is registered for it. Lets an operator choose the
	// key up front (e.g. a CI secret shared with the clients that will enroll)
	// instead of minting one on the box with -bootstrap. Idempotent.
	BootstrapFleet     string `yaml:"bootstrap_fleet"`
	BootstrapEnrollKey string `yaml:"bootstrap_enroll_key"`
}

// MinEnrollKeyLen guards against seeding a guessable key.
const MinEnrollKeyLen = 16

func Default() Config {
	return Config{
		Listen:              ":8080",
		DB:                  "fleet.db",
		HeartbeatIntervalMs: 10000,
		LeaseTTLMs:          15000,
		SweepMs:             1000,
	}
}

// Load returns defaults, overlaid by the yaml file at path (if non-empty),
// overlaid by FLEET_* environment variables.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("config: %w", err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}
	if err := cfg.applyEnv(os.LookupEnv); err != nil {
		return cfg, err
	}
	return cfg.validate()
}

// Environment variable names, one per key.
const (
	EnvListen              = "FLEET_LISTEN"
	EnvDB                  = "FLEET_DB"
	EnvHeartbeatIntervalMs = "FLEET_HEARTBEAT_INTERVAL_MS"
	EnvLeaseTTLMs          = "FLEET_LEASE_TTL_MS"
	EnvSweepMs             = "FLEET_SWEEP_MS"
	EnvBootstrapFleet      = "FLEET_BOOTSTRAP_FLEET"
	EnvBootstrapEnrollKey  = "FLEET_BOOTSTRAP_ENROLL_KEY"
)

func (c *Config) applyEnv(lookup func(string) (string, bool)) error {
	if v, ok := lookup(EnvListen); ok {
		c.Listen = v
	}
	if v, ok := lookup(EnvDB); ok {
		c.DB = v
	}
	if v, ok := lookup(EnvBootstrapFleet); ok {
		c.BootstrapFleet = v
	}
	if v, ok := lookup(EnvBootstrapEnrollKey); ok {
		c.BootstrapEnrollKey = v
	}
	for _, e := range []struct {
		name string
		dst  *int
	}{
		{EnvHeartbeatIntervalMs, &c.HeartbeatIntervalMs},
		{EnvLeaseTTLMs, &c.LeaseTTLMs},
		{EnvSweepMs, &c.SweepMs},
	} {
		v, ok := lookup(e.name)
		if !ok {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: %s=%q is not an integer", e.name, v)
		}
		*e.dst = n
	}
	return nil
}

func (c Config) validate() (Config, error) {
	switch {
	case c.Listen == "":
		return c, fmt.Errorf("config: listen must be set")
	case c.DB == "":
		return c, fmt.Errorf("config: db must be set")
	case c.HeartbeatIntervalMs <= 0:
		return c, fmt.Errorf("config: heartbeat_interval_ms must be > 0")
	case c.LeaseTTLMs <= 0:
		return c, fmt.Errorf("config: lease_ttl_ms must be > 0")
	case c.SweepMs <= 0:
		return c, fmt.Errorf("config: sweep_ms must be > 0")
	case (c.BootstrapFleet == "") != (c.BootstrapEnrollKey == ""):
		return c, fmt.Errorf("config: bootstrap_fleet and bootstrap_enroll_key must be set together")
	case c.BootstrapEnrollKey != "" && len(c.BootstrapEnrollKey) < MinEnrollKeyLen:
		return c, fmt.Errorf("config: bootstrap_enroll_key must be at least %d characters", MinEnrollKeyLen)
	}
	return c, nil
}

// Bootstrap reports whether declarative bootstrap is configured.
func (c Config) Bootstrap() bool { return c.BootstrapFleet != "" }

func (c Config) HeartbeatInterval() time.Duration {
	return time.Duration(c.HeartbeatIntervalMs) * time.Millisecond
}
func (c Config) LeaseTTL() time.Duration   { return time.Duration(c.LeaseTTLMs) * time.Millisecond }
func (c Config) SweepEvery() time.Duration { return time.Duration(c.SweepMs) * time.Millisecond }
