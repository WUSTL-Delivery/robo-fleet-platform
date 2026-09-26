// The console's picture of the fleet, rebuilt from the server's stream.
//
// State is a pure reducer over two inputs: a `snapshot` (on every subscribe,
// including after each reconnect) and the `event` envelopes that follow it.
// The snapshot is authoritative for membership, presence, FSM state, manifest
// and lease; it carries no telemetry, so the last telemetry is kept across a
// snapshot for robots that are still online and dropped for the rest.
import type {
  FleetEvent,
  Lease,
  Manifest,
  Pose,
  Presence,
  RobotState,
  RobotSummary,
  SnapshotPayload,
  TelemetryPayload,
} from "@fleet-platform/sdk";

export interface RobotView {
  robot_id: string;
  name?: string;
  presence: Presence;
  state: RobotState;
  manifest?: Manifest;
  lease?: Lease;
  /** Latest telemetry payload, verbatim. */
  telemetry?: TelemetryPayload;
  /** Local receive time of `telemetry` (ms since epoch). */
  telemetryAtMs?: number;
}

export interface FleetState {
  /** False until the first snapshot arrives. */
  synced: boolean;
  robots: ReadonlyMap<string, RobotView>;
}

export const emptyFleet: FleetState = { synced: false, robots: new Map() };

export type FleetAction =
  | { kind: "reset" }
  | { kind: "snapshot"; snapshot: SnapshotPayload }
  | { kind: "event"; event: FleetEvent; atMs: number };

export function fleetReducer(prev: FleetState, action: FleetAction): FleetState {
  if (action.kind === "reset") return emptyFleet;
  if (action.kind === "snapshot") return applySnapshot(prev, action.snapshot);
  const next = applyEvent(prev.robots.get(action.event.robot_id), action.event, action.atMs);
  if (!next) return prev;
  const robots = new Map(prev.robots);
  robots.set(next.robot_id, next);
  return { ...prev, robots };
}

function applySnapshot(prev: FleetState, snapshot: SnapshotPayload): FleetState {
  const robots = new Map<string, RobotView>();
  for (const s of snapshot.robots) {
    const old = prev.robots.get(s.robot_id);
    const view = fromSummary(s);
    if (s.presence === "online" && old?.telemetry) {
      view.telemetry = old.telemetry;
      if (old.telemetryAtMs !== undefined) view.telemetryAtMs = old.telemetryAtMs;
    }
    robots.set(s.robot_id, view);
  }
  return { synced: true, robots };
}

function fromSummary(s: RobotSummary): RobotView {
  const v: RobotView = { robot_id: s.robot_id, presence: s.presence, state: s.state };
  if (s.name !== undefined) v.name = s.name;
  if (s.manifest !== undefined) v.manifest = s.manifest;
  if (s.lease !== undefined) v.lease = s.lease;
  return v;
}

/** Returns the robot after `event`, or undefined when nothing changed. */
function applyEvent(old: RobotView | undefined, event: FleetEvent, atMs: number): RobotView | undefined {
  // A robot the snapshot did not list (first enrollment after we subscribed)
  // starts from defaults; the next snapshot fills in its name and manifest.
  const r: RobotView = old ? { ...old } : { robot_id: event.robot_id, presence: "offline", state: "AUTONOMOUS" };
  switch (event.event) {
    case "robot.online":
      r.presence = "online";
      return r;
    case "robot.offline":
      r.presence = "offline";
      delete r.telemetry;
      delete r.telemetryAtMs;
      return r;
    case "robot.telemetry":
      r.telemetry = event.data;
      r.telemetryAtMs = atMs;
      // Telemetry only flows from a connected robot.
      r.presence = "online";
      return r;
    case "robot.help_requested":
      r.state = "HELP_REQUESTED";
      return r;
    case "robot.lease_granted":
      r.state = "TELEOP";
      r.lease = event.data;
      return r;
    case "robot.lease_released":
      if (r.lease && r.lease.lease_id !== event.data.lease_id) return undefined;
      delete r.lease;
      r.state = "AUTONOMOUS";
      return r;
    case "robot.lease_revoked":
      if (r.lease && r.lease.lease_id !== event.data.lease_id) return undefined;
      delete r.lease;
      // Expiry and operator loss put the robot back in the queue; a steal is
      // followed by lease_granted for the new holder.
      if (event.data.reason !== "stolen") r.state = "HELP_REQUESTED";
      return r;
    default:
      return undefined;
  }
}

/** Robots in display order: online first, then by name. */
export function sortedRobots(state: FleetState): RobotView[] {
  return [...state.robots.values()].sort((a, b) => {
    if (a.presence !== b.presence) return a.presence === "online" ? -1 : 1;
    return displayName(a).localeCompare(displayName(b));
  });
}

export function displayName(r: RobotView): string {
  return r.name || r.robot_id;
}

export function poseOf(r: RobotView): Pose | undefined {
  return r.telemetry?.pose;
}
