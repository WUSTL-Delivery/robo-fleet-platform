// Who has the wheel as one console sees it, and the notices for losing it.
import { describe, expect, it } from "vitest";
import type { Lease } from "@fleet-platform/sdk";
import type { RobotView } from "../fleet/model";
import { claimRefusedNotice, controlOf, noticeText, revokedNotice, withTaker } from "./control";

const lease = (operatorId: string, leaseId = "ls_1"): Lease => ({
  lease_id: leaseId,
  robot_id: "r1",
  operator_id: operatorId,
  expires_at_ms: 1_755_100_060_000,
});

const robot = (extra: Partial<RobotView> = {}): RobotView => ({ robot_id: "r1", presence: "online", state: "AUTONOMOUS", ...extra });

const names: Record<string, string> = { o_a: "ada", o_b: "bo" };
const nameOf = (id: string) => names[id] ?? id;

describe("controlOf", () => {
  it("is free when nobody drives an online robot, offline otherwise", () => {
    expect(controlOf(robot(), "o_a", "idle")).toEqual({ mode: "free" });
    expect(controlOf(robot({ presence: "offline" }), "o_a", "idle")).toEqual({ mode: "offline" });
  });

  it("is spectating, with the driver, when another operator holds the lease", () => {
    expect(controlOf(robot({ state: "TELEOP", lease: lease("o_b") }), "o_a", "idle")).toEqual({ mode: "spectating", driverId: "o_b" });
  });

  it("stays spectating when that robot goes offline: the lease is still theirs", () => {
    expect(controlOf(robot({ presence: "offline", state: "TELEOP", lease: lease("o_b") }), "o_a", "idle").mode).toBe("spectating");
  });

  it("is driving only on this console's own phase, never on the fleet state alone", () => {
    const mine = robot({ state: "TELEOP", lease: lease("o_a") });
    expect(controlOf(mine, "o_a", "driving")).toEqual({ mode: "driving" });
    // Our operator's lease, but not held by this console (another tab, a reload).
    expect(controlOf(mine, "o_a", "idle")).toEqual({ mode: "held" });
  });

  it("does not offer back the lease this console just lost while the fleet state still lists it", () => {
    const stale = robot({ state: "TELEOP", lease: lease("o_a", "ls_1") });
    expect(controlOf(stale, "o_a", "idle", "ls_1")).toEqual({ mode: "settling" });
    // A newer lease of ours (another tab claimed) is a real one to resume.
    expect(controlOf(robot({ state: "TELEOP", lease: lease("o_a", "ls_2") }), "o_a", "idle", "ls_1")).toEqual({ mode: "held" });
    expect(controlOf(robot(), "o_a", "idle", "ls_1")).toEqual({ mode: "free" });
  });

  it("drops to spectating the moment this console's phase goes idle under another's lease (a steal)", () => {
    const stolen = robot({ state: "TELEOP", lease: lease("o_b", "ls_2") });
    expect(controlOf(stolen, "o_a", "idle")).toEqual({ mode: "spectating", driverId: "o_b" });
  });

  it("keeps the driver in view while a steal from them is out", () => {
    expect(controlOf(robot({ state: "TELEOP", lease: lease("o_b") }), "o_a", "claiming")).toEqual({ mode: "claiming", driverId: "o_b" });
    expect(controlOf(robot(), "o_a", "claiming")).toEqual({ mode: "claiming" });
  });

  it("treats every lease as another's before the console knows its own id", () => {
    expect(controlOf(robot({ state: "TELEOP", lease: lease("o_a") }), undefined, "idle").mode).toBe("spectating");
  });
});

describe("revokedNotice", () => {
  it("is a steal for reason stolen, and plain text for the rest", () => {
    expect(revokedNotice("stolen")).toEqual({ kind: "stolen" });
    expect(revokedNotice("expired")?.kind).toBe("text");
    expect(revokedNotice("operator_lost")?.kind).toBe("text");
    expect(revokedNotice("released")).toEqual({ kind: "text", text: "Control handed back." });
  });
});

describe("withTaker", () => {
  it("names the first other holder the fleet state shows after a steal", () => {
    expect(withTaker({ kind: "stolen" }, lease("o_b", "ls_2"), "o_a")).toEqual({ kind: "stolen", by: "o_b" });
  });

  it("waits while the fleet state still shows our own lease, or none", () => {
    const n = { kind: "stolen" } as const;
    expect(withTaker(n, lease("o_a"), "o_a")).toBe(n);
    expect(withTaker(n, undefined, "o_a")).toBe(n);
  });

  it("does not rewrite who took it when the robot later changes hands again", () => {
    const n = { kind: "stolen", by: "o_b" } as const;
    expect(withTaker(n, lease("o_c", "ls_3"), "o_a")).toBe(n);
  });

  it("leaves other notices and no notice alone", () => {
    const text = { kind: "text", text: "x" } as const;
    expect(withTaker(text, lease("o_b"), "o_a")).toBe(text);
    expect(withTaker(undefined, lease("o_b"), "o_a")).toBeUndefined();
  });
});

describe("claimRefusedNotice", () => {
  it("names who holds the robot from the lease on a conflict", () => {
    const n = claimRefusedNotice({ code: "conflict", message: "robot is leased to another operator", lease: lease("o_b") });
    expect(n).toEqual({ kind: "beaten", by: "o_b" });
    expect(noticeText(n, nameOf)).toBe("bo claimed it first.");
  });

  it("falls back to the server's message for any other refusal", () => {
    expect(claimRefusedNotice({ code: "not_found", message: "no such robot" })).toEqual({
      kind: "text",
      text: "Take over refused: no such robot.",
    });
    expect(claimRefusedNotice({ code: "conflict", message: "busy" }).kind).toBe("text");
  });
});

describe("noticeText", () => {
  it("names who took control, by name, then by id, then not at all", () => {
    expect(noticeText({ kind: "stolen", by: "o_b" }, nameOf)).toBe("bo took control from you.");
    expect(noticeText({ kind: "stolen", by: "o_zz" }, nameOf)).toBe("o_zz took control from you.");
    expect(noticeText({ kind: "stolen" }, nameOf)).toBe("Another operator took control from you.");
  });
});
