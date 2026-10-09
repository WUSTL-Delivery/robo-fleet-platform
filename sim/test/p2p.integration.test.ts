// The teleop data plane end to end (protocol/README.md, "Teleop data plane"):
// the console's twist transport on the operator side, the sim robot's answerer
// on the other, WebRTC by werift on both ends, and the offer / answer / ICE
// relayed by the real fleet-server (test/support/server.globalSetup.ts).
//
// The operator side is the console's own module, not a copy of it: what is
// tested here is what the browser runs, with werift's peer connection handed
// in where the browser's would be.
import { afterAll, beforeAll, describe, expect, inject, it } from "vitest";
import { FleetClient, type Envelope, type Lease, type TwistPayload, type WebSocketConstructor } from "@fleet-platform/sdk";
import { RTCPeerConnection } from "werift";
import { WebSocket as WsWebSocket } from "ws";
import {
  PONG_TIMEOUT_MS,
  RETRY_AFTER_MS,
  openTwistTransport,
  type ChannelLike,
  type CreatePeer,
  type PeerLike,
  type TwistLinkStatus,
  type TwistTransport,
} from "../../console/src/teleop/twistTransport.js";
import { startFleet, type Fleet } from "../src/fleet.js";
import type { SimRobot } from "../src/robot.js";
import { DEADMAN_MS } from "../src/teleop.js";

const server = inject("fleetServer");
const WS = WsWebSocket as unknown as WebSocketConstructor;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const FORWARD = { linear: { x_mps: 1 }, angular: { z_radps: 0 } };
const STOPPED = { vx: 0, vy: 0, wz: 0 };

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
  readonly statuses: TwistLinkStatus[] = [];
  readonly busSent: TwistPayload[] = [];
  /** The newest data channel, for sending what the transport never would. */
  raw: ChannelLike | undefined;
  /** true: the direct link goes silent in both directions without closing. */
  cut = false;
  peers = 0;

  constructor(client: FleetClient, robotId: string, lease: Lease) {
    const createPeer: CreatePeer = () => {
      this.peers += 1;
      const pc = new RTCPeerConnection({ iceServers: [], iceAdditionalHostAddresses: ["127.0.0.1"] });
      const peer = pc as unknown as PeerLike;
      const create = peer.createDataChannel.bind(peer);
      peer.createDataChannel = (label, options) => this.#tap(create(label, options));
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
      onStatus: (s) => this.statuses.push(s),
      createPeer,
    });
  }

  get active(): "bus" | "p2p" {
    return this.transport.status.active;
  }

  #tap(dc: ChannelLike): ChannelLike {
    const link = this;
    const tapped: ChannelLike = {
      get readyState() {
        return dc.readyState;
      },
      onopen: null,
      onclose: null,
      onmessage: null,
      send: (data) => {
        if (!link.cut) dc.send(data);
      },
      close: () => dc.close(),
    };
    dc.onopen = () => tapped.onopen?.(undefined as never);
    dc.onclose = () => tapped.onclose?.(undefined as never);
    dc.onmessage = (ev) => {
      if (!link.cut) tapped.onmessage?.(ev);
    };
    this.raw = dc;
    return tapped;
  }
}

describe("teleop data plane: twist over a WebRTC data channel", () => {
  let fleet: Fleet;
  let robot: SimRobot;
  let operator: FleetClient;
  let lease: Lease;
  let link: OperatorLink;

  /** Sends `cmd` through the transport at 20 Hz for `ms`. */
  async function drive(ms: number, cmd = FORWARD): Promise<void> {
    const end = Date.now() + ms;
    while (Date.now() < end) {
      link.transport.send(cmd);
      await sleep(50);
    }
  }

  beforeAll(async () => {
    fleet = await startFleet({ url: server.wsUrl, enrollmentKey: server.enrollKey, stateDir: null, count: 1, namePrefix: "p2p" });
    robot = fleet.robots[0]!;
    const res = await fetch(`${server.httpUrl}/api/admin/fleets/${server.fleet}/operator-invites`, {
      method: "POST",
      headers: { Authorization: `Bearer ${server.adminToken}` },
    });
    const invite = ((await res.json()) as { key: string }).key;
    operator = new FleetClient({ url: server.wsUrl, WebSocket: WS, reconnect: false, kind: "operator", name: "p2p-op", enrollmentKey: invite });
    await operator.connect();

    const granted = new Promise<Envelope<"lease.granted">>((resolve) => {
      const off = operator.on("lease.granted", (e) => {
        off();
        resolve(e);
      });
    });
    operator.send("lease.claim", { robot_id: robot.robotId! });
    lease = (await granted).payload;
    await until(() => robot.leaseId === lease.lease_id, "the robot to hear its lease");
    link = new OperatorLink(operator, robot.robotId!, lease);
  });

  afterAll(() => {
    link?.transport.close();
    operator?.close();
    fleet?.stop();
  });

  it("starts on the bus, then drives over the data channel once the robot answers", async () => {
    expect(link.active).toBe("bus");
    expect(link.transport.status.direct).toBe("connecting");
    await until(() => link.active === "p2p", "the direct link");
    expect(link.transport.status.direct).toBe("open");
    expect(link.transport.status.rttMs).toBeGreaterThanOrEqual(0);
    expect(robot.peerOpen).toBe(true);
    expect(link.raw?.readyState).toBe("open");

    const before = robot.pose;
    await drive(1000);
    const after = robot.pose;
    expect(Math.hypot(after.x - before.x, after.y - before.y)).toBeGreaterThan(0.6);
    // Every twist went over the channel and none over the bus.
    expect(robot.twistsAccepted.p2p).toBeGreaterThanOrEqual(15);
    expect(robot.twistsAccepted.bus).toBe(0);
    expect(link.busSent).toHaveLength(0);
    expect(robot.twistVia).toBe("p2p");
  });

  it("stops on the deadman when the channel goes silent", async () => {
    await drive(400);
    expect(robot.velocity.vx).toBeGreaterThan(0.9);
    // No stop is sent: only the robot's own deadman can end this.
    await sleep(DEADMAN_MS + 150);
    expect(robot.velocity).toEqual(STOPPED);
    const rest = robot.pose;
    await sleep(300);
    expect(robot.pose).toEqual(rest);
    // Pings kept flowing the whole time; they are not twists and did not hold the deadman off.
    expect(link.active).toBe("p2p");
  });

  it("ignores a twist on the channel that bears another lease, or an older seq", async () => {
    const accepted = robot.twistsAccepted.p2p;
    const rest = robot.pose;
    for (let i = 0; i < 10; i++) {
      // A lease this robot does not hold, with a seq far ahead of anything sent.
      link.raw!.send(JSON.stringify({ lease_id: "ls_not_this_one", seq: 1_000_000 + i, ...FORWARD }));
      await sleep(40);
    }
    expect(robot.twistsAccepted.p2p).toBe(accepted);
    expect(robot.velocity).toEqual(STOPPED);
    expect(robot.pose).toEqual(rest);

    // The right lease but a seq the robot has already passed: a late, reordered packet.
    link.raw!.send(JSON.stringify({ lease_id: lease.lease_id, seq: 1, ...FORWARD }));
    // Not a twist at all.
    link.raw!.send("not json");
    link.raw!.send(JSON.stringify({ lease_id: lease.lease_id, seq: 2_000_000, linear: { x_mps: "fast" }, angular: { z_radps: 0 } }));
    await sleep(150);
    expect(robot.velocity).toEqual(STOPPED);
    // The refused twists did not move the seq mark: the real driver is still heard.
    await drive(200);
    expect(robot.twistsAccepted.p2p).toBeGreaterThan(accepted);
    await sleep(DEADMAN_MS + 150);
  });

  it("does not let a bus twist override a fresh data-channel twist", async () => {
    const bus = robot.twistsAccepted.bus;
    const driving = drive(600);
    await sleep(300);
    // A straggler on the bus (the server relays it: the lease is valid) asking for reverse.
    for (let i = 0; i < 5; i++) {
      operator.send("twist", { lease_id: lease.lease_id, linear: { x_mps: -1 }, angular: { z_radps: 0 } });
      await sleep(30);
      expect(robot.velocity.vx).toBeGreaterThan(0.9);
    }
    await driving;
    expect(robot.twistsAccepted.bus).toBe(bus);
    await sleep(DEADMAN_MS + 150);
  });

  it("falls back to bus twist within two seconds of the peer connection being killed, then returns to the channel", async () => {
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
    expect(robot.velocity.vx).toBeGreaterThan(0.9);

    const busBefore = robot.twistsAccepted.bus;
    robot.dropPeer();
    const fellBack = await until(() => link.active === "bus" && robot.twistsAccepted.bus > busBefore, "bus twist reaching the robot");
    expect(fellBack).toBeLessThan(2000);
    expect(link.transport.status.direct).toBe("retrying");
    await sleep(400);
    expect(robot.velocity.vx).toBeGreaterThan(0.9); // still driving, now over the bus
    expect(robot.twistVia).toBe("bus");

    // The transport offers again on its own; twist moves back and the bus goes quiet.
    await until(() => link.active === "p2p", "the direct link to come back", RETRY_AFTER_MS + 6000);
    expect(link.peers).toBe(peers + 1);
    await sleep(DEADMAN_MS + 200);
    const busAfter = robot.twistsAccepted.bus;
    const sentOnBus = link.busSent.length;
    await sleep(500);
    expect(robot.twistsAccepted.bus).toBe(busAfter);
    expect(link.busSent).toHaveLength(sentOnBus);
    expect(robot.twistVia).toBe("p2p");
    expect(robot.velocity.vx).toBeGreaterThan(0.9);

    driving = false;
    await loop;
    await sleep(DEADMAN_MS + 150);
    expect(robot.velocity).toEqual(STOPPED);
  });

  it("falls back within two seconds when the link dies silently (no close, no answer to pings)", async () => {
    expect(link.active).toBe("p2p");
    let driving = true;
    const loop = (async () => {
      while (driving) {
        link.transport.send(FORWARD);
        await sleep(100);
      }
    })();
    await sleep(300);
    const busBefore = robot.twistsAccepted.bus;
    link.cut = true;
    const fellBack = await until(() => link.active === "bus" && robot.twistsAccepted.bus > busBefore, "bus twist reaching the robot");
    expect(fellBack).toBeGreaterThan(PONG_TIMEOUT_MS - 300); // found by the unanswered ping, not by a close
    expect(fellBack).toBeLessThan(2000);
    link.cut = false;
    driving = false;
    await loop;
  });

  it("closes the peer when the lease is released; a twist on the dead lease drives nothing", async () => {
    await until(() => link.active === "p2p" && robot.peerOpen, "the direct link to come back", RETRY_AFTER_MS + 6000);
    const raw = link.raw!;
    operator.send("lease.release", { lease_id: lease.lease_id, resolution: "resolved" });
    await until(() => robot.leaseId === undefined, "the robot to hear the release");
    // The robot closed its end because the lease ended, not the other way round.
    expect(robot.peerOpen).toBe(false);
    expect(robot.mode).toBe("autonomous");
    try {
      raw.send(JSON.stringify({ lease_id: lease.lease_id, seq: 9_000_000, ...FORWARD }));
    } catch {
      /* already closed under us: equally nothing to obey */
    }
    await sleep(150);
    expect(robot.velocity).toEqual(STOPPED);
    link.transport.close();
    // An offer for a lease that is gone is not answered.
    const late = new OperatorLink(operator, robot.robotId!, lease);
    await sleep(1000);
    expect(late.active).toBe("bus");
    expect(robot.peerOpen).toBe(false);
    late.transport.close();
  });
});
