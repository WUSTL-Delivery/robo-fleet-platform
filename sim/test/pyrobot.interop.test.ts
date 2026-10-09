// Interop for the teleop data plane (protocol/README.md, "Teleop data plane"):
// the console's own twist transport (werift as the offerer) against the
// Python SDK's robot (aiortc as the answerer), through the real fleet-server.
//
// The robot is sdk/python/examples/fake_robot.py in a child process. What it
// did is read from the outside only: the telemetry it reports and what it
// prints.
//
// Opt-in, because it needs a Python with the SDK's webrtc extra installed:
//
//   python3 -m venv /tmp/fleet-py && /tmp/fleet-py/bin/pip install -e '../sdk/python[webrtc]'
//   FLEET_PYTHON=/tmp/fleet-py/bin/python npx vitest run test/pyrobot.interop.test.ts
//
// Without FLEET_PYTHON the whole file is skipped.
import { spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterAll, beforeAll, describe, expect, inject, it } from "vitest";
import { FleetClient, type Envelope, type Lease, type TwistPayload, type WebSocketConstructor } from "@fleet-platform/sdk";
import { RTCPeerConnection } from "werift";
import { WebSocket as WsWebSocket } from "ws";
import {
  RETRY_AFTER_MS,
  openTwistTransport,
  type ChannelLike,
  type CreatePeer,
  type PeerLike,
  type TwistTransport,
} from "../../console/src/teleop/twistTransport.js";

const python = process.env.FLEET_PYTHON;
const server = inject("fleetServer");
const WS = WsWebSocket as unknown as WebSocketConstructor;
const fakeRobot = fileURLToPath(new URL("../../sdk/python/examples/fake_robot.py", import.meta.url));
const DEADMAN_MS = 300;
const TELEMETRY_HZ = 20;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const FORWARD = { linear: { x_mps: 1 }, angular: { z_radps: 0 } };

async function until(ok: () => boolean, what: string, timeoutMs = 8000): Promise<number> {
  const t0 = Date.now();
  while (!ok()) {
    if (Date.now() - t0 > timeoutMs) throw new Error(`timed out waiting for ${what}`);
    await sleep(10);
  }
  return Date.now() - t0;
}

/** The operator's end of one lease: the console transport over werift, with taps for the test. */
class OperatorLink {
  readonly transport: TwistTransport;
  readonly busSent: TwistPayload[] = [];
  /** The newest peer connection and data channel, for doing what the transport never would. */
  pc: RTCPeerConnection | undefined;
  raw: ChannelLike | undefined;
  peers = 0;

  constructor(client: FleetClient, robotId: string, lease: Lease) {
    const createPeer: CreatePeer = () => {
      this.peers += 1;
      const pc = new RTCPeerConnection({ iceServers: [], iceAdditionalHostAddresses: ["127.0.0.1"] });
      this.pc = pc;
      const peer = pc as unknown as PeerLike;
      const create = peer.createDataChannel.bind(peer);
      peer.createDataChannel = (label, options) => (this.raw = create(label, options));
      return peer;
    };
    this.transport = openTwistTransport({
      client,
      robotId,
      leaseId: lease.lease_id,
      sendBus: (payload) => {
        this.busSent.push(payload);
        client.send("twist", payload);
      },
      createPeer,
    });
  }

  get active(): "bus" | "p2p" {
    return this.transport.status.active;
  }
}

describe.skipIf(!python)("teleop data plane: console transport (werift) to the Python SDK robot (aiortc)", () => {
  let proc: ChildProcess;
  let stateDir: string;
  let out = "";
  let robotId: string;
  let operator: FleetClient;
  let lease: Lease;
  let link: OperatorLink;
  /** The speed the robot last reported, and when. */
  let v = 0;
  let samples = 0;

  const printed = (line: string) => out.split(line).length - 1;

  async function drive(ms: number, cmd = FORWARD): Promise<void> {
    const end = Date.now() + ms;
    while (Date.now() < end) {
      link.transport.send(cmd);
      await sleep(50);
    }
  }

  /** Waits for telemetry sampled after now. */
  async function fresh(): Promise<void> {
    const seen = samples;
    await until(() => samples >= seen + 2, "fresh telemetry");
  }

  beforeAll(async () => {
    stateDir = mkdtempSync(join(tmpdir(), "fleet-pyrobot-"));
    proc = spawn(
      python!,
      [fakeRobot, "--url", server.wsUrl, "--name", "py-interop", "--token-file", join(stateDir, "token.json"), "--no-wander", "--hz", String(TELEMETRY_HZ)],
      { env: { ...process.env, FLEET_ENROLL_KEY: server.enrollKey, PYTHONUNBUFFERED: "1" }, stdio: ["ignore", "pipe", "pipe"] },
    );
    proc.stdout!.on("data", (b: Buffer) => (out += b.toString()));
    proc.stderr!.on("data", (b: Buffer) => (out += b.toString()));
    await until(() => /online as (\S+)/.test(out) || proc.exitCode !== null, "the Python robot to come online", 15_000);
    const m = /online as (\S+)/.exec(out);
    if (!m) throw new Error(`fake_robot.py did not start:\n${out}`);
    robotId = m[1]!;
    await until(() => out.includes("data channel:"), "the robot to say whether it answers offers");
    if (!out.includes("answers WebRTC offers")) throw new Error(`FLEET_PYTHON has no webrtc extra (aiortc):\n${out}`);

    const res = await fetch(`${server.httpUrl}/api/admin/fleets/${server.fleet}/operator-invites`, {
      method: "POST",
      headers: { Authorization: `Bearer ${server.adminToken}` },
    });
    const invite = ((await res.json()) as { key: string }).key;
    operator = new FleetClient({ url: server.wsUrl, WebSocket: WS, reconnect: false, kind: "operator", name: "py-interop-op", enrollmentKey: invite });
    await operator.connect();
    operator.on("event", ({ payload }) => {
      if (payload.event !== "robot.telemetry" || payload.robot_id !== robotId) return;
      const velocity = (payload.data as { velocity?: { v_mps?: number } }).velocity;
      if (typeof velocity?.v_mps === "number") {
        v = velocity.v_mps;
        samples += 1;
      }
    });
    await operator.subscribe(["telemetry"]);

    const granted = new Promise<Envelope<"lease.granted">>((resolve) => {
      const off = operator.on("lease.granted", (e) => {
        off();
        resolve(e);
      });
    });
    operator.send("lease.claim", { robot_id: robotId });
    lease = (await granted).payload;
    await until(() => out.includes("took over"), "the robot to hear its lease");
    link = new OperatorLink(operator, robotId, lease);
  }, 40_000);

  afterAll(async () => {
    link?.transport.close();
    operator?.close();
    if (proc && proc.exitCode === null) {
      proc.kill("SIGINT");
      await until(() => proc.exitCode !== null, "the robot to exit", 5000).catch(() => proc.kill("SIGKILL"));
    }
    if (stateDir) rmSync(stateDir, { recursive: true, force: true });
    if (process.env.FLEET_INTEROP_LOG) console.log(out);
  });

  it("starts on the bus, then drives the Python robot over the data channel once it answers", async () => {
    expect(link.active).toBe("bus");
    await until(() => link.active === "p2p", "the direct link");
    // p2p means the robot's pong came back: aiortc answered werift's offer and echoed the ping.
    expect(link.transport.status.direct).toBe("open");
    expect(link.transport.status.rttMs).toBeGreaterThanOrEqual(0);
    expect(link.raw?.readyState).toBe("open");

    await drive(1000);
    await fresh();
    expect(v).toBeGreaterThan(0.9);
    // Every twist went over the channel and none over the bus.
    expect(link.busSent).toHaveLength(0);
    expect(printed("twist over the WebRTC data channel")).toBe(1);
    expect(printed("twist over the bus")).toBe(0);
  });

  it("stops on the robot's deadman when the channel goes silent, pings notwithstanding", async () => {
    await drive(300);
    const stops = printed("stop (deadman)");
    await sleep(DEADMAN_MS + 200);
    expect(printed("stop (deadman)")).toBe(stops + 1);
    await fresh();
    expect(v).toBe(0);
    expect(link.active).toBe("p2p"); // pings kept flowing; they are not twists
  });

  it("ignores a channel twist that bears another lease or an older seq, without losing the real driver", async () => {
    for (let i = 0; i < 10; i++) {
      link.raw!.send(JSON.stringify({ lease_id: "ls_not_this_one", seq: 1_000_000 + i, ...FORWARD }));
      await sleep(40);
    }
    link.raw!.send(JSON.stringify({ lease_id: lease.lease_id, seq: 1, ...FORWARD }));
    link.raw!.send("not json");
    await sleep(100);
    await fresh();
    expect(v).toBe(0);
    // The refused twists did not move the seq mark: the transport's next seq is still heard.
    await drive(300);
    await fresh();
    expect(v).toBeGreaterThan(0.9);
    await sleep(DEADMAN_MS + 200);
  });

  it("falls back to the bus when the peer connection is killed, and the robot answers the next offer", async () => {
    expect(link.active).toBe("p2p");
    const peers = link.peers;
    let driving = true;
    const loop = (async () => {
      while (driving) {
        link.transport.send(FORWARD);
        await sleep(100); // the console's cadence
      }
    })();
    await sleep(300);

    await link.pc!.close();
    const fellBack = await until(() => link.active === "bus" && out.includes("twist over the bus"), "bus twist reaching the robot");
    expect(fellBack).toBeLessThan(2000);
    await sleep(400);
    await fresh();
    expect(v).toBeGreaterThan(0.9); // still driving, now over the bus

    // The transport offers again on its own; the Python robot answers a second session under the same lease.
    await until(() => link.active === "p2p", "the direct link to come back", RETRY_AFTER_MS + 6000);
    expect(link.peers).toBe(peers + 1);
    const onBus = link.busSent.length;
    await sleep(DEADMAN_MS + 400);
    expect(link.busSent).toHaveLength(onBus);
    expect(printed("twist over the WebRTC data channel")).toBe(2);
    await fresh();
    expect(v).toBeGreaterThan(0.9);

    driving = false;
    await loop;
    await sleep(DEADMAN_MS + 200);
    await fresh();
    expect(v).toBe(0);
  });

  it("closes its end when the lease is released", async () => {
    const raw = link.raw!;
    expect(raw.readyState).toBe("open");
    operator.send("lease.release", { lease_id: lease.lease_id, resolution: "resolved" });
    await until(() => out.includes("control handed back (released)"), "the robot to hear the release");
    // The robot closed the peer because the lease ended; the operator's channel sees it go.
    await until(() => raw.readyState !== "open", "the robot to close the channel", 5000);
    link.transport.close();
    // An offer for a lease that is gone is not answered.
    const late = new OperatorLink(operator, robotId, lease);
    await sleep(1000);
    expect(late.active).toBe("bus");
    late.transport.close();
  });
});
