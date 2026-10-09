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
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen              string `yaml:"listen"`
	DB                  string `yaml:"db"`
	HeartbeatIntervalMs int    `yaml:"heartbeat_interval_ms"`
	LeaseTTLMs          int    `yaml:"lease_ttl_ms"`
	SweepMs             int    `yaml:"sweep_ms"`

	// Per-connection inbound limits on the chatty message types (telemetry,
	// channel.publish, watch), each metered separately (DESIGN.md D2). Over-limit
	// messages are dropped with a rate_limited notice; the socket stays open.
	ClientMsgsPerSec int `yaml:"client_msgs_per_sec"`
	ClientMsgsBurst  int `yaml:"client_msgs_burst"`

	// Optional declarative bootstrap: on startup, ensure this fleet exists and
	// that this enrollment key is registered for it. Lets an operator choose the
	// key up front (e.g. a CI secret shared with the clients that will enroll)
	// instead of minting one on the box with -bootstrap. Idempotent.
	BootstrapFleet     string `yaml:"bootstrap_fleet"`
	BootstrapEnrollKey string `yaml:"bootstrap_enroll_key"`

	// AdminToken authenticates the admin HTTP API that fleetctl calls
	// (DESIGN.md D14). Empty (the default) leaves the admin API unmounted.
	AdminToken string `yaml:"admin_token"`

	// The console map's home view: where it opens and, with map_lock, the area
	// it stays inside. Installation config, not domain logic: the platform only
	// passes it to the console. Unset leaves the console fitting to the fleet.
	MapCenter  string  `yaml:"map_center"`   // "lat,lon", e.g. "38.6488,-90.3108"
	MapRadiusM float64 `yaml:"map_radius_m"` // metres around the centre to show
	MapLock    bool    `yaml:"map_lock"`     // keep panning and zooming inside that area

	mapLat, mapLon float64 // parsed from MapCenter by validate
}

// DefaultMapRadiusM applies when map_center is set without map_radius_m.
const DefaultMapRadiusM = 1000

// MinEnrollKeyLen guards against seeding a guessable key.
const MinEnrollKeyLen = 16

// MinAdminTokenLen guards against a guessable admin token.
const MinAdminTokenLen = 16

func Default() Config {
	return Config{
		Listen:              ":8080",
		DB:                  "fleet.db",
		HeartbeatIntervalMs: 10000,
		LeaseTTLMs:          15000,
		SweepMs:             1000,
		ClientMsgsPerSec:    50,
		ClientMsgsBurst:     100,
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
	EnvClientMsgsPerSec    = "FLEET_CLIENT_MSGS_PER_SEC"
	EnvClientMsgsBurst     = "FLEET_CLIENT_MSGS_BURST"
	EnvBootstrapFleet      = "FLEET_BOOTSTRAP_FLEET"
	EnvBootstrapEnrollKey  = "FLEET_BOOTSTRAP_ENROLL_KEY"
	EnvAdminToken          = "FLEET_ADMIN_TOKEN"
	EnvMapCenter           = "FLEET_MAP_CENTER"
	EnvMapRadiusM          = "FLEET_MAP_RADIUS_M"
	EnvMapLock             = "FLEET_MAP_LOCK"
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
	if v, ok := lookup(EnvAdminToken); ok {
		c.AdminToken = v
	}
	if v, ok := lookup(EnvMapCenter); ok {
		c.MapCenter = v
	}
	if v, ok := lookup(EnvMapRadiusM); ok && v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("config: %s=%q is not a number", EnvMapRadiusM, v)
		}
		c.MapRadiusM = f
	}
	if v, ok := lookup(EnvMapLock); ok && v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config: %s=%q is not true or false", EnvMapLock, v)
		}
		c.MapLock = b
	}
	for _, e := range []struct {
		name string
		dst  *int
	}{
		{EnvHeartbeatIntervalMs, &c.HeartbeatIntervalMs},
		{EnvLeaseTTLMs, &c.LeaseTTLMs},
		{EnvSweepMs, &c.SweepMs},
		{EnvClientMsgsPerSec, &c.ClientMsgsPerSec},
		{EnvClientMsgsBurst, &c.ClientMsgsBurst},
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
	case c.ClientMsgsPerSec <= 0:
		return c, fmt.Errorf("config: client_msgs_per_sec must be > 0")
	case c.ClientMsgsBurst <= 0:
		return c, fmt.Errorf("config: client_msgs_burst must be > 0")
	case (c.BootstrapFleet == "") != (c.BootstrapEnrollKey == ""):
		return c, fmt.Errorf("config: bootstrap_fleet and bootstrap_enroll_key must be set together")
	case c.BootstrapEnrollKey != "" && len(c.BootstrapEnrollKey) < MinEnrollKeyLen:
		return c, fmt.Errorf("config: bootstrap_enroll_key must be at least %d characters", MinEnrollKeyLen)
	case c.AdminToken != "" && len(c.AdminToken) < MinAdminTokenLen:
		return c, fmt.Errorf("config: admin_token must be at least %d characters", MinAdminTokenLen)
	case c.MapRadiusM < 0:
		return c, fmt.Errorf("config: map_radius_m must be > 0")
	case c.MapCenter == "" && (c.MapRadiusM != 0 || c.MapLock):
		return c, fmt.Errorf("config: map_radius_m and map_lock need map_center")
	}
	if c.MapCenter != "" {
		lat, lon, err := parseLatLon(c.MapCenter)
		if err != nil {
			return c, fmt.Errorf("config: map_center: %w", err)
		}
		c.mapLat, c.mapLon = lat, lon
		if c.MapRadiusM == 0 {
			c.MapRadiusM = DefaultMapRadiusM
		}
	}
	return c, nil
}

func parseLatLon(s string) (lat, lon float64, err error) {
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("%q is not \"lat,lon\"", s)
	}
	if lat, err = strconv.ParseFloat(strings.TrimSpace(parts[0]), 64); err != nil || lat < -90 || lat > 90 {
		return 0, 0, fmt.Errorf("%q: latitude must be a number in [-90, 90]", s)
	}
	if lon, err = strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err != nil || lon < -180 || lon > 180 {
		return 0, 0, fmt.Errorf("%q: longitude must be a number in [-180, 180]", s)
	}
	return lat, lon, nil
}

// MapView is the console's configured home view, or nil when none is set.
type MapView struct {
	Lat, Lon float64
	RadiusM  float64
	Lock     bool
}

// Map returns the console map's home view, or nil if map_center is unset.
func (c Config) Map() *MapView {
	if c.MapCenter == "" {
		return nil
	}
	return &MapView{Lat: c.mapLat, Lon: c.mapLon, RadiusM: c.MapRadiusM, Lock: c.MapLock}
}

// Bootstrap reports whether declarative bootstrap is configured.
func (c Config) Bootstrap() bool { return c.BootstrapFleet != "" }

// AdminEnabled reports whether the admin API should be mounted.
func (c Config) AdminEnabled() bool { return c.AdminToken != "" }

func (c Config) HeartbeatInterval() time.Duration {
	return time.Duration(c.HeartbeatIntervalMs) * time.Millisecond
}
func (c Config) LeaseTTL() time.Duration   { return time.Duration(c.LeaseTTLMs) * time.Millisecond }
func (c Config) SweepEvery() time.Duration { return time.Duration(c.SweepMs) * time.Millisecond }
