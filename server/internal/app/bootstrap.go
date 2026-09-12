package app

import (
	"fmt"
	"log/slog"

	"fleetplatform/server/internal/store"
)

// Bootstrap ensures fleetName exists and that key is a registered enrollment
// key for it. Idempotent: safe to run on every startup, so a deployment can
// declare "this fleet, this key" in its environment and a fresh box comes up
// ready for clients to enroll. The key is never logged.
func Bootstrap(st store.Store, fleetName, key string) error {
	if fleetID, ok, err := st.AuthEnroll(key); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	} else if ok {
		// The key must belong to the named fleet; a key already owned by some
		// other fleet is refused rather than silently re-homed.
		fleet, found, err := st.FleetByName(fleetName)
		if err != nil {
			return fmt.Errorf("bootstrap: %w", err)
		}
		if !found || fleet.ID != fleetID {
			return fmt.Errorf("bootstrap: enrollment key is already registered to a different fleet than %q", fleetName)
		}
		slog.Info("bootstrap: enrollment key already registered", "fleet", fleetName)
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
