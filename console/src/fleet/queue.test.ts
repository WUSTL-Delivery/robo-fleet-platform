// The queue is events in, ordered entries out. These tests drive fleetReducer
// with the snapshots and events the server sends and read the queue back.
import { describe, expect, it } from "vitest";
import type { FleetEvent, HelpDetails, Lease, RobotSummary } from "@fleet-platform/sdk";
import { emptyFleet, fleetReducer, type FleetState } from "./model";
import { contextRows, formatWait, interventionQueue, waitMs } from "./queue";

const T0 = 1_755_100_000_000;

function summary(id: string, extra: Partial<RobotSummary> = {}): RobotSummary {
  return { robot_id: id, presence: "online", state: "AUTONOMOUS", ...extra };
}

function snapshot(state: FleetState, robots: RobotSummary[]): FleetState {
  return fleetReducer(state, { kind: "snapshot", snapshot: { robots } });
}

function apply(state: FleetState, ...events: FleetEvent[]): FleetState {
  return events.reduce((s, event) => fleetReducer(s, { kind: "event", event, atMs: 0 }), state);
}

const help = (reason: string, atMs: number, context?: HelpDetails["context"]): HelpDetails =>
  context ? { reason, context, requested_at_ms: atMs } : { reason, requested_at_ms: atMs };

const helpRequested = (id: string, h: HelpDetails): FleetEvent => ({ event: "robot.help_requested", robot_id: id, data: h });

const lease = (id: string, leaseId: string, operator = "op_a"): Lease => ({
  lease_id: leaseId,
  robot_id: id,
  operator_id: operator,
  expires_at_ms: T0 + 60_000,
});

const granted = (l: Lease): FleetEvent => ({ event: "robot.lease_granted", robot_id: l.robot_id, data: l });

const revoked = (l: Lease, reason: "expired" | "stolen" | "operator_lost", h?: HelpDetails): FleetEvent => ({
  event: "robot.lease_revoked",
  robot_id: l.robot_id,
  data: h ? { lease_id: l.lease_id, robot_id: l.robot_id, reason, help: h } : { lease_id: l.lease_id, robot_id: l.robot_id, reason },
});

const released = (l: Lease): FleetEvent => ({
  event: "robot.lease_released",
  robot_id: l.robot_id,
  data: { lease_id: l.lease_id, robot_id: l.robot_id, reason: "released" },
});

const ids = (s: FleetState) => interventionQueue(s).map((e) => e.robot.robot_id);

const three = () => snapshot(emptyFleet, [summary("r1"), summary("r2"), summary("r3")]);

describe("interventionQueue", () => {
  it("is empty when no robot has asked for help", () => {
    expect(interventionQueue(emptyFleet)).toEqual([]);
    expect(interventionQueue(three())).toEqual([]);
  });

  it("builds from a snapshot, longest wait first", () => {
    const s = snapshot(emptyFleet, [
      summary("r1", { state: "HELP_REQUESTED", help: help("b", T0 + 5000) }),
      summary("r2"),
      summary("r3", { state: "HELP_REQUESTED", help: help("a", T0 + 1000, { attempts: 3 }) }),
      summary("r4", { state: "TELEOP", lease: lease("r4", "ls_4") }),
    ]);
    const q = interventionQueue(s);
    expect(q.map((e) => e.robot.robot_id)).toEqual(["r3", "r1"]);
    expect(q[0]?.help).toEqual({ reason: "a", context: { attempts: 3 }, requested_at_ms: T0 + 1000 });
  });

  it("orders by the server's request time, not by arrival order", () => {
    const s = apply(three(), helpRequested("r2", help("late", T0 + 9000)), helpRequested("r1", help("early", T0 + 2000)));
    expect(ids(s)).toEqual(["r1", "r2"]);
  });

  it("gives two consoles the same order whatever order the robots were listed in", () => {
    const a = summary("ra", { state: "HELP_REQUESTED", help: help("x", T0) });
    const b = summary("rb", { state: "HELP_REQUESTED", help: help("y", T0) });
    expect(ids(snapshot(emptyFleet, [a, b]))).toEqual(["ra", "rb"]);
    expect(ids(snapshot(emptyFleet, [b, a]))).toEqual(["ra", "rb"]);
  });

  it("removes the entry when anyone's claim is granted", () => {
    let s = apply(three(), helpRequested("r1", help("a", T0)), helpRequested("r2", help("b", T0 + 1000)));
    s = apply(s, granted(lease("r1", "ls_1", "op_someone_else")));
    expect(ids(s)).toEqual(["r2"]);
    expect(s.robots.get("r1")?.state).toBe("TELEOP");
    expect(s.robots.get("r1")?.help).toBeUndefined();
  });

  it("puts a robot back at its original place when the lease expires", () => {
    const first = help("stuck", T0, { attempts: 3 });
    const l = lease("r1", "ls_1");
    let s = apply(three(), helpRequested("r1", first), helpRequested("r2", help("b", T0 + 1000)), granted(l));
    expect(ids(s)).toEqual(["r2"]);
    // 40 s later the claimer's lease expires; the server sends the entry it kept.
    s = apply(s, revoked(l, "expired", first));
    const q = interventionQueue(s);
    expect(q.map((e) => e.robot.robot_id)).toEqual(["r1", "r2"]);
    expect(q[0]?.help).toEqual(first);
    expect(waitMs(q[0]!, T0 + 40_000)).toBe(40_000);
  });

  it("requeues after operator loss the same way", () => {
    const first = help("stuck", T0);
    const l = lease("r1", "ls_1");
    const s = apply(three(), helpRequested("r1", first), granted(l), revoked(l, "operator_lost", first));
    expect(interventionQueue(s)[0]?.help?.requested_at_ms).toBe(T0);
  });

  it("keeps a stolen robot out of the queue", () => {
    const a = lease("r1", "ls_a", "op_a");
    const b = lease("r1", "ls_b", "op_b");
    let s = apply(three(), helpRequested("r1", help("a", T0)), granted(a));
    s = apply(s, revoked(a, "stolen"));
    expect(ids(s)).toEqual([]);
    expect(s.robots.get("r1")?.state).toBe("TELEOP");
    s = apply(s, granted(b));
    expect(ids(s)).toEqual([]);
    expect(s.robots.get("r1")?.lease?.operator_id).toBe("op_b");
  });

  it("ignores a revocation of a lease that is no longer the robot's", () => {
    const a = lease("r1", "ls_a", "op_a");
    const b = lease("r1", "ls_b", "op_b");
    // The grant to the new holder arrives before the old lease's revocation.
    const s = apply(three(), helpRequested("r1", help("a", T0)), granted(a), granted(b), revoked(a, "expired", help("a", T0)));
    expect(ids(s)).toEqual([]);
    expect(s.robots.get("r1")?.lease?.lease_id).toBe("ls_b");
  });

  it("closes the entry on handback, and a later request starts a new wait", () => {
    const l = lease("r1", "ls_1");
    let s = apply(three(), helpRequested("r1", help("first", T0)), granted(l), released(l));
    expect(ids(s)).toEqual([]);
    expect(s.robots.get("r1")?.state).toBe("AUTONOMOUS");
    s = apply(s, helpRequested("r2", help("other", T0 + 60_000)), helpRequested("r1", help("second", T0 + 90_000)));
    expect(interventionQueue(s).map((e) => [e.robot.robot_id, e.help?.reason])).toEqual([
      ["r2", "other"],
      ["r1", "second"],
    ]);
  });

  it("keeps a robot that went offline in the queue, with its place and marked offline", () => {
    let s = apply(three(), helpRequested("r1", help("a", T0)), helpRequested("r2", help("b", T0 + 1000)));
    s = apply(s, { event: "robot.offline", robot_id: "r1", data: undefined });
    const q = interventionQueue(s);
    expect(q.map((e) => e.robot.robot_id)).toEqual(["r1", "r2"]);
    expect(q[0]?.robot.presence).toBe("offline");
    s = apply(s, { event: "robot.online", robot_id: "r1", data: undefined });
    expect(interventionQueue(s)[0]?.help?.requested_at_ms).toBe(T0);
  });

  it("takes a requeue it got no event for from the next snapshot", () => {
    // A lease that expires while its robot is offline emits no event.
    const l = lease("r1", "ls_1");
    let s = apply(three(), helpRequested("r1", help("a", T0)), granted(l));
    expect(ids(s)).toEqual([]);
    s = snapshot(s, [summary("r1", { state: "HELP_REQUESTED", help: help("a", T0) }), summary("r2"), summary("r3")]);
    expect(interventionQueue(s)[0]?.help).toEqual(help("a", T0));
    expect(s.robots.get("r1")?.lease).toBeUndefined();
  });

  it("drops entries the snapshot no longer lists as waiting", () => {
    let s = apply(three(), helpRequested("r1", help("a", T0)));
    s = snapshot(s, [summary("r1"), summary("r2"), summary("r3")]);
    expect(ids(s)).toEqual([]);
    expect(s.robots.get("r1")?.help).toBeUndefined();
  });

  it("lists a waiting robot with no usable request time last, with no wait", () => {
    const l = lease("r2", "ls_2");
    let s = apply(three(), granted(l), revoked(l, "expired"));
    s = apply(s, helpRequested("r3", help("c", T0 + 5000)));
    const q = interventionQueue(s);
    expect(q.map((e) => e.robot.robot_id)).toEqual(["r3", "r2"]);
    expect(waitMs(q[1]!, T0 + 10_000)).toBeUndefined();
  });

  it("queues a robot first heard of through its help request", () => {
    const s = apply(three(), helpRequested("r9", help("new", T0)));
    expect(ids(s)).toEqual(["r9"]);
  });
});

describe("waitMs", () => {
  const entry = { robot: { robot_id: "r1", presence: "online", state: "HELP_REQUESTED" } as const, help: help("a", T0) };

  it("is the server time elapsed since the request", () => {
    expect(waitMs(entry, T0 + 65_000)).toBe(65_000);
  });

  it("never goes negative when the estimate trails the server", () => {
    expect(waitMs(entry, T0 - 300)).toBe(0);
  });
});

describe("formatWait", () => {
  it("formats seconds, minutes and hours", () => {
    expect(formatWait(0)).toBe("0s");
    expect(formatWait(999)).toBe("0s");
    expect(formatWait(42_500)).toBe("42s");
    expect(formatWait(60_000)).toBe("1m 00s");
    expect(formatWait(185_000)).toBe("3m 05s");
    expect(formatWait(3_720_000)).toBe("1h 02m");
    expect(formatWait(-5)).toBe("0s");
  });
});

describe("contextRows", () => {
  it("shows keys as sent, scalars as text and nested values as JSON", () => {
    expect(contextRows(undefined)).toEqual([]);
    expect(contextRows({ attempts: 3, note: "blocked", ok: false, at: { x: 1 }, none: null })).toEqual([
      { key: "attempts", value: "3" },
      { key: "note", value: "blocked" },
      { key: "ok", value: "false" },
      { key: "at", value: '{"x":1}' },
      { key: "none", value: "null" },
    ]);
  });
});
