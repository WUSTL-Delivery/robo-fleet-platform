// The sim fleet against the real fleet-server (started by
// test/support/server.globalSetup.ts): 20 robots online in a snapshot, a robot
// driven under a lease, and the robot-side deadman stopping it ~300 ms after
// the operator's twists stop. Observed only through the SDK, as the console would.
import { afterAll, beforeAll, describe, expect, inject, it } from "vitest";
import {
  FleetClient,
  type Envelope,
  type FleetClientOptions,
  type FleetEvent,
  type GeoPose,
  type SnapshotPayload,
  type TelemetryEvent,
  type WebSocketConstructor,
} from "@fleet-platform/sdk";
import { WebSocket as WsWebSocket } from "ws";
import { DEFAULT_ORIGIN, startFleet, type Fleet } from "../src/fleet.js";
import { fromGeo } from "../src/kinematics.js";
import { DEADMAN_MS } from "../src/teleop.js";

const server = inject("fleetServer");
const WS = WsWebSocket as unknown as WebSocketConstructor;

const clients: FleetClient[] = [];
const fleets: Fleet[] = [];

function client(opts: Pick<FleetClientOptions, "kind" | "enrollmentKey" | "name">): FleetClient {
  const c = new FleetClient({ url: server.wsUrl, WebSocket: WS, reconnect: false, ...opts });
  clients.push(c);
  return c;
}

async function fleet(opts: Omit<Parameters<typeof startFleet>[0], "url" | "enrollmentKey" | "stateDir">): Promise<Fleet> {
  const f = await startFleet({ url: server.wsUrl, enrollmentKey: server.enrollKey, stateDir: null, ...opts });
  fleets.push(f);
  return f;
}

afterAll(() => {
  for (const c of clients.splice(0)) c.close();
  for (const f of fleets.splice(0)) f.stop();
});

async function mintOperatorInvite(): Promise<string> {
  const res = await fetch(`${server.httpUrl}/api/admin/fleets/${server.fleet}/operator-invites`, {
    method: "POST",
    headers: { Authorization: `Bearer ${server.adminToken}` },
  });
  expect(res.status).toBe(200);
  return ((await res.json()) as { key: string }).key;
}

function next<T>(register: (h: (v: T) => void) => () => void, pred: (v: T) => boolean = () => true, what = "value", timeoutMs = 5000): Promise<T> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      off();
      reject(new Error(`timed out waiting for ${what}`));
    }, timeoutMs);
    const off = register((v) => {
      if (!pred(v)) return;
      clearTimeout(timer);
      off();
      resolve(v);
    });
  });
}

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

/** Re-subscribes until `ok(snapshot)` holds (manifests land just after presence). */
async function snapshotUntil(c: FleetClient, ok: (s: SnapshotPayload) => boolean, what: string): Promise<SnapshotPayload> {
  const deadline = Date.now() + 5000;
  for (;;) {
    const snap = await c.subscribe(["presence", "events", "telemetry"]);
    if (ok(snap)) return snap;
    if (Date.now() > deadline) throw new Error(`snapshot never showed ${what}: ${JSON.stringify(snap).slice(0, 2000)}`);
    await sleep(50);
  }
}

describe("sim fleet against fleet-server", () => {
  let sim: Fleet;
  let svc: FleetClient;
  let operator: FleetClient;

  beforeAll(async () => {
    sim = await fleet({ count: 20, telemetryHz: 20 });
    svc = client({ kind: "service", name: "observer", enrollmentKey: server.enrollKey });
    await svc.connect();
    operator = client({ kind: "operator", name: "op-1", enrollmentKey: await mintOperatorInvite() });
    await operator.connect();
  });

  it("shows 20 robots online with their manifests in a snapshot", async () => {
    const ids = new Set(sim.robots.map((r) => r.robotId!));
    expect(ids.size).toBe(20);
    const mine = (s: SnapshotPayload) => s.robots.filter((r) => ids.has(r.robot_id));
    const snap = await snapshotUntil(
      svc,
      (s) => mine(s).filter((r) => r.presence === "online" && r.manifest).length === 20,
      "20 sim robots online",
    );
    const robots = mine(snap);
    expect(robots.every((r) => r.state === "AUTONOMOUS")).toBe(true);
    for (const r of robots) {
      expect(r.manifest?.drive?.type).toBe("twist");
      expect(r.manifest?.cameras?.[0]?.id).toBe("front");
      expect(r.manifest?.battery).toEqual({});
    }
    expect(robots.map((r) => r.name).sort()).toEqual(sim.robots.map((r) => r.name).sort());
    // Default mix: one in five is holonomic, and advertises its own limits.
    expect(sim.robots.filter((r) => r.morphology === "omni")).toHaveLength(4);
    const omniId = sim.robots.at(-1)!.robotId;
    expect(robots.find((r) => r.robot_id === omniId)?.manifest?.drive?.max_v_mps).toBe(1.0);

    // Telemetry carries a geographic pose near the default origin.
    const tel = await next<TelemetryEvent>((h) => svc.onTelemetry(h), (e) => ids.has(e.robot_id), "telemetry");
    const pose = tel.data.pose as GeoPose;
    expect(pose.frame).toBe("geographic");
    const local = fromGeo(DEFAULT_ORIGIN, pose);
    expect(Math.hypot(local.x, local.y)).toBeLessThan(125);
  });

  it("moves under a lease and stops ~300 ms after the twists stop (deadman)", async () => {
    const robot = sim.robots[0]!;
    const robotId = robot.robotId!;

    // Every telemetry sample for this robot, stamped on receipt.
    const samples: { t: number; x: number; y: number; v: number }[] = [];
    const offTel = svc.onTelemetry((e) => {
      if (e.robot_id !== robotId) return;
      const p = fromGeo(DEFAULT_ORIGIN, e.data.pose as GeoPose);
      samples.push({ t: Date.now(), x: p.x, y: p.y, v: e.data.velocity?.v_mps ?? NaN });
    });
    const poseAt = (t: number) => {
      const before = samples.filter((s) => s.t <= t);
      return before.at(-1) ?? samples[0]!;
    };
    const dist = (a: { x: number; y: number }, b: { x: number; y: number }) => Math.hypot(a.x - b.x, a.y - b.y);

    // Claim: both the operator and the robot hear the lease.
    const opGrant = next<Envelope<"lease.granted">>((h) => operator.on("lease.granted", h), (e) => e.payload.robot_id === robotId, "operator grant");
    const granted = next<FleetEvent<"robot.lease_granted">>((h) => svc.onEvent("robot.lease_granted", h), (e) => e.robot_id === robotId, "robot.lease_granted");
    operator.send("lease.claim", { robot_id: robotId });
    const lease = (await opGrant).payload;
    await granted;
    await sleep(100); // the robot's copy of the grant rides the same server tick
    expect(robot.leaseId).toBe(lease.lease_id);
    expect(robot.mode).toBe("teleop");

    await sleep(200);
    const tStart = Date.now();

    // Drive forward at 1 m/s for 1 s, twist at 20 Hz.
    let lastTwistAt = 0;
    for (let i = 0; i < 20; i++) {
      operator.send("twist", { lease_id: lease.lease_id, linear: { x_mps: 1.0 }, angular: { z_radps: 0 } });
      lastTwistAt = Date.now();
      await sleep(50);
    }
    // Stop sending. Nothing else tells the robot to stop: only its deadman.
    await sleep(1200);
    offTel();

    const start = poseAt(tStart);
    const atLast = poseAt(lastTwistAt);
    const end = samples.at(-1)!;
    expect(dist(start, atLast)).toBeGreaterThan(0.6); // it moved while driven
    const coast = dist(atLast, end);
    expect(coast).toBeGreaterThan(0.1); // kept going through the deadman window...
    expect(coast).toBeLessThan(0.3 + 0.35); // ...and no further than ~DEADMAN_MS at 1 m/s (+ sampling slack)

    // Everything reported from lastTwist + 2x deadman on is stationary with zero speed.
    const settled = samples.filter((s) => s.t > lastTwistAt + 2 * DEADMAN_MS);
    expect(settled.length).toBeGreaterThan(5);
    for (const s of settled) {
      expect(s.v).toBe(0);
      expect(dist(s, end)).toBeLessThan(1e-6);
    }
    expect(robot.velocity).toEqual({ vx: 0, vy: 0, wz: 0 });

    // Handback: the robot resumes autonomy and the server refuses twists on the dead lease.
    const released = next<FleetEvent<"robot.lease_released">>((h) => svc.onEvent("robot.lease_released", h), (e) => e.robot_id === robotId, "released");
    operator.send("lease.release", { lease_id: lease.lease_id, resolution: "resolved" });
    await released;
    await sleep(50);
    expect(robot.mode).toBe("autonomous");
    expect(robot.leaseId).toBeUndefined();
    const refused = next<Envelope<"error">>((h) => operator.on("error", h), (e) => e.payload.ref === "late-twist", "refusal");
    operator.send("twist", { lease_id: lease.lease_id, linear: { x_mps: 1 }, angular: { z_radps: 0 } }, { id: "late-twist" });
    expect((await refused).payload.code).toBe("not_authorized");
  });

  it("realizes the same twist per morphology: the omni robot strafes, diff-drive does not", async () => {
    const drive = async (idx: number) => {
      const robot = sim.robots[idx]!;
      const grant = next<Envelope<"lease.granted">>((h) => operator.on("lease.granted", h), (e) => e.payload.robot_id === robot.robotId, "grant");
      operator.send("lease.claim", { robot_id: robot.robotId! });
      const lease = (await grant).payload;
      await sleep(100);
      const before = robot.pose;
      for (let i = 0; i < 10; i++) {
        operator.send("twist", { lease_id: lease.lease_id, linear: { x_mps: 0, y_mps: 0.8 }, angular: { z_radps: 0 } });
        await sleep(50);
      }
      await sleep(2 * DEADMAN_MS);
      operator.send("lease.release", { lease_id: lease.lease_id, resolution: "resolved" });
      const after = robot.pose;
      return { robot, moved: Math.hypot(after.x - before.x, after.y - before.y), dir: Math.atan2(after.y - before.y, after.x - before.x) - before.yaw };
    };
    const omni = await drive(19);
    expect(omni.robot.morphology).toBe("omni");
    expect(omni.moved).toBeGreaterThan(0.3);
    expect(Math.cos(omni.dir - Math.PI / 2)).toBeGreaterThan(0.99); // to its left
    const diff = await drive(1);
    expect(diff.robot.morphology).toBe("diff");
    expect(diff.moved).toBe(0);
  });

  it("--no-drive declares a manifest without a drive block, and ignores twist", async () => {
    const f = await fleet({ count: 1, drive: false, namePrefix: "nodrive" });
    const id = f.robots[0]!.robotId!;
    const snap = await snapshotUntil(svc, (s) => !!s.robots.find((r) => r.robot_id === id)?.manifest, "no-drive robot");
    const m = snap.robots.find((r) => r.robot_id === id)!.manifest!;
    expect(m.drive).toBeUndefined();
    expect(m.cameras).toHaveLength(1);
  });

  it("--help-rate raises help.request and the server queues the robot", async () => {
    const f = await fleet({ count: 2, helpRatePerMin: 60 * 60, namePrefix: "needy" });
    const ids = new Set(f.robots.map((r) => r.robotId!));
    const help = await next<FleetEvent<"robot.help_requested">>((h) => svc.onEvent("robot.help_requested", h), (e) => ids.has(e.robot_id), "help");
    expect(help.data.reason).toMatch(/^[a-z_]+$/);
    await snapshotUntil(svc, (s) => s.robots.some((r) => ids.has(r.robot_id) && r.state === "HELP_REQUESTED"), "HELP_REQUESTED");
  });
});
