// The intervention queue: robots that asked for help and have no driver yet,
// longest wait first.
//
// It is a pure selector over FleetState, so every console that has seen the
// same snapshot and events shows the same queue in the same order. Order comes
// only from `help.requested_at_ms`, which the server stamps on its own clock;
// no local clock takes part in it. A claim (robot.lease_granted) moves the
// robot to TELEOP in fleetReducer, which is what removes its entry here, for
// the claimer and for everyone else watching.
import type { HelpDetails } from "@fleet-platform/sdk";
import type { FleetState, RobotView } from "./model";

export interface QueueEntry {
  robot: RobotView;
  /**
   * What the robot asked for. Missing only when the server said the robot is
   * waiting without saying since when; such entries sort last.
   */
  help?: HelpDetails;
}

/** Robots in HELP_REQUESTED, oldest request first (ties broken by robot id). */
export function interventionQueue(state: FleetState): QueueEntry[] {
  const entries: QueueEntry[] = [];
  for (const robot of state.robots.values()) {
    if (robot.state !== "HELP_REQUESTED") continue;
    entries.push(robot.help ? { robot, help: robot.help } : { robot });
  }
  return entries.sort((a, b) => {
    const ta = a.help?.requested_at_ms ?? Infinity;
    const tb = b.help?.requested_at_ms ?? Infinity;
    if (ta !== tb) return ta < tb ? -1 : 1;
    return a.robot.robot_id < b.robot.robot_id ? -1 : a.robot.robot_id > b.robot.robot_id ? 1 : 0;
  });
}

/**
 * How long the entry has waited at `serverNowMs` (an estimate of the server's
 * clock, see serverClock.ts). Never negative: an estimate that trails the
 * server by the network delay must not show a robot waiting "-1s".
 */
export function waitMs(entry: QueueEntry, serverNowMs: number): number | undefined {
  if (!entry.help) return undefined;
  return Math.max(0, serverNowMs - entry.help.requested_at_ms);
}

/** "42s", "3m 05s", "1h 02m". */
export function formatWait(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const s = total % 60;
  const m = Math.floor(total / 60) % 60;
  const h = Math.floor(total / 3600);
  const pad = (n: number) => String(n).padStart(2, "0");
  if (h > 0) return `${h}h ${pad(m)}m`;
  if (m > 0) return `${m}m ${pad(s)}s`;
  return `${s}s`;
}

export interface ContextRow {
  key: string;
  value: string;
}

/**
 * The robot's `context`, flattened for display. The console does not interpret
 * it: keys are shown as sent, scalars as text, anything nested as JSON.
 */
export function contextRows(context: HelpDetails["context"]): ContextRow[] {
  if (!context) return [];
  return Object.entries(context).map(([key, v]) => ({
    key,
    value: typeof v === "string" ? v : (JSON.stringify(v) ?? String(v)),
  }));
}
