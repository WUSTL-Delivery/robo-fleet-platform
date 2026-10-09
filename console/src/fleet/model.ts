// The console's picture of the fleet, rebuilt from the server's stream.
//
// State is a pure reducer over two inputs: a `snapshot` (on every subscribe,
// including after each reconnect) and the `event` envelopes that follow it.
// The snapshot is authoritative for membership, presence, FSM state, manifest
// lease and the open help request; it carries no telemetry, so the last telemetry is kept across a
// snapshot for robots that are still online and dropped for the rest.
//
// Operators live beside the robots: the snapshot lists every one, and each
// operator.* event carries that operator's whole entry, which replaces the
// stored one. Who is watching or driving what is derived in presence.ts.
import type {
  FleetEvent,
  HelpDetails,
  Lease,
  Manifest,
  OperatorEvent,
  OperatorSummary,
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
  /**
   * The open help request, while the robot is HELP_REQUESTED: what it asked
   * for and when it entered the queue (server clock). See fleet/queue.ts.
   */
  help?: HelpDetails;
  /** Latest telemetry payload, verbatim. */
  telemetry?: TelemetryPayload;
  /** Local receive time of `telemetry` (ms since epoch). */
  telemetryAtMs?: number;
}

export interface FleetState {
  /** False until the first snapshot arrives. */
  synced: boolean;
  robots: ReadonlyMap<string, RobotView>;
  /** Every operator of the fleet the server has told us about, by operator id. */
  operators: ReadonlyMap<string, OperatorSummary>;
}

export const emptyFleet: FleetState = { synced: false, robots: new Map(), operators: new Map() };

export type FleetAction =
  | { kind: "reset" }
  | { kind: "snapshot"; snapshot: SnapshotPayload }
  | { kind: "event"; event: FleetEvent; atMs: number };

export function fleetReducer(prev: FleetState, action: FleetAction): FleetState {
  if (action.kind === "reset") return emptyFleet;
  if (action.kind === "snapshot") return applySnapshot(prev, action.snapshot);
  if (isOperatorEvent(action.event)) return applyOperatorEvent(prev, action.event);
  // A robot event always names its robot; one that does not is not ours to guess at.
  if (!action.event.robot_id) return prev;
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
  const operators = new Map<string, OperatorSummary>();
  // Optional on the wire so that a snapshot from an older server still reads.
  for (const o of snapshot.operators ?? []) {
    if (isOperator(o)) operators.set(o.operator_id, o);
  }
  return { synced: true, robots, operators };
}

/** operator.* events are about an operator: they carry `operator_id` and no robot. */
export function isOperatorEvent(event: FleetEvent): event is OperatorEvent {
  return event.event.startsWith("operator.");
}

/**
 * Events carry no robot name or manifest. A robot the console has not seen,
 * or one coming back online (it re-sends its manifest on connect), is worth a
 * fresh snapshot. Operator events never are: each carries the whole entry.
 */
export function needsResync(event: FleetEvent, knownRobots: ReadonlySet<string>): boolean {
  if (isOperatorEvent(event) || !event.robot_id) return false;
  return event.event === "robot.online" || !knownRobots.has(event.robot_id);
}

/**
 * The entry in `data` is the operator as of the event, whole: it replaces the
 * stored one, so an entry without `watching` clears what was there.
 */
function applyOperatorEvent(prev: FleetState, event: OperatorEvent): FleetState {
  const entry = event.data;
  if (!isOperator(entry)) return prev;
  const operators = new Map(prev.operators);
  operators.set(entry.operator_id, entry);
  return { ...prev, operators };
}

function isOperator(o: unknown): o is OperatorSummary {
  if (typeof o !== "object" || o === null) return false;
  const e = o as Partial<OperatorSummary>;
  return typeof e.operator_id === "string" && e.operator_id !== "" && typeof e.online === "boolean";
}

function fromSummary(s: RobotSummary): RobotView {
  const v: RobotView = { robot_id: s.robot_id, presence: s.presence, state: s.state };
  if (s.name !== undefined) v.name = s.name;
  if (s.manifest !== undefined) v.manifest = s.manifest;
  if (s.lease !== undefined) v.lease = s.lease;
  if (s.state === "HELP_REQUESTED" && isHelp(s.help)) v.help = s.help;
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
      if (isHelp(event.data)) r.help = event.data;
      else delete r.help;
      return r;
    case "robot.lease_granted":
      // A claim closes the queue entry for everyone watching.
      r.state = "TELEOP";
      r.lease = event.data;
      delete r.help;
      return r;
    case "robot.lease_released":
      if (r.lease && r.lease.lease_id !== event.data.lease_id) return undefined;
      delete r.lease;
      delete r.help;
      r.state = "AUTONOMOUS";
      return r;
    case "robot.lease_revoked":
      if (r.lease && r.lease.lease_id !== event.data.lease_id) return undefined;
      delete r.lease;
      // A steal leaves the robot in TELEOP and is followed by lease_granted
      // for the new holder. Expiry and operator loss put it back in the queue
      // with the entry the server kept (original reason and request time).
      if (event.data.reason === "stolen") return r;
      r.state = "HELP_REQUESTED";
      if (isHelp(event.data.help)) r.help = event.data.help;
      else delete r.help;
      return r;
    default:
      return undefined;
  }
}

/** The wire is typed, but a queue ordered by a missing time would be wrong for everyone. */
function isHelp(h: unknown): h is HelpDetails {
  if (typeof h !== "object" || h === null) return false;
  const d = h as Partial<HelpDetails>;
  return typeof d.reason === "string" && typeof d.requested_at_ms === "number" && Number.isFinite(d.requested_at_ms);
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
