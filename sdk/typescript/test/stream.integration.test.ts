// Snapshot-then-stream, typed events and channels against the real fleet-server
// (started by test/support/clientCoreServer.globalSetup.ts). The first test is
// the command-brain half of the server's TestIntegrationStoryline
// (server/internal/app/integration_test.go), driven only through the SDK.
import { afterEach, describe, expect, inject, it } from "vitest";
import {
  FleetClient,
  FleetClientError,
  type ChannelMessage,
  type ConnectionState,
  type Envelope,
  type FleetClientOptions,
  type FleetEvent,
  type OperatorPresenceEvent,
  type PresenceEvent,
  type SnapshotPayload,
  type StateChange,
  type TelemetryEvent,
  type Topic,
  type WebSocketConstructor,
} from "../src/index.js";
import { WebSocket as WsWebSocket } from "ws";

const server = inject("clientCoreServer");

/** `ws` constructor that records every socket, so a test can kill the live one. */
function recordingWebSocket() {
  const sockets: WsWebSocket[] = [];
  class Recording extends WsWebSocket {
    constructor(url: string) {
      super(url);
      sockets.push(this);
    }
  }
  return { WebSocket: Recording as unknown as WebSocketConstructor, sockets };
}

const clients: FleetClient[] = [];
function makeClient(opts: Partial<FleetClientOptions> & Pick<FleetClientOptions, "kind">): FleetClient {
  const c = new FleetClient({
    url: server.wsUrl,
    WebSocket: WsWebSocket as unknown as WebSocketConstructor,
    reconnect: { initialDelayMs: 20, maxDelayMs: 200 },
    ...opts,
  });
  clients.push(c);
  return c;
}

afterEach(() => {
  for (const c of clients.splice(0)) c.close();
});

/** Resolves with the next value a callback-style subscription delivers that matches `pred`. */
function next<T>(
  register: (h: (v: T) => void) => () => void,
  pred: (v: T) => boolean = () => true,
  what = "value",
  timeoutMs = 5000,
): Promise<T> {
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

const waitState = (c: FleetClient, want: ConnectionState) =>
  next<StateChange>((h) => c.onState(h), (s) => s.state === want, `state ${want}`);

async function mintOperatorInvite(): Promise<string> {
  const res = await fetch(`${server.httpUrl}/api/admin/fleets/${server.fleet}/operator-invites`, {
    method: "POST",
    headers: { Authorization: `Bearer ${server.adminToken}` },
  });
  expect(res.status).toBe(200);
  return ((await res.json()) as { key: string }).key;
}

/** Subscribes until the snapshot shows `robotId` online with a manifest (manifest lands asynchronously). */
async function subscribeUntilOnline(c: FleetClient, robotId: string): Promise<SnapshotPayload> {
  const deadline = Date.now() + 3000;
  for (;;) {
    const snap = await c.subscribe(["presence", "events", "telemetry"]);
    const r = snap.robots.find((x) => x.robot_id === robotId);
    if (r?.presence === "online" && r.manifest) return snap;
    if (Date.now() > deadline) throw new Error(`snapshot never showed ${robotId} online: ${JSON.stringify(snap)}`);
    await new Promise((res) => setTimeout(res, 20));
  }
}

interface Assignment {
  seq: number;
  order_id?: number;
  ack?: number;
}

describe("subscribe, events and channels against fleet-server", () => {
  it("runs the brain half of the storyline through the SDK", async () => {
    // Robot: enrolls, declares a manifest that speaks the assignment channel,
    // and acks every assignment back to its sender.
    const robot = makeClient({ kind: "robot", name: "sim-01", enrollmentKey: server.enrollKey });
    const robotId = (await robot.connect()).client_id;
    robot.send("manifest", {
      drive: { type: "twist", max_v_mps: 1.5, max_w_radps: 2 },
      battery: {},
      channels: ["assignment"],
    });
    const robotAssignments = robot.channel<Assignment>("assignment");
    robotAssignments.onMessage((m) => robotAssignments.publish({ seq: m.data.seq, ack: m.data.seq }, { to: m.from }));

    // Brain: subscribe → snapshot lists the robot online, AUTONOMOUS, with its manifest.
    const brain = makeClient({ kind: "service", name: "brain", enrollmentKey: server.enrollKey });
    await brain.connect();
    const snapshots: SnapshotPayload[] = [];
    brain.onSnapshot((s) => snapshots.push(s));
    const snap = await subscribeUntilOnline(brain, robotId);
    const summary = snap.robots.find((r) => r.robot_id === robotId)!;
    expect(summary.state).toBe("AUTONOMOUS");
    expect(summary.manifest?.channels).toEqual(["assignment"]);
    expect(brain.snapshot).toBe(snap);
    expect(snapshots.at(-1)).toBe(snap);
    expect(brain.topics).toEqual(["presence", "events", "telemetry"]);

    // Telemetry arrives as a typed event carrying the robot's payload.
    const telemetry = next<TelemetryEvent>((h) => brain.onTelemetry(h), (e) => e.robot_id === robotId, "telemetry");
    robot.send("telemetry", { pose: { frame: "local", frame_id: "map", x_m: 1, y_m: 2 }, battery: { pct: 80 } });
    const tel = await telemetry;
    expect(tel.event).toBe("robot.telemetry");
    expect(tel.data.pose).toEqual({ frame: "local", frame_id: "map", x_m: 1, y_m: 2 });
    expect(tel.data.battery?.pct).toBe(80);

    // Help request → robot.help_requested with the queue entry: the request
    // plus the server's requested_at_ms.
    const help = next<FleetEvent<"robot.help_requested">>((h) => brain.onEvent("robot.help_requested", h), (e) => e.robot_id === robotId, "help");
    const askedAfter = Date.now();
    robot.send("help.request", { reason: "nav_goal_failed", context: { attempts: 3 } });
    const asked = (await help).data;
    expect(asked.reason).toBe("nav_goal_failed");
    expect(asked.context).toEqual({ attempts: 3 });
    expect(Number.isInteger(asked.requested_at_ms)).toBe(true);
    expect(asked.requested_at_ms).toBeGreaterThanOrEqual(askedAfter);
    expect(asked.requested_at_ms).toBeLessThanOrEqual(Date.now());

    // A fresh snapshot carries the same entry on the HELP_REQUESTED robot.
    const queued = (await brain.subscribe(["events"])).robots.find((r) => r.robot_id === robotId)!;
    expect(queued.state).toBe("HELP_REQUESTED");
    expect(queued.help).toEqual(asked);

    // Operator takes over: lease.granted reaches operator and robot (direct
    // messages via on()), the brain sees robot.lease_granted, twist reaches the robot.
    const operator = makeClient({ kind: "operator", name: "op-1", enrollmentKey: await mintOperatorInvite() });
    await operator.connect();
    const opGrant = next<Envelope<"lease.granted">>((h) => operator.on("lease.granted", h), undefined, "operator lease.granted");
    const robotGrant = next<Envelope<"lease.granted">>((h) => robot.on("lease.granted", h), undefined, "robot lease.granted");
    const brainGrant = next<FleetEvent<"robot.lease_granted">>((h) => brain.onEvent("robot.lease_granted", h), undefined, "robot.lease_granted");
    operator.send("lease.claim", { robot_id: robotId });
    const lease = (await opGrant).payload;
    expect((await robotGrant).payload.lease_id).toBe(lease.lease_id);
    const granted = await brainGrant;
    expect(granted.robot_id).toBe(robotId);
    expect(granted.data.lease_id).toBe(lease.lease_id);
    expect(granted.data.operator_id).toBe(operator.clientId);

    const twist = next<Envelope<"twist">>((h) => robot.on("twist", h), undefined, "twist");
    operator.send("twist", { lease_id: lease.lease_id, linear: { x_mps: 0.5 }, angular: { z_radps: -0.2 } });
    expect((await twist).payload).toMatchObject({ lease_id: lease.lease_id, linear: { x_mps: 0.5 } });

    // Handback: robot hears lease.revoked(released), brain sees robot.lease_released.
    const revoked = next<Envelope<"lease.revoked">>((h) => robot.on("lease.revoked", h), undefined, "lease.revoked");
    const released = next<FleetEvent<"robot.lease_released">>((h) => brain.onEvent("robot.lease_released", h), undefined, "robot.lease_released");
    operator.send("lease.release", { lease_id: lease.lease_id, resolution: "resolved" });
    expect((await revoked).payload.reason).toBe("released");
    expect((await released).data).toMatchObject({ lease_id: lease.lease_id, reason: "released" });

    // Channels: directed assignment, the robot's ack comes back to the brain.
    const assignments = brain.channel<Assignment>("assignment");
    const ack = next<ChannelMessage<Assignment>>((h) => assignments.onMessage(h), (m) => m.data.ack === 1, "ack");
    assignments.publish({ seq: 1, order_id: 17 }, { to: robotId });
    const ackMsg = await ack;
    expect(ackMsg.from).toBe(robotId);
    expect(ackMsg.channel).toBe("assignment");

    // Broadcast on a channel the brain subscribes to.
    const edges = brain.channel<{ edge: string; seconds: number }>("edge_report");
    await edges.subscribe();
    expect(brain.topics).toContain("channel:edge_report");
    const edge = next<ChannelMessage<{ edge: string; seconds: number }>>((h) => edges.onMessage(h), undefined, "edge_report");
    robot.channel("edge_report").publish({ edge: "e12", seconds: 41.5 }, { broadcast: true });
    expect(await edge).toMatchObject({ from: robotId, data: { edge: "e12", seconds: 41.5 } });

    // A directed publish to nobody is refused with not_found, correlated by id.
    const notFound = next<Envelope<"error">>((h) => brain.on("error", h), (e) => e.payload.ref === "pub-x", "not_found");
    assignments.publish({ seq: 2 }, { to: "r_nobody" }, { id: "pub-x" });
    expect((await notFound).payload.code).toBe("not_found");

    // Robot drops → presence subscribers see robot.offline.
    const offline = next<PresenceEvent>((h) => brain.onPresence(h), (e) => e.event === "robot.offline", "offline");
    robot.close();
    expect((await offline).robot_id).toBe(robotId);
  });

  it("lists operators in the snapshot and delivers operator presence as typed events", async () => {
    const ada = makeClient({ kind: "operator", name: "ada", enrollmentKey: await mintOperatorInvite() });
    const adaId = (await ada.connect()).client_id;
    const snap = await ada.subscribe(["presence"]);
    expect(snap.operators).toContainEqual({ operator_id: adaId, name: "ada", online: true });

    // A wildcard handler sees the operator event too, with robot_id "".
    const all: FleetEvent[] = [];
    ada.onEvent((e) => all.push(e));

    const online = next<FleetEvent<"operator.online">>((h) => ada.onEvent("operator.online", h), undefined, "operator.online");
    const bo = makeClient({ kind: "operator", name: "bo", enrollmentKey: await mintOperatorInvite() });
    const boId = (await bo.connect()).client_id;
    const on = await online;
    expect(on).toMatchObject({
      event: "operator.online",
      operator_id: boId,
      robot_id: "",
      data: { operator_id: boId, name: "bo", online: true },
    });
    expect(all.filter((e) => e.event === "operator.online" && e.operator_id === boId)).toHaveLength(1);

    // The late operator finds both in its snapshot.
    const late = await bo.subscribe(["presence"]);
    expect(late.operators).toEqual(
      expect.arrayContaining([
        { operator_id: adaId, name: "ada", online: true },
        { operator_id: boId, name: "bo", online: true },
      ]),
    );

    const offline = next<OperatorPresenceEvent>(
      (h) => ada.onEvent("operator.offline", h),
      (e) => e.operator_id === boId,
      "operator.offline",
    );
    bo.close();
    expect((await offline).data).toEqual({ operator_id: boId, name: "bo", online: false });
  });

  it("re-subscribes after a reconnect and re-delivers a fresh snapshot before further events", async () => {
    const robot = makeClient({ kind: "robot", enrollmentKey: server.enrollKey });
    const robotId = (await robot.connect()).client_id;
    robot.send("manifest", { battery: {} });

    const rec = recordingWebSocket();
    const brain = makeClient({ kind: "service", enrollmentKey: server.enrollKey, WebSocket: rec.WebSocket });

    // One ordered log of everything the stream layer delivers, plus opens.
    const log: string[] = [];
    brain.onState((s) => s.state === "open" && log.push("open"));
    brain.onSnapshot(() => log.push("snapshot"));
    brain.onEvent((e: FleetEvent) => log.push(`event:${e.event}`));

    // Subscribing before connect() resolves with the first connection's snapshot.
    const first = brain.subscribe(["presence", "telemetry"]);
    await brain.connect();
    expect((await first).robots.map((r) => r.robot_id)).toContain(robotId);
    expect(log.slice(0, 2)).toEqual(["open", "snapshot"]);

    // Keep telemetry flowing throughout, so events race the reconnect.
    const pump = setInterval(() => {
      if (robot.state === "open") robot.send("telemetry", { battery: { pct: 50 } });
    }, 10);
    try {
      await next<TelemetryEvent>((h) => brain.onTelemetry(h), undefined, "telemetry before reconnect");

      // Kill the brain's live socket; the client reconnects and re-subscribes by itself.
      const snapshotsBefore = log.filter((l) => l === "snapshot").length;
      const reopened = waitState(brain, "open");
      const fresh = next<SnapshotPayload>((h) => brain.onSnapshot(h), undefined, "fresh snapshot");
      rec.sockets.at(-1)!.close();
      await reopened;
      const freshSnap = await fresh;
      expect(freshSnap.robots.find((r) => r.robot_id === robotId)?.presence).toBe("online");
      expect(brain.snapshot).toBe(freshSnap);

      // Events flow again on the new connection.
      await next<TelemetryEvent>((h) => brain.onTelemetry(h), undefined, "telemetry after reconnect");

      // Ordering: after the reconnect's "open", the first stream delivery is the
      // fresh snapshot; no event slipped in ahead of it.
      const reopenAt = log.lastIndexOf("open");
      expect(log.filter((l) => l === "snapshot").length).toBe(snapshotsBefore + 1);
      const afterReopen = log.slice(reopenAt + 1);
      expect(afterReopen.find((l) => l === "snapshot" || l.startsWith("event:"))).toBe("snapshot");
      expect(afterReopen).toContain("event:robot.telemetry");

      // The resubscribe carried every remembered topic, once.
      expect(brain.topics).toEqual(["presence", "telemetry"]);
    } finally {
      clearInterval(pump);
    }
  });

  it("rejects pending subscribes when the client closes and validates channel names", async () => {
    const c = makeClient({ kind: "service", enrollmentKey: server.enrollKey });
    const pending = c.subscribe(["presence"]); // never connected
    c.close();
    await expect(pending).rejects.toBeInstanceOf(FleetClientError);
    await expect(c.subscribe(["presence"])).rejects.toBeInstanceOf(FleetClientError);
    expect(() => c.channel("Not A Channel")).toThrow(TypeError);
    expect(c.channel("edge_report").topic).toBe("channel:edge_report");
  });

  it("rejects a subscribe the server refuses and keeps streaming", async () => {
    const robot = makeClient({ kind: "robot", enrollmentKey: server.enrollKey });
    await robot.connect();
    const c = makeClient({ kind: "service", enrollmentKey: server.enrollKey });
    await c.connect();
    await c.subscribe(["telemetry"]);
    // A malformed topic list: the server answers error{invalid_message, ref: <subscribe id>}.
    const refused = c.subscribe([42 as unknown as Topic]).then(
      () => null,
      (e: unknown) => e,
    );
    const err = await refused;
    expect(err).toBeInstanceOf(FleetClientError);
    expect((err as FleetClientError).code).toBe("invalid_message");
    expect(c.topics).toEqual(["telemetry"]);
    // Nothing stays held back behind the refused subscribe.
    const tel = next<TelemetryEvent>((h) => c.onTelemetry(h), undefined, "telemetry");
    robot.send("telemetry", { battery: { pct: 1 } });
    expect((await tel).data.battery?.pct).toBe(1);
  });

  it("refuses a plain claim on a held lease without closing the client; steal takes it", async () => {
    const robot = makeClient({ kind: "robot", name: "sim-claim", enrollmentKey: server.enrollKey });
    const robotId = (await robot.connect()).client_id;
    const ada = makeClient({ kind: "operator", name: "ada", enrollmentKey: await mintOperatorInvite() });
    await ada.connect();
    const rec = recordingWebSocket();
    const bob = makeClient({
      kind: "operator",
      name: "bob",
      enrollmentKey: await mintOperatorInvite(),
      WebSocket: rec.WebSocket,
    });
    await bob.connect();

    const adaGrant = next<Envelope<"lease.granted">>((h) => ada.on("lease.granted", h), undefined, "ada lease.granted");
    ada.send("lease.claim", { robot_id: robotId });
    const adaLease = (await adaGrant).payload;

    // Bob's plain claim is refused with a conflict that names ada's lease. The
    // claim carries no id, so the reply has no ref: the lease is what tells
    // the client this conflict is a reply and not the server closing it.
    const refused = next<Envelope<"error">>((h) => bob.on("error", h), undefined, "bob's conflict");
    bob.send("lease.claim", { robot_id: robotId });
    const conflict = (await refused).payload;
    expect(conflict.code).toBe("conflict");
    expect(conflict.ref).toBeUndefined();
    expect(conflict.lease).toEqual(adaLease);
    expect(bob.state).toBe("open");

    // Had the refusal been taken for a takeover notice, this drop would close
    // bob for good. It must reconnect like any other network drop.
    const reopened = waitState(bob, "open");
    rec.sockets.at(-1)!.close();
    await reopened;

    // With steal, bob takes the wheel and ada is told her lease was stolen.
    const stolen = next<Envelope<"lease.revoked">>((h) => ada.on("lease.revoked", h), undefined, "ada lease.revoked");
    const bobGrant = next<Envelope<"lease.granted">>((h) => bob.on("lease.granted", h), undefined, "bob lease.granted");
    bob.send("lease.claim", { robot_id: robotId, steal: true });
    const bobLease = (await bobGrant).payload;
    expect(bobLease.operator_id).toBe(bob.clientId);
    expect(bobLease.lease_id).not.toBe(adaLease.lease_id);
    expect((await stolen).payload).toEqual({ lease_id: adaLease.lease_id, robot_id: robotId, reason: "stolen" });
  });
});
