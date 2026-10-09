// Presence is snapshot + operator.* events in, people-on-robots out. These
// tests drive fleetReducer with what the server sends and read the selectors.
import { describe, expect, it } from "vitest";
import type { FleetEvent, Lease, OperatorSummary, RobotSummary } from "@fleet-platform/sdk";
import { emptyFleet, fleetReducer, needsResync, type FleetState } from "./model";
import { audienceOf, audienceSummary, audiences, operatorLabel, operatorViews } from "./presence";

const robot = (id: string, extra: Partial<RobotSummary> = {}): RobotSummary => ({
  robot_id: id,
  presence: "online",
  state: "AUTONOMOUS",
  ...extra,
});

const op = (id: string, extra: Partial<OperatorSummary> = {}): OperatorSummary => ({ operator_id: id, online: true, ...extra });

const lease = (robotId: string, operatorId: string, leaseId = `ls_${robotId}_${operatorId}`): Lease => ({
  lease_id: leaseId,
  robot_id: robotId,
  operator_id: operatorId,
  expires_at_ms: 1_755_100_060_000,
});

function snapshot(robots: RobotSummary[], operators?: OperatorSummary[], from: FleetState = emptyFleet): FleetState {
  return fleetReducer(from, { kind: "snapshot", snapshot: operators ? { robots, operators } : { robots } });
}

function apply(state: FleetState, ...events: FleetEvent[]): FleetState {
  return events.reduce((s, event) => fleetReducer(s, { kind: "event", event, atMs: 0 }), state);
}

const operatorEvent = (name: "operator.online" | "operator.offline" | "operator.watching", entry: OperatorSummary): FleetEvent => ({
  event: name,
  robot_id: "",
  operator_id: entry.operator_id,
  data: entry,
});

describe("operators in the fleet state", () => {
  it("takes the operator list from the snapshot", () => {
    const s = snapshot([robot("r1")], [op("o_a", { name: "ada" }), op("o_b", { online: false })]);
    expect([...s.operators.keys()]).toEqual(["o_a", "o_b"]);
    expect(s.operators.get("o_a")).toEqual({ operator_id: "o_a", name: "ada", online: true });
  });

  it("reads a snapshot without operators (older server) as none", () => {
    const s = snapshot([robot("r1")]);
    expect(s.synced).toBe(true);
    expect(s.operators.size).toBe(0);
  });

  it("a later snapshot replaces the list: an operator it no longer lists is gone", () => {
    const first = snapshot([robot("r1")], [op("o_a"), op("o_b")]);
    const second = snapshot([robot("r1")], [op("o_a")], first);
    expect([...second.operators.keys()]).toEqual(["o_a"]);
  });

  it("adds an operator who came online after the snapshot, from the event alone", () => {
    const s = apply(snapshot([robot("r1")], [op("o_a")]), operatorEvent("operator.online", op("o_b", { name: "bo" })));
    expect(s.operators.get("o_b")).toEqual({ operator_id: "o_b", name: "bo", online: true });
    expect(s.robots.size).toBe(1); // an operator event never invents a robot
  });

  it("replaces the whole entry: watching without `watching` clears it", () => {
    let s = snapshot([robot("r1")], [op("o_a", { name: "ada" })]);
    s = apply(s, operatorEvent("operator.watching", op("o_a", { name: "ada", watching: "r1" })));
    expect(s.operators.get("o_a")?.watching).toBe("r1");
    s = apply(s, operatorEvent("operator.watching", op("o_a", { name: "ada" })));
    expect(s.operators.get("o_a")).toEqual({ operator_id: "o_a", name: "ada", online: true });
  });

  it("offline carries the entry with online false and no watching", () => {
    let s = snapshot([robot("r1")], [op("o_a", { watching: "r1" })]);
    s = apply(s, operatorEvent("operator.offline", op("o_a", { online: false })));
    expect(s.operators.get("o_a")).toEqual({ operator_id: "o_a", online: false });
    expect(audienceOf(audiences(s, undefined), "r1").watchers).toEqual([]);
  });

  it("ignores an operator event whose entry is not an operator", () => {
    const before = snapshot([robot("r1")], [op("o_a")]);
    const bad = { event: "operator.online", robot_id: "", operator_id: "o_x", data: {} } as unknown as FleetEvent;
    expect(apply(before, bad)).toBe(before);
  });

  it("leaves robots alone on operator events and operators alone on robot events", () => {
    const before = snapshot([robot("r1")], [op("o_a")]);
    const afterOp = apply(before, operatorEvent("operator.watching", op("o_a", { watching: "r1" })));
    expect(afterOp.robots).toBe(before.robots);
    const afterRobot = apply(afterOp, { event: "robot.offline", robot_id: "r1", data: undefined });
    expect(afterRobot.operators).toBe(afterOp.operators);
  });
});

describe("needsResync", () => {
  const known = new Set(["r1"]);

  it("asks for a snapshot when an unknown robot appears or a known one comes back online", () => {
    expect(needsResync({ event: "robot.telemetry", robot_id: "r9", data: {} }, known)).toBe(true);
    expect(needsResync({ event: "robot.online", robot_id: "r1", data: undefined }, known)).toBe(true);
    expect(needsResync({ event: "robot.telemetry", robot_id: "r1", data: {} }, known)).toBe(false);
  });

  it("never does for an operator event, whose robot_id is empty", () => {
    for (const name of ["operator.online", "operator.offline", "operator.watching"] as const) {
      expect(needsResync(operatorEvent(name, op("o_new", { watching: "r1" })), known)).toBe(false);
    }
  });
});

describe("operatorLabel", () => {
  const s = snapshot([], [op("o_a", { name: "ada" }), op("o_b"), op("o_c", { name: "" })]);
  it("uses the name where there is one and the id otherwise", () => {
    expect(operatorLabel(s.operators, "o_a")).toBe("ada");
    expect(operatorLabel(s.operators, "o_b")).toBe("o_b");
    expect(operatorLabel(s.operators, "o_c")).toBe("o_c");
    expect(operatorLabel(s.operators, "o_unknown")).toBe("o_unknown");
  });
});

describe("operatorViews (the presence bar)", () => {
  it("puts this console first, then online before offline, then by name", () => {
    const s = snapshot(
      [],
      [op("o_1", { name: "zed" }), op("o_2", { name: "bo", online: false }), op("o_3", { name: "ada" }), op("o_me", { name: "me" })],
    );
    expect(operatorViews(s, "o_me").map((o) => o.label)).toEqual(["me", "ada", "zed", "bo"]);
    expect(operatorViews(s, "o_me")[0]).toMatchObject({ self: true, online: true });
  });

  it("joins watching to a robot and driving to the leases", () => {
    const s = snapshot(
      [robot("r1", { name: "alpha" }), robot("r2", { name: "beta", state: "TELEOP", lease: lease("r2", "o_a") })],
      [op("o_a", { watching: "r2" }), op("o_b", { watching: "r1" })],
    );
    const [a, b] = operatorViews(s, undefined);
    expect(a!.driving.map((r) => r.robot_id)).toEqual(["r2"]);
    expect(a!.watching?.robot_id).toBe("r2");
    expect(b!.driving).toEqual([]);
    expect(b!.watching?.robot_id).toBe("r1");
  });

  it("treats a watching id that is not in the robot list as watching none", () => {
    const s = snapshot([robot("r1")], [op("o_a", { watching: "r_gone" })]);
    expect(operatorViews(s, undefined)[0]!.watching).toBeUndefined();
    expect(audiences(s, undefined).size).toBe(0);
  });

  it("follows a steal: the lease moves from one operator's driving to the other's", () => {
    const first = lease("r1", "o_a", "ls_1");
    const second = lease("r1", "o_b", "ls_2");
    let s = snapshot([robot("r1", { state: "TELEOP", lease: first })], [op("o_a", { watching: "r1" }), op("o_b", { watching: "r1" })]);
    s = apply(
      s,
      { event: "robot.lease_revoked", robot_id: "r1", data: { lease_id: "ls_1", robot_id: "r1", reason: "stolen" } },
      { event: "robot.lease_granted", robot_id: "r1", data: second },
    );
    const views = operatorViews(s, "o_a");
    expect(views.find((o) => o.operator_id === "o_a")!.driving).toEqual([]);
    expect(views.find((o) => o.operator_id === "o_b")!.driving.map((r) => r.robot_id)).toEqual(["r1"]);
    const a = audienceOf(audiences(s, "o_a"), "r1");
    expect(a.driver).toEqual({ operator_id: "o_b", label: "o_b", self: false });
    expect(a.watchers).toEqual([{ operator_id: "o_a", label: "o_a", self: true }]);
  });
});

describe("audiences (who is on each robot)", () => {
  const s = snapshot(
    [robot("r1", { state: "TELEOP", lease: lease("r1", "o_a") }), robot("r2"), robot("r3")],
    [
      op("o_a", { name: "ada", watching: "r1" }),
      op("o_b", { name: "bo", watching: "r1" }),
      op("o_c", { name: "cy", watching: "r2" }),
      op("o_d", { name: "di", watching: "r1" }),
      op("o_e", { name: "ed", online: false }),
    ],
  );
  const all = audiences(s, "o_b");

  it("names the driver from the lease and lists the other watchers by name", () => {
    const a = audienceOf(all, "r1");
    expect(a.driver).toEqual({ operator_id: "o_a", label: "ada", self: false });
    expect(a.watchers.map((w) => w.label)).toEqual(["bo", "di"]); // the driver is not also a watcher
    expect(a.watchers[0]!.self).toBe(true);
  });

  it("has watchers without a driver, and nobody on a robot nobody opened", () => {
    expect(audienceOf(all, "r2")).toEqual({ watchers: [{ operator_id: "o_c", label: "cy", self: false }] });
    expect(audienceOf(all, "r3")).toEqual({ watchers: [] });
  });

  it("names a driver the operator list does not have by id", () => {
    const lone = snapshot([robot("r1", { state: "TELEOP", lease: lease("r1", "o_zz") })], []);
    expect(audienceOf(audiences(lone, undefined), "r1").driver?.label).toBe("o_zz");
  });

  it("summarises a row, leaving out this console's own watching", () => {
    expect(audienceSummary(audienceOf(all, "r1"))).toBe("ada driving, di watching");
    expect(audienceSummary(audienceOf(audiences(s, "o_a"), "r1"))).toBe("you driving, 2 watching");
    expect(audienceSummary(audienceOf(audiences(s, "o_c"), "r2"))).toBe("");
    expect(audienceSummary(audienceOf(all, "r3"))).toBe("");
  });
});
