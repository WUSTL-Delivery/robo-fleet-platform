package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMapUnsetMeansNoHomeView(t *testing.T) {
	cfg, err := Default().validate()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Map() != nil {
		t.Fatalf("Map() = %+v, want nil", cfg.Map())
	}
}

func TestMapFromEnv(t *testing.T) {
	cfg := Default()
	err := cfg.applyEnv(lookupFrom(map[string]string{
		EnvMapCenter:  " 38.6488 , -90.3108 ",
		EnvMapRadiusM: "1500",
		EnvMapLock:    "true",
	}))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = cfg.validate()
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Map()
	if m == nil || m.Lat != 38.6488 || m.Lon != -90.3108 || m.RadiusM != 1500 || !m.Lock {
		t.Fatalf("Map() = %+v", m)
	}
}

func TestMapFromFileWithDefaultRadius(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fleet.yml")
	if err := os.WriteFile(path, []byte("map_center: \"51.5,-0.12\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Map()
	if m == nil || m.Lat != 51.5 || m.Lon != -0.12 || m.RadiusM != DefaultMapRadiusM || m.Lock {
		t.Fatalf("Map() = %+v", m)
	}
}

func TestMapRejectsBadValues(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"one number":         {EnvMapCenter: "38.6"},
		"latitude too big":   {EnvMapCenter: "91,0"},
		"longitude too big":  {EnvMapCenter: "0,181"},
		"not numbers":        {EnvMapCenter: "north,west"},
		"negative radius":    {EnvMapCenter: "0,0", EnvMapRadiusM: "-5"},
		"radius, no center":  {EnvMapRadiusM: "500"},
		"lock, no center":    {EnvMapLock: "true"},
		"radius not numeric": {EnvMapCenter: "0,0", EnvMapRadiusM: "wide"},
		"lock not bool":      {EnvMapCenter: "0,0", EnvMapLock: "sure"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			if err := cfg.applyEnv(lookupFrom(env)); err != nil {
				return // rejected while parsing the env
			}
			if _, err := cfg.validate(); err == nil {
				t.Fatalf("accepted %v", env)
			}
		})
	}
}
