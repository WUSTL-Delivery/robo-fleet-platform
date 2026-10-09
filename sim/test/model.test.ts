// Pure motion model and the robot-side lease gate + deadman, with an injected clock.
import { describe, expect, it } from "vitest";
import { fromGeo, integrate, normalizeAngle, realize, spread, toGeo } from "../src/kinematics.js";
import { readFileSync } from "node:fs";
import { parseTwist } from "../src/p2p.js";
import { DEADMAN_MS, TeleopGate } from "../src/teleop.js";

const LIM = { maxV: 1.5, maxW: 2 };

describe("kinematics", () => {
  it("diff-drive ignores linear.y; omni honors it", () => {
    expect(realize({ vx: 0.5, vy: 0.4, wz: 0 }, "diff", LIM)).toEqual({ vx: 0.5, vy: 0, wz: 0 });
    expect(realize({ vx: 0.5, vy: 0.4, wz: 0 }, "omni", LIM)).toEqual({ vx: 0.5, vy: 0.4, wz: 0 });
  });

  it("clamps speed and yaw rate to the declared limits", () => {
    const v = realize({ vx: 3, vy: 4, wz: -9 }, "omni", LIM);
    expect(Math.hypot(v.vx, v.vy)).toBeCloseTo(1.5);
    expect(v.vx / v.vy).toBeCloseTo(3 / 4);
    expect(v.wz).toBe(-2);
  });

  it("integrates in the body frame: forward follows yaw, strafe is to the left", () => {
    const north = integrate({ x: 0, y: 0, yaw: Math.PI / 2 }, { vx: 1, vy: 0, wz: 0 }, 2);
    expect(north.x).toBeCloseTo(0);
    expect(north.y).toBeCloseTo(2);
    const left = integrate({ x: 0, y: 0, yaw: 0 }, { vx: 0, vy: 1, wz: 0 }, 1);
    expect(left.x).toBeCloseTo(0);
    expect(left.y).toBeCloseTo(1);
    // A full circle at constant twist comes back to the start.
    let p = { x: 0, y: 0, yaw: 0 };
    for (let i = 0; i < 1000; i++) p = integrate(p, { vx: 1, vy: 0, wz: 1 }, (2 * Math.PI) / 1000);
    expect(Math.hypot(p.x, p.y)).toBeLessThan(1e-3);
  });

  it("wraps angles to (-pi, pi]", () => {
    expect(normalizeAngle(3 * Math.PI)).toBeCloseTo(Math.PI);
    expect(normalizeAngle(-Math.PI / 2 - 2 * Math.PI)).toBeCloseTo(-Math.PI / 2);
  });

  it("round-trips local metres through lat/lon", () => {
    const origin = { lat: 38.6488, lon: -90.3108 };
    const g = toGeo(origin, 100, -50);
    expect(g.lat).toBeLessThan(origin.lat);
    expect(g.lon).toBeGreaterThan(origin.lon);
    const back = fromGeo(origin, g);
    expect(back.x).toBeCloseTo(100, 6);
    expect(back.y).toBeCloseTo(-50, 6);
  });

  it("spreads start positions deterministically inside the radius", () => {
    const a = spread(20, 100);
    expect(a).toEqual(spread(20, 100));
    expect(a).toHaveLength(20);
    for (const p of a) expect(Math.hypot(p.x, p.y)).toBeLessThanOrEqual(100);
    const distinct = new Set(a.map((p) => `${p.x.toFixed(2)},${p.y.toFixed(2)}`));
    expect(distinct.size).toBe(20);
  });
});

describe("TeleopGate", () => {
  const fwd = { vx: 1, vy: 0, wz: 0 };

  it("obeys twist only under the current lease", () => {
    const g = new TeleopGate();
    expect(g.accept("ls_a", fwd, 0)).toBe(false); // no lease yet
    g.grant("ls_a");
    expect(g.accept("ls_b", fwd, 0)).toBe(false); // wrong lease
    expect(g.current(0)).toEqual({ vx: 0, vy: 0, wz: 0 });
    expect(g.accept("ls_a", fwd, 0)).toBe(true);
    expect(g.current(10)).toEqual(fwd);
  });

  it("zeroes velocity once the deadman lapses, and resumes on the next valid twist", () => {
    const g = new TeleopGate();
    g.grant("ls_a");
    g.accept("ls_a", fwd, 1000);
    expect(g.current(1000 + DEADMAN_MS)).toEqual(fwd);
    expect(g.current(1000 + DEADMAN_MS + 1)).toEqual({ vx: 0, vy: 0, wz: 0 });
    g.accept("ls_a", fwd, 2000);
    expect(g.current(2100)).toEqual(fwd);
  });

  it("stops at once on revoke of the held lease, ignores revokes of other leases", () => {
    const g = new TeleopGate();
    g.grant("ls_a");
    g.accept("ls_a", fwd, 0);
    expect(g.revoke("ls_old")).toBe(false);
    expect(g.current(1)).toEqual(fwd);
    expect(g.revoke("ls_a")).toBe(true);
    expect(g.leaseId).toBeUndefined();
    expect(g.current(1)).toEqual({ vx: 0, vy: 0, wz: 0 });
    expect(g.accept("ls_a", fwd, 2)).toBe(false);
  });

  it("a steal re-grants: the old lease's twists are refused, the new one's obeyed", () => {
    const g = new TeleopGate();
    g.grant("ls_a");
    g.accept("ls_a", fwd, 0);
    g.grant("ls_b");
    expect(g.current(1)).toEqual({ vx: 0, vy: 0, wz: 0 }); // re-grant halts
    expect(g.accept("ls_a", fwd, 2)).toBe(false);
    expect(g.accept("ls_b", fwd, 2)).toBe(true);
  });

  it("halt() stops but keeps the lease (link blip)", () => {
    const g = new TeleopGate();
    g.grant("ls_a");
    g.accept("ls_a", fwd, 0);
    g.halt();
    expect(g.current(1)).toEqual({ vx: 0, vy: 0, wz: 0 });
    expect(g.leaseId).toBe("ls_a");
    expect(g.accept("ls_a", fwd, 2)).toBe(true);
  });

  it("confirm() leaves the gate holding exactly the lease the welcome states", () => {
    const g = new TeleopGate();
    // Nothing held, nothing stated.
    expect(g.confirm(undefined)).toBe("none");
    expect(g.leaseId).toBeUndefined();

    // The link blipped and the server still holds the lease: driven again.
    g.grant("ls_a");
    g.accept("ls_a", fwd, 0);
    g.halt();
    expect(g.confirm("ls_a")).toBe("kept");
    expect(g.current(1)).toEqual({ vx: 0, vy: 0, wz: 0 }); // the old setpoint did not come back
    expect(g.accept("ls_a", fwd, 2)).toBe(true);

    // The lease ended while the robot was away (or the server did not say): gone, stopped.
    g.halt();
    expect(g.confirm(undefined)).toBe("dropped");
    expect(g.leaseId).toBeUndefined();
    expect(g.accept("ls_a", fwd, 3)).toBe(false);
    expect(g.accept("ls_a", fwd, 3, "p2p")).toBe(false);
    expect(g.current(4)).toEqual({ vx: 0, vy: 0, wz: 0 });

    // The server names a lease the robot did not hold: taken as a grant, the old one refused.
    g.grant("ls_b");
    g.accept("ls_b", fwd, 5);
    expect(g.confirm("ls_c")).toBe("granted");
    expect(g.current(6)).toEqual({ vx: 0, vy: 0, wz: 0 });
    expect(g.accept("ls_b", fwd, 7)).toBe(false);
    expect(g.accept("ls_c", fwd, 7)).toBe(true);
  });

  it("ignores a bus twist while a data-channel twist is fresh, and takes it once the window has passed", () => {
    const g = new TeleopGate();
    const back = { vx: -1, vy: 0, wz: 0 };
    g.grant("ls_a");
    expect(g.accept("ls_a", fwd, 1000, "p2p")).toBe(true);
    expect(g.accept("ls_a", back, 1000 + DEADMAN_MS, "bus")).toBe(false); // a straggler from before the switch
    expect(g.current(1000 + DEADMAN_MS)).toEqual(fwd);
    expect(g.accept("ls_a", back, 1000 + DEADMAN_MS + 1, "bus")).toBe(true); // the fallback takes over
    expect(g.current(1000 + DEADMAN_MS + 2)).toEqual(back);
    // The data channel is never shadowed by the bus.
    expect(g.accept("ls_a", fwd, 1000 + DEADMAN_MS + 3, "p2p")).toBe(true);
    // A wrong-lease twist on the channel does not start the window.
    g.grant("ls_b");
    expect(g.accept("ls_a", fwd, 5000, "p2p")).toBe(false);
    expect(g.accept("ls_b", back, 5001, "bus")).toBe(true);
  });
});

describe("data-channel twist", () => {
  const fixture = (name: string) =>
    JSON.parse(readFileSync(new URL(`../../protocol/fixtures/datachannel/${name}.json`, import.meta.url), "utf8")) as Record<string, unknown>;

  it("reads the golden fixture as the bus twist payload plus seq", () => {
    expect(parseTwist(fixture("twist"))).toEqual({
      seq: 42,
      payload: { lease_id: "ls_7f8e9d0c", linear: { x_mps: 0.5 }, angular: { z_radps: -0.3 } },
    });
  });

  it("refuses anything that is not a whole twist", () => {
    const good = fixture("twist");
    expect(parseTwist(fixture("ping"))).toBeUndefined();
    expect(parseTwist({ ...good, seq: undefined })).toBeUndefined();
    expect(parseTwist({ ...good, seq: 0 })).toBeUndefined();
    expect(parseTwist({ ...good, seq: 1.5 })).toBeUndefined();
    expect(parseTwist({ ...good, lease_id: "" })).toBeUndefined();
    expect(parseTwist({ ...good, linear: { x_mps: "0.5" } })).toBeUndefined();
    expect(parseTwist({ ...good, linear: { x_mps: 0.5, y_mps: null } })).toBeUndefined();
    expect(parseTwist({ ...good, angular: {} })).toBeUndefined();
    expect(parseTwist({ ...good, linear: { x_mps: 0.1, y_mps: 0.2 } })?.payload.linear).toEqual({ x_mps: 0.1, y_mps: 0.2 });
  });
});
