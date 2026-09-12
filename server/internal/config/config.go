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
}

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
)

func (c *Config) applyEnv(lookup func(string) (string, bool)) error {
	if v, ok := lookup(EnvListen); ok {
		c.Listen = v
	}
	if v, ok := lookup(EnvDB); ok {
		c.DB = v
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
	}
	return c, nil
}

func (c Config) HeartbeatInterval() time.Duration {
	return time.Duration(c.HeartbeatIntervalMs) * time.Millisecond
}
func (c Config) LeaseTTL() time.Duration   { return time.Duration(c.LeaseTTLMs) * time.Millisecond }
func (c Config) SweepEvery() time.Duration { return time.Duration(c.SweepMs) * time.Millisecond }
