// Teleop over a TURN relay and nothing else: a real TURN server, a
// fleet-server configured with it, and an operator whose peer connection may
// use relayed candidates only, so no direct path can carry the twist.
//
// It needs a TURN server that takes the time-limited shared-secret credentials
// fleet-server signs (docs/INTEGRATION.md, section 7.1), so it is skipped
// unless these are set:
//
//   FLEET_TEST_TURN_URLS    comma-separated, e.g. turn:127.0.0.1:3478?transport=udp
//   FLEET_TEST_TURN_SECRET  the server's static-auth-secret
//
//   docker run --rm -p 3478:3478/udp coturn/coturn:4.6 -n --log-file=stdout \
//     --listening-port=3478 --fingerprint --use-auth-secret \
//     --static-auth-secret=relay-test-secret-0123456789 --realm=turn.test \
//     --no-multicast-peers --no-tls --no-dtls
//   FLEET_TEST_TURN_URLS='turn:127.0.0.1:3478?transport=udp' \
//     FLEET_TEST_TURN_SECRET=relay-test-secret-0123456789 \
//     npx vitest run test/relay.integration.test.ts
//
// Only the TURN port is published. The relay address coturn hands the operator
// is inside the container, and that is enough: the relay sends to the robot's
// own address from there, and the robot answers to where that came from.
//
// With FLEET_PYTHON also set (a Python with sdk/python and its `webrtc` extra,
// as for pyrobot.interop.test.ts) the same is done with the Python SDK's robot.
// aiortc gathers from the machine's interface addresses and not from loopback,
// so for that robot the TURN URL must name an address those can reach: the
// machine's own LAN address, not 127.0.0.1.
import { spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterAll, beforeAll, describe, expect, inject, it } from "vitest";
import { FleetClient, MemoryTokenStore, type Envelope, type Lease, type WebSocketConstructor } from "@fleet-platform/sdk";
import { WebSocket as WsWebSocket } from "ws";
import { LIMITS } from "../src/fleet.js";
import { SimRobot } from "../src/robot.js";
import { OperatorLink } from "./support/operatorLink.js";
import { startFleetServer, type FleetServer } from "./support/server.globalSetup.js";

const TURN_URLS = (process.env.FLEET_TEST_TURN_URLS ?? "").split(",").map((u) => u.trim()).filter(Boolean);
const TURN_SECRET = process.env.FLEET_TEST_TURN_SECRET ?? "";
const python = process.env.FLEET_PYTHON;
const fakeRobot = fileURLToPath(new URL("../../sdk/python/examples/fake_robot.py", import.meta.url));
const WS = WsWebSocket as unknown as WebSocketConstructor;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const FORWARD = { linear: { x_mps: 1 }, angular: { z_radps: 0 } };

async function until(ok: () => boolean, what: string, timeoutMs = 10_000): Promise<void> {
  const t0 = Date.now();
  while (!ok()) {
    if (Date.now() - t0 > timeoutMs) throw new Error(`timed out waiting for ${what}`);
    await sleep(10);
  }
}

describe.skipIf(TURN_URLS.length === 0 || TURN_SECRET === "")("teleop through a TURN relay only", () => {
  let server: FleetServer;
  let stopServer: (() => Promise<void>) | undefined;
  const cleanup: (() => void | Promise<void>)[] = [];

  async function operatorWithLease(name: string, robotId: string): Promise<{ operator: FleetClient; lease: Lease }> {
    const res = await fetch(`${server.httpUrl}/api/admin/fleets/${server.fleet}/operator-invites`, {
      method: "POST",
      headers: { Authorization: `Bearer ${server.adminToken}` },
    });
    const invite = ((await res.json()) as { key: string }).key;
    const operator = new FleetClient({ url: server.wsUrl, WebSocket: WS, reconnect: false, kind: "operator", name, enrollmentKey: invite });
    await operator.connect();
    cleanup.push(() => operator.close());
    const granted = new Promise<Envelope<"lease.granted">>((resolve) => {
      const off = operator.on("lease.granted", (e) => {
        off();
        resolve(e);
      });
    });
    operator.send("lease.claim", { robot_id: robotId });
    return { operator, lease: (await granted).payload };
  }

  /** The relay-only link: its ICE servers come from the server, and it may use nothing but a relay. */
  function relayLink(operator: FleetClient, robotId: string, lease: Lease): OperatorLink {
    const link = new OperatorLink(operator, robotId, lease, { iceServers: () => operator.iceServers(), relayOnly: true });
    cleanup.push(() => link.transport.close());
    return link;
  }

  /** What the operator's end must look like once the link is up. */
  function expectRelayed(link: OperatorLink, operator: FleetClient): void {
    expect(link.iceServers[0]![0]!.urls).toEqual(TURN_URLS);
    expect(link.iceServers[0]![0]!.username).toMatch(new RegExp(`:${operator.clientId}$`));
    // The pair ICE settled on leaves the operator through the relay.
    expect(link.pc!.iceTransports[0]!.connection.nominated?.localCandidate.type).toBe("relay");
  }

  async function drive(link: OperatorLink, ms: number): Promise<void> {
    const end = Date.now() + ms;
    while (Date.now() < end) {
      link.transport.send(FORWARD);
      await sleep(50);
    }
  }

  beforeAll(async () => {
    const started = await startFleetServer(inject("fleetServerBin"), "sim-relay", 15_000, {
      FLEET_TURN_URLS: TURN_URLS.join(","),
      FLEET_TURN_SECRET: TURN_SECRET,
    });
    server = started.cfg;
    stopServer = started.stop;
  });

  afterAll(async () => {
    for (const fn of cleanup.splice(0).reverse()) await fn();
    await stopServer?.();
  });

  it("drives a sim robot over the data channel when the operator may use relayed candidates only", async () => {
    const robot = new SimRobot({
      url: server.wsUrl,
      name: "relay-01",
      enrollmentKey: server.enrollKey,
      tokenStore: new MemoryTokenStore(),
      WebSocket: WS,
      morphology: "diff",
      limits: LIMITS.diff,
      drive: true,
      start: { x: 0, y: 0, yaw: 0 },
      frame: { kind: "local", frameId: "sim" },
    });
    cleanup.push(() => robot.stop());
    await robot.start();
    const { operator, lease } = await operatorWithLease("relay-op", robot.robotId!);
    await until(() => robot.leaseId === lease.lease_id, "the robot to hear its lease");

    const offered: string[] = [];
    robot.client.on("signal", ({ payload }) => {
      const d = payload.data as { sdp?: string; candidate?: { candidate?: string } };
      offered.push(d.sdp ?? d.candidate?.candidate ?? "");
    });

    const link = relayLink(operator, robot.robotId!, lease);
    await until(() => link.active === "p2p" && robot.peerOpen, "the relayed link");

    // Both ends were handed the TURN server, each with its own credential.
    expectRelayed(link, operator);
    expect(robot.peerIceServers![0]!.username).toMatch(new RegExp(`:${robot.robotId}$`));
    // The operator offered no address of its own: every candidate it sent is a relay's.
    const candidates = offered.flatMap((text) => text.match(/typ \w+/g) ?? []);
    expect(candidates.length).toBeGreaterThan(0);
    expect(candidates.filter((c) => c !== "typ relay")).toEqual([]);

    const before = robot.pose;
    await drive(link, 1000);
    const after = robot.pose;
    expect(Math.hypot(after.x - before.x, after.y - before.y)).toBeGreaterThan(0.6);
    expect(robot.twistsAccepted.p2p).toBeGreaterThanOrEqual(15);
    expect(robot.twistsAccepted.bus).toBe(0);
    expect(link.busSent).toHaveLength(0);
    expect(robot.twistVia).toBe("p2p");
    expect(link.transport.status.rttMs).toBeGreaterThanOrEqual(0);
  });

  it.skipIf(!python)("drives the Python SDK's robot the same way", async () => {
    const stateDir = mkdtempSync(join(tmpdir(), "fleet-pyrelay-"));
    cleanup.push(() => rmSync(stateDir, { recursive: true, force: true }));
    let out = "";
    const proc: ChildProcess = spawn(
      python!,
      [fakeRobot, "--url", server.wsUrl, "--name", "py-relay", "--token-file", join(stateDir, "token.json"), "--no-wander"],
      { env: { ...process.env, FLEET_ENROLL_KEY: server.enrollKey, FLEET_ICE_SERVERS: "", PYTHONUNBUFFERED: "1" }, stdio: ["ignore", "pipe", "pipe"] },
    );
    proc.stdout!.on("data", (b: Buffer) => (out += b.toString()));
    proc.stderr!.on("data", (b: Buffer) => (out += b.toString()));
    cleanup.push(async () => {
      if (proc.exitCode === null) {
        proc.kill("SIGINT");
        await until(() => proc.exitCode !== null, "the robot to exit", 5000).catch(() => proc.kill("SIGKILL"));
      }
      if (process.env.FLEET_INTEROP_LOG) console.log(out);
    });
    await until(() => /online as (\S+)/.test(out) || proc.exitCode !== null, "the Python robot to come online", 15_000);
    const robotId = /online as (\S+)/.exec(out)?.[1];
    if (!robotId) throw new Error(`fake_robot.py did not start:\n${out}`);
    await until(() => out.includes("data channel:"), "the robot to say whether it answers offers");
    if (!out.includes("answers WebRTC offers")) throw new Error(`FLEET_PYTHON has no webrtc extra (aiortc):\n${out}`);

    const { operator, lease } = await operatorWithLease("py-relay-op", robotId);
    await until(() => out.includes("took over"), "the robot to hear its lease");
    const link = relayLink(operator, robotId, lease);
    await until(() => link.active === "p2p", "the relayed link");
    expectRelayed(link, operator);

    await drive(link, 1000);
    expect(link.busSent).toHaveLength(0);
    await until(() => out.includes("twist over the WebRTC data channel"), "the robot to report channel twist");
    expect(out).not.toContain("twist over the bus");
  }, 40_000);
});
