package main

import (
	"testing"

	"fleetplatform/sdk/go/fleet"
	"fleetplatform/sdk/go/protocol"
)

func snapshot(robots ...fleet.RobotSummary) fleet.Snapshot { return fleet.Snapshot{Robots: robots} }

func robot(id, presence, state string) fleet.RobotSummary {
	return fleet.RobotSummary{RobotID: id, Presence: presence, State: state}
}

// The one rule the example exists to show: a job only ever goes to a robot that
// is online and AUTONOMOUS.
func TestPickSkipsRobotsThatAreOfflineOrNotAutonomous(t *testing.T) {
	w := &world{}
	w.reset(snapshot(
		robot("r_offline", "offline", protocol.StateAutonomous),
		robot("r_teleop", "online", protocol.StateTeleop),
		robot("r_help", "online", protocol.StateHelpRequested),
	))
	if id, err := w.pick(); err == nil {
		t.Fatalf("pick = %s, want none", id)
	}

	w.reset(snapshot(
		robot("r_offline", "offline", protocol.StateAutonomous),
		robot("r_teleop", "online", protocol.StateTeleop),
		robot("r_help", "online", protocol.StateHelpRequested),
		robot("r_ready", "online", protocol.StateAutonomous),
	))
	for range 50 { // map order is random: the same answer every time
		if id, err := w.pick(); err != nil || id != "r_ready" {
			t.Fatalf("pick = %q, %v; want r_ready", id, err)
		}
	}
}

// A snapshot replaces the model: what the last one said and the next one does
// not is gone.
func TestResetForgetsThePreviousSnapshot(t *testing.T) {
	w := &world{}
	w.reset(snapshot(robot("r_1", "online", protocol.StateAutonomous)))
	w.reset(snapshot(robot("r_1", "online", protocol.StateTeleop)))
	if id, err := w.pick(); err == nil {
		t.Fatalf("pick = %s after a snapshot that put it in TELEOP", id)
	}
	w.reset(snapshot())
	if id, err := w.pick(); err == nil {
		t.Fatalf("pick = %s after an empty snapshot", id)
	}
}
