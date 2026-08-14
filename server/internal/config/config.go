// Package config is the ~10-key bootstrap file (DESIGN.md D1): it describes the
// installation. The database describes the world; the wire describes the robots.
package config

import (
	"fmt"
	"os"
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

func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("config: %w", err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return cfg, nil
}

func (c Config) HeartbeatInterval() time.Duration { return time.Duration(c.HeartbeatIntervalMs) * time.Millisecond }
func (c Config) LeaseTTL() time.Duration          { return time.Duration(c.LeaseTTLMs) * time.Millisecond }
func (c Config) SweepEvery() time.Duration        { return time.Duration(c.SweepMs) * time.Millisecond }
