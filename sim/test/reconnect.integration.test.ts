// A sim robot's lease across its own reconnects (protocol/README.md, "A
// robot's lease at connect"), against a real fleet-server whose leases expire
// after two seconds. The robot's control link is cut and held down from the
// test; the WebRTC peer to the operator stays up throughout, which is what
// makes a stale lease dangerous.
import { afterAll, beforeAll, describe, expect, inject, it } from "vitest";
import { FleetClient, MemoryTokenStore, type Envelope, type Lease, type WebSocketConstructor } from "@fleet-platform/sdk";
import { WebSocket as WsWebSocket } from "ws";
import { LIMITS } from "../src/fleet.js";
import { SimRobot } from "../src/robot.js";
import { OperatorLink } from "./support/operatorLink.js";

const server = inject("shortLeaseServer");
const WS = WsWebSocket as unknown as WebSocketConstructor;
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const FORWARD = { linear: { x_mps: 1 }, angular: { z_radps: 0 } };
const STOPPED = { vx: 0, vy: 0, wz: 0 };

async function until(ok: () => boolean, what: string, timeoutMs = 10_000): Promise<void> {
  const t0 = Date.now();
  while (!ok()) {
    if (Date.now() - t0 > timeoutMs) throw new Error(`timed out waiting for ${what}`);
    await sleep(10);
  }
}

/** The robot's control link: every socket it opens, and a switch that kills them and keeps new ones from connecting. */
class ControlLink {
  down = false;
  readonly #sockets = new Set<WsWebSocket>();
  readonly WebSocket: WebSocketConstructor;

  constructor() {
    const link = this;
    this.WebSocket = class extends WsWebSocket {
      constructor(url: string) {
        // Port 9 (discard) on loopback refuses: a dial that fails, as on a dead network.
        super(link.down ? "ws://127.0.0.1:9/ws" : url);
        link.#sockets.add(this);
        this.on("close", () => link.#sockets.delete(this));
        this.on("error", () => {});
      }
    } as unknown as WebSocketConstructor;
  }

  cut(): void {
    this.down = true;
    for (const s of this.#sockets) s.terminate();
  }

  restore(): void {
    this.down = false;
  }
}

describe("a robot's lease across a reconnect", () => {
  const control = new ControlLink();
  let robot: SimRobot;
  let operator: FleetClient;
  let lease: Lease;
  let link: OperatorLink;
  let renewing: ReturnType<typeof setInterval> | undefined;
  const revoked: Envelope<"lease.revoked">["payload"][] = [];
  const errors: string[] = [];

  /** Sends a twist under `leaseId` straight onto the data channel, as an operator end that never stopped would. */
  function channelTwist(leaseId: string, seq: number): boolean {
    try {
      link.raw!.send(JSON.stringify({ lease_id: leaseId, seq, ...FORWARD }));
      return true;
    } catch {
      return false; // the robot closed its end: equally nothing to obey
    }
  }

  beforeAll(async () => {
    robot = new SimRobot({
      url: server.wsUrl,
      name: "reconnect-01",
      enrollmentKey: server.enrollKey,
      tokenStore: new MemoryTokenStore(),
      WebSocket: control.WebSocket,
      morphology: "diff",
      limits: LIMITS.diff,
      drive: true,
      start: { x: 0, y: 0, yaw: 0 },
      frame: { kind: "local", frameId: "sim" },
    });
    await robot.start();

    const res = await fetch(`${server.httpUrl}/api/admin/fleets/${server.fleet}/operator-invites`, {
      method: "POST",
      headers: { Authorization: `Bearer ${server.adminToken}` },
    });
    const invite = ((await res.json()) as { key: string }).key;
    operator = new FleetClient({ url: server.wsUrl, WebSocket: WS, reconnect: false, kind: "operator", name: "reconnect-op", enrollmentKey: invite });
    await operator.connect();
    operator.on("lease.revoked", (e) => void revoked.push(e.payload));
    operator.on("error", (e) => void errors.push(e.payload.code));

    const granted = new Promise<Envelope<"lease.granted">>((resolve) => {
      const off = operator.on("lease.granted", (e) => {
        off();
        resolve(e);
      });
    });
    operator.send("lease.claim", { robot_id: robot.robotId! });
    lease = (await granted).payload;
    // The lease lives two seconds: keep it alive until the test wants it to run out.
    renewing = setInterval(() => operator.send("lease.renew", { lease_id: lease.lease_id }), 500);
    await until(() => robot.leaseId === lease.lease_id, "the robot to hear its lease");
    link = new OperatorLink(operator, robot.robotId!, lease);
    await until(() => link.active === "p2p" && robot.peerOpen, "the data channel to open");
  });

  afterAll(() => {
    clearInterval(renewing);
    link?.transport.close();
    operator?.close();
    robot?.stop();
  });

  it("keeps a lease the server still holds, and is driven again on the peer it kept", async () => {
    expect(channelTwist(lease.lease_id, 1_000_000)).toBe(true);
    await until(() => robot.twistsAccepted.p2p > 0, "a channel twist to be obeyed");

    control.cut();
    await until(() => robot.client.state !== "open", "the robot to notice its link is gone");
    // Away: it remembers the lease and obeys nothing under it.
    expect(robot.leaseId).toBe(lease.lease_id);
    const whileAway = robot.twistsAccepted.p2p;
    channelTwist(lease.lease_id, 1_000_001);
    await sleep(100);
    expect(robot.twistsAccepted.p2p).toBe(whileAway);
    expect(robot.velocity).toEqual(STOPPED);

    control.restore();
    await until(() => robot.client.state === "open", "the robot to reconnect");
    expect(robot.client.welcome?.lease?.lease_id).toBe(lease.lease_id);
    expect(robot.leaseId).toBe(lease.lease_id);
    expect(robot.mode).toBe("teleop");
    expect(robot.peerOpen).toBe(true);
    expect(channelTwist(lease.lease_id, 1_000_002)).toBe(true);
    await until(() => robot.twistsAccepted.p2p > whileAway, "a channel twist to be obeyed after the reconnect");
    expect(revoked).toEqual([]);
  });

  it("drops a lease that expired while it was away, and obeys the old lease id on neither transport", async () => {
    control.cut();
    await until(() => robot.client.state !== "open", "the robot to notice its link is gone");
    clearInterval(renewing); // nobody renews: the server revokes it while the robot cannot be told
    await until(() => revoked.length > 0, "the lease to expire");
    expect(revoked[0]).toMatchObject({ lease_id: lease.lease_id, reason: "expired" });
    expect(robot.client.state).not.toBe("open");
    expect(robot.leaseId).toBe(lease.lease_id); // still what the robot remembers

    control.restore();
    await until(() => robot.client.state === "open", "the robot to reconnect");
    // The welcome said "lease": null and the robot took its word.
    expect(robot.client.welcome?.lease).toBeNull();
    expect(robot.leaseId).toBeUndefined();
    expect(robot.mode).toBe("help");
    expect(robot.peerOpen).toBe(false);

    const before = robot.twistsAccepted;
    // The data channel: an operator end that kept sending.
    for (let i = 0; i < 4; i++) {
      channelTwist(lease.lease_id, 2_000_000 + i);
      await sleep(50);
    }
    // The bus: the server refuses to relay a twist on a lease it revoked.
    operator.send("twist", { lease_id: lease.lease_id, ...FORWARD });
    await until(() => errors.includes("not_authorized"), "the server to refuse the bus twist");
    await sleep(100);
    expect(robot.twistsAccepted).toEqual(before);
    expect(robot.velocity).toEqual(STOPPED);
  });
});
