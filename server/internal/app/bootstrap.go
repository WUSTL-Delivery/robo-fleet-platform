package app

import (
	"fmt"
	"log/slog"
	"time"

	"fleetplatform/server/internal/store"
)

// Bootstrap ensures fleetName exists and that key is a registered enrollment
// key for it. Idempotent: safe to run on every startup, so a deployment can
// declare "this fleet, this key" in its environment and a fresh box comes up
// ready for clients to enroll. The key is never logged.
//
// Revocation wins over the environment: if the key was revoked (fleetctl
// enroll-key revoke) it stays revoked across restarts, and Bootstrap only warns
// that the environment still names a dead key. Startup is not failed for it,
// so a revocation never takes the server down; rotate by setting a new key.
func Bootstrap(st store.Store, fleetName, key string) error {
	if k, found, err := st.LookupEnrollKey(key); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	} else if found {
		// The key must belong to the named fleet; a key already owned by some
		// other fleet is refused rather than silently re-homed.
		fleet, ok, err := st.FleetByName(fleetName)
		if err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
		if !ok || fleet.ID != k.FleetID {
			return fmt.Errorf("bootstrap: enrollment key is already registered to a different fleet than %q", fleetName)
		}
		switch {
		case k.Revoked():
			slog.Warn("bootstrap: FLEET_BOOTSTRAP_ENROLL_KEY is revoked and stays revoked; set a new key to allow enrollment with it",
				"fleet", fleetName, "key_id", k.ID)
		case k.Expired(time.Now()):
			slog.Warn("bootstrap: FLEET_BOOTSTRAP_ENROLL_KEY has expired; set a new key to allow enrollment with it",
				"fleet", fleetName, "key_id", k.ID)
		default:
			slog.Info("bootstrap: enrollment key already registered", "fleet", fleetName, "key_id", k.ID)
		}
		return nil
	}
	fleet, ok, err := st.FleetByName(fleetName)
	if err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}
	if !ok {
		if fleet, err = st.CreateFleet(fleetName); err != nil {
			return fmt.Errorf("bootstrap: create fleet: %w", err)
		}
		slog.Info("bootstrap: created fleet", "fleet", fleet.Name, "id", fleet.ID)
	}
	if err := st.SeedEnrollKey(fleet.ID, key); err != nil {
		return fmt.Errorf("bootstrap: seed enrollment key: %w", err)
	}
	slog.Info("bootstrap: registered enrollment key", "fleet", fleet.Name)
	return nil
}
