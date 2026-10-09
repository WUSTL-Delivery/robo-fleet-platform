// Who is handed which ICE servers (protocol/README.md, "ICE servers"), against
// real fleet-servers: one configured with STUN/TURN, one with none, and one
// that answers as a server older than `ice.request` does. The operator end is
// the console's twist transport asking as the console does
// (`() => client.iceServers()`); the robot end is the sim robot.
//
// The configured server is a stub that answers STUN and refuses every TURN
// allocation (support/stunStub.ts). What is checked is what each peer
// connection was created with, and that the credential it then presented to
// that server was its own. A relayed path through a real TURN server is
// test/relay.integration.test.ts.
import { createHmac } from "node:crypto";
import { afterAll, afterEach, beforeAll, describe, expect, inject, it } from "vitest";
import {
  FleetClient,
  MemoryTokenStore,
  type Envelope,
  type Lease,
  type TokenStore,
  type WebSocketConstructor,
} from "@fleet-platform/sdk";
import { RTCPeerConnection } from "werift";
import { WebSocket as WsWebSocket } from "ws";
import { RETRY_AFTER_MS, type IceServer } from "../../console/src/teleop/twistTransport.js";
import { LIMITS } from "../src/fleet.js";
import { ICE_EXPIRY_MARGIN_MS, TwistAnswerer } from "../src/p2p.js";
import { SimRobot } from "../src/robot.js";
import { serverWithoutIce } from "./support/oldServer.js";
import { OperatorLink } from "./support/operatorLink.js";
import { startFleetServer, type FleetServer } from "./support/server.globalSetup.js";
import { startStunStub, type StunStub } from "./support/stunStub.js";

const plain = inject("fleetServer");
const TURN_SECRET = "sim-turn-secret-0123456789";
/** A fleet-server configured with the stub as its STUN and TURN server. */
let withIce: FleetServer & { stunUrls: string[]; turnUrls: string[] };
let stub: StunStub;
let stopServer: (() => Promise<void>) | undefined;

beforeAll(async () => {
  stub = await startStunStub(TURN_SECRET);
  const stunUrls = [`stun:127.0.0.1:${stub.port}`];
  const turnUrls = [`turn:127.0.0.1:${stub.port}?transport=udp`];
  const started = await startFleetServer(inject("fleetServerBin"), "sim-ice", 15_000, {
    FLEET_STUN_URLS: stunUrls.join(","),
    FLEET_TURN_URLS: turnUrls.join(","),
    FLEET_TURN_SECRET: TURN_SECRET,
  });
  stopServer = started.stop;
  withIce = { ...started.cfg, stunUrls, turnUrls };
});
afterAll(async () => {
  await stopServer?.();
  await stub?.close();
});
const WS = WsWebSocket as unknown as WebSocketConstructor;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));

async function until(ok: () => boolean, what: string, timeoutMs = 8000): Promise<void> {
  const t0 = Date.now();
  while (!ok()) {
    if (Date.now() - t0 > timeoutMs) throw new Error(`timed out waiting for ${what}`);
    await sleep(10);
  }
}

function simRobot(server: FleetServer, name: string, extra: { tokenStore?: TokenStore; WebSocket?: WebSocketConstructor } = {}): SimRobot {
  return new SimRobot({
    url: server.wsUrl,
    name,
    enrollmentKey: server.enrollKey,
    tokenStore: extra.tokenStore ?? new MemoryTokenStore(),
    WebSocket: extra.WebSocket ?? WS,
    morphology: "diff",
    limits: LIMITS.diff,
    drive: true,
    start: { x: 0, y: 0, yaw: 0 },
    frame: { kind: "local", frameId: "sim" },
  });
}

async function operatorOn(server: FleetServer, name: string, WebSocket: WebSocketConstructor = WS): Promise<FleetClient> {
  const res = await fetch(`${server.httpUrl}/api/admin/fleets/${server.fleet}/operator-invites`, {
    method: "POST",
    headers: { Authorization: `Bearer ${server.adminToken}` },
  });
  const invite = ((await res.json()) as { key: string }).key;
  const op = new FleetClient({ url: server.wsUrl, WebSocket, reconnect: false, kind: "operator", name, enrollmentKey: invite });
  await op.connect();
  return op;
}

async function claim(operator: FleetClient, robot: SimRobot): Promise<Lease> {
  const granted = new Promise<Envelope<"lease.granted">>((resolve) => {
    const off = operator.on("lease.granted", (e) => {
      off();
      resolve(e);
    });
  });
  operator.send("lease.claim", { robot_id: robot.robotId! });
  const lease = (await granted).payload;
  await until(() => robot.leaseId === lease.lease_id, "the robot to hear its lease");
  return lease;
}

/** The list the ICE-configured server must hand `clientId`: its URLs, and a TURN credential that is that client's own. */
function expectConfigured(list: readonly IceServer[] | undefined, clientId: string): void {
  expect(list).toBeDefined();
  const stun = list!.filter((s) => s.username === undefined);
  const turn = list!.filter((s) => s.username !== undefined);
  expect(stun.map((s) => s.urls)).toEqual([withIce.stunUrls]);
  expect(turn).toHaveLength(1);
  expect(turn[0]!.urls).toEqual(withIce.turnUrls);
  // username is "<expiry in Unix seconds>:<client id>", the credential its HMAC under the shared secret.
  const [expiry, who] = turn[0]!.username!.split(":");
  expect(who).toBe(clientId);
  expect(Number(expiry) * 1000).toBeGreaterThan(Date.now());
  expect(turn[0]!.credential).toBe(createHmac("sha1", TURN_SECRET).update(turn[0]!.username!).digest("base64"));
}

describe("ICE servers reach both peer connections", () => {
  const cleanup: (() => void)[] = [];
  afterEach(() => {
    for (const fn of cleanup.splice(0).reverse()) fn();
  });

  it("configured: each side is given the installation's servers with its own credential, and the operator asks again on a retry", async () => {
    const robot = simRobot(withIce, "ice-01");
    cleanup.push(() => robot.stop());
    await robot.start();
    const operator = await operatorOn(withIce, "ice-op");
    cleanup.push(() => operator.close());
    expect(robot.iceRequests).toBe(0); // nothing is asked before there is a lease

    const lease = await claim(operator, robot);
    const link = new OperatorLink(operator, robot.robotId!, lease, { iceServers: () => operator.iceServers() });
    cleanup.push(() => link.transport.close());
    await until(() => link.active === "p2p" && robot.peerOpen, "the direct link");

    expectConfigured(link.iceServers[0], operator.clientId!);
    expectConfigured(robot.peerIceServers, robot.robotId!);
    expect(robot.iceRequests).toBe(1);
    const robotFirst = robot.peerIceServers;
    // Not only handed over: each peer connection went to that server with its own credential.
    const presented = () => stub.usernames.map((u) => u.split(":")[1]);
    expect(presented()).toContain(operator.clientId);
    expect(presented()).toContain(robot.robotId);
    expect(stub.bindings).toBeGreaterThan(0);

    // The link dies; the transport offers again after its pause.
    robot.dropPeer();
    await until(() => link.active === "bus", "the fallback to the bus");
    await until(() => link.active === "p2p" && robot.peerOpen, "the direct link to come back", RETRY_AFTER_MS + 6000);
    // The operator asked again for the new session...
    expect(link.iceServers).toHaveLength(2);
    expectConfigured(link.iceServers[1], operator.clientId!);
    expect(link.iceServers[1]).not.toBe(link.iceServers[0]);
    // ...and the robot answered it with what it was told when it took the lease.
    expect(robot.iceRequests).toBe(1);
    expect(robot.peerIceServers).toBe(robotFirst);
  });

  it("nothing configured: both peer connections get an explicit empty list", async () => {
    const robot = simRobot(plain, "ice-none-01");
    cleanup.push(() => robot.stop());
    await robot.start();
    const operator = await operatorOn(plain, "ice-none-op");
    cleanup.push(() => operator.close());
    const fromRobot: string[] = [];
    operator.on("signal", ({ payload }) => {
      const d = payload.data as { sdp?: string; candidate?: { candidate?: string } };
      fromRobot.push(d.sdp ?? d.candidate?.candidate ?? "");
    });
    const lease = await claim(operator, robot);
    const link = new OperatorLink(operator, robot.robotId!, lease, { iceServers: () => operator.iceServers() });
    cleanup.push(() => link.transport.close());
    await until(() => link.active === "p2p" && robot.peerOpen, "the direct link");

    expect(link.iceServers).toEqual([[]]);
    expect(robot.peerIceServers).toEqual([]);
    expect(robot.iceRequests).toBe(1); // it asked; the answer was the empty list
    // And with none configured the robot asked no STUN server of its own
    // choosing: every candidate it sent is one of its own addresses.
    expect(fromRobot.length).toBeGreaterThan(0);
    expect(fromRobot.filter((text) => /typ (srflx|relay)/.test(text))).toEqual([]);
  });

  it("a server older than ice.request (invalid_message): both carry on with an empty list", async () => {
    const robotSocket = serverWithoutIce();
    const operatorSocket = serverWithoutIce();
    const robot = simRobot(withIce, "ice-old-01", { WebSocket: robotSocket.WebSocket });
    cleanup.push(() => robot.stop());
    await robot.start();
    const operator = await operatorOn(withIce, "ice-old-op", operatorSocket.WebSocket);
    cleanup.push(() => operator.close());
    await expect(operator.iceConfig()).rejects.toMatchObject({ code: "invalid_message" });

    const lease = await claim(operator, robot);
    const link = new OperatorLink(operator, robot.robotId!, lease, { iceServers: () => operator.iceServers() });
    cleanup.push(() => link.transport.close());
    await until(() => link.active === "p2p" && robot.peerOpen, "the direct link");

    expect(link.iceServers).toEqual([[]]);
    expect(robot.peerIceServers).toEqual([]);
    expect(operatorSocket.refused).toBe(2); // the explicit iceConfig() above, then the transport's
    expect(robotSocket.refused).toBeGreaterThanOrEqual(1);
  });

  it("a lease taken from the welcome (the robot restarted while leased) asks too", async () => {
    const tokens = new MemoryTokenStore();
    const first = simRobot(withIce, "ice-restart-01", { tokenStore: tokens });
    await first.start();
    const operator = await operatorOn(withIce, "ice-restart-op");
    cleanup.push(() => operator.close());
    const lease = await claim(operator, first);
    const robotId = first.robotId!;
    first.stop(); // the process is gone; the server keeps the lease

    // The same identity comes back knowing nothing of the lease: only the welcome names it.
    const again = simRobot(withIce, "ice-restart-01", { tokenStore: tokens });
    cleanup.push(() => again.stop());
    const welcome = await again.start();
    expect(again.robotId).toBe(robotId);
    expect(welcome.lease?.lease_id).toBe(lease.lease_id);
    expect(again.leaseId).toBe(lease.lease_id);
    expect(again.iceRequests).toBe(1);

    const link = new OperatorLink(operator, robotId, lease, { iceServers: () => operator.iceServers() });
    cleanup.push(() => link.transport.close());
    await until(() => link.active === "p2p" && again.peerOpen, "the direct link");
    expectConfigured(again.peerIceServers, robotId);
    expect(again.iceRequests).toBe(1);
  });
});

// The answerer on its own with a real robot connection, so the clock can be
// moved: a stored answer serves every session of its lease until the
// credential is at its expiry, and then the robot asks when the offer arrives.
describe("the robot's stored ICE answer", () => {
  let client: FleetClient;
  let now = Date.now();
  const offers: RTCPeerConnection[] = [];
  let n = 0;

  beforeAll(async () => {
    client = new FleetClient({
      url: withIce.wsUrl,
      WebSocket: WS,
      reconnect: false,
      kind: "robot",
      name: "ice-expiry-01",
      enrollmentKey: withIce.enrollKey,
    });
    await client.connect();
  });
  afterAll(() => {
    client?.close();
    for (const pc of offers) void pc.close();
  });

  /** A real offer, delivered as the server would relay it from `operatorId`. */
  async function offer(answerer: TwistAnswerer, leaseId: string, operatorId: string): Promise<void> {
    const pc = new RTCPeerConnection({ iceServers: [] });
    offers.push(pc);
    pc.createDataChannel("twist", { ordered: false, maxRetransmits: 0 });
    const sdp = (await pc.createOffer()).sdp;
    answerer.onSignal({ from: operatorId, kind: "offer", data: { session: `s-${++n}`, lease_id: leaseId, type: "offer", sdp } });
  }

  it("is reused under the lease until expires_at_ms, asked for again after it, and never carried to another lease", async () => {
    const answerer = new TwistAnswerer({ client: () => client, onTwist: () => false, now: () => now });
    const expiresAt = (await client.iceConfig()).expires_at_ms!;
    expect(expiresAt).toBeGreaterThan(Date.now());

    answerer.grant("ls_one", "o_one");
    expect(answerer.iceRequests).toBe(1);
    answerer.grant("ls_one", "o_one"); // a renewal
    expect(answerer.iceRequests).toBe(1);

    await offer(answerer, "ls_one", "o_one");
    await until(() => answerer.iceServers !== undefined, "the first peer connection");
    const first = answerer.iceServers!;
    expectConfigured(first, client.clientId!);

    // A second session a while later: still the stored answer.
    now = expiresAt - ICE_EXPIRY_MARGIN_MS - 1;
    await offer(answerer, "ls_one", "o_one");
    await sleep(200);
    expect(answerer.iceRequests).toBe(1);
    expect(answerer.iceServers).toBe(first);

    // Past the expiry: the robot asks when the offer arrives and uses the new answer.
    now = expiresAt + 1;
    await offer(answerer, "ls_one", "o_one");
    await until(() => answerer.iceServers !== first, "a peer connection made with a new answer");
    expect(answerer.iceRequests).toBe(2);
    // The clock in this test is ahead of the server's, so the new credential
    // is itself "expired" by it: the robot does not use one it believes is
    // past its time, and goes ahead with none.
    expect(answerer.iceServers).toEqual([]);

    // Another lease: asked afresh, whatever was stored.
    now = Date.now();
    answerer.grant("ls_two", "o_two");
    expect(answerer.iceRequests).toBe(3);
    await offer(answerer, "ls_two", "o_two");
    await until(() => (answerer.iceServers?.length ?? 0) > 0, "the new lease's peer connection");
    expectConfigured(answerer.iceServers, client.clientId!);
    expect(answerer.iceRequests).toBe(3);

    answerer.revoke();
  });

  it("uses a fixed list as given and never asks the server", async () => {
    const fixed = [{ urls: `stun:127.0.0.1:${stub.port}` }];
    const answerer = new TwistAnswerer({ client: () => client, onTwist: () => false, iceServers: fixed });
    answerer.grant("ls_fixed", "o_fixed");
    await offer(answerer, "ls_fixed", "o_fixed");
    await until(() => answerer.iceServers !== undefined, "the peer connection");
    expect(answerer.iceServers).toBe(fixed);
    expect(answerer.iceRequests).toBe(0);
    answerer.revoke();
  });
});
