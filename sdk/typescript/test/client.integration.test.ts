// FleetClient against the real fleet-server binary (started by
// test/support/clientCoreServer.globalSetup.ts).
import { afterEach, describe, expect, inject, it } from "vitest";
import {
  FleetClient,
  FleetClientError,
  MemoryTokenStore,
  type ConnectionState,
  type FleetClientOptions,
  type StateChange,
  type WebSocketConstructor,
} from "../src/index.js";
import { WebSocket as WsWebSocket } from "ws";

const server = inject("clientCoreServer");

// Node 20 has no global WebSocket; the client takes an injected constructor there.
const DefaultWebSocket: WebSocketConstructor =
  (globalThis as { WebSocket?: WebSocketConstructor }).WebSocket ?? (WsWebSocket as unknown as WebSocketConstructor);

/**
 * The `ws` package's WebSocket (exercising constructor injection on every Node
 * version), recording every socket and every frame the client sends.
 */
function recordingWebSocket() {
  const sockets: WsWebSocket[] = [];
  const sent: { type: string; payload: unknown }[] = [];
  class Recording extends WsWebSocket {
    constructor(url: string) {
      super(url);
      sockets.push(this);
    }
    override send(data: any, ...rest: any[]): void {
      sent.push(JSON.parse(String(data)));
      (super.send as (...a: unknown[]) => void)(data, ...rest);
    }
  }
  return { WebSocket: Recording as unknown as WebSocketConstructor, sockets, sent };
}

function waitForState(client: FleetClient, want: ConnectionState, timeoutMs = 5000): Promise<StateChange> {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      off();
      reject(new Error(`timed out waiting for state ${want}; now ${client.state}`));
    }, timeoutMs);
    const off = client.onState((c) => {
      if (c.state !== want) return;
      clearTimeout(timer);
      off();
      resolve(c);
    });
  });
}

const clients: FleetClient[] = [];
function makeClient(opts: Partial<FleetClientOptions> & Pick<FleetClientOptions, "kind">): FleetClient {
  const c = new FleetClient({
    url: server.wsUrl,
    WebSocket: DefaultWebSocket,
    reconnect: { initialDelayMs: 20, maxDelayMs: 200 },
    ...opts,
  });
  clients.push(c);
  return c;
}

afterEach(() => {
  for (const c of clients.splice(0)) c.close();
});

describe("FleetClient against fleet-server", () => {
  it("enrolls once, heartbeats, and reconnects with the same client_id after the socket is killed", async () => {
    const rec = recordingWebSocket();
    const store = new MemoryTokenStore();
    const client = makeClient({
      kind: "service",
      name: "client-core-test",
      enrollmentKey: server.enrollKey,
      tokenStore: store,
      agent: { name: "sdk-test", version: "0.0.0" },
      WebSocket: rec.WebSocket,
    });
    const states: ConnectionState[] = [];
    client.onState((c) => states.push(c.state));

    const welcome = await client.connect();
    expect(welcome.kind).toBe("service");
    expect(welcome.client_id).toMatch(/^s_/);
    expect(welcome.heartbeat_interval_ms).toBe(server.heartbeatIntervalMs);
    expect(store.load()?.client_id).toBe(welcome.client_id);
    expect(states).toEqual(["enrolling", "connecting", "open"]);

    // Outlast ~2.5 missed intervals several times over: only heartbeats keep us online.
    await new Promise((r) => setTimeout(r, server.heartbeatIntervalMs * 5));
    expect(client.state).toBe("open");
    expect(states).toEqual(["enrolling", "connecting", "open"]);
    expect(rec.sent.filter((m) => m.type === "heartbeat").length).toBeGreaterThanOrEqual(3);

    // Kill the live session socket out from under the client.
    const reopened = waitForState(client, "open");
    rec.sockets.at(-1)!.close();
    const again = await reopened;

    expect(again.welcome?.client_id).toBe(welcome.client_id);
    expect(client.clientId).toBe(welcome.client_id);
    expect(states).toContain("reconnecting");
    // Exactly one enroll ever; one hello per connection, all with the same token.
    expect(rec.sent.filter((m) => m.type === "enroll.request")).toHaveLength(1);
    const hellos = rec.sent.filter((m) => m.type === "hello") as { payload: { token: string } }[];
    expect(hellos).toHaveLength(2);
    expect(new Set(hellos.map((h) => h.payload.token)).size).toBe(1);
    expect(hellos[0]!.payload.token).toBe(store.load()!.token);
  });

  it("reuses stored credentials without an enrollment key", async () => {
    const store = new MemoryTokenStore();
    const first = makeClient({ kind: "robot", enrollmentKey: server.enrollKey, tokenStore: store });
    const w1 = await first.connect();
    expect(w1.client_id).toMatch(/^r_/);
    first.close();
    expect(first.state).toBe("closed");

    const rec = recordingWebSocket();
    const second = makeClient({ kind: "robot", tokenStore: store, WebSocket: rec.WebSocket });
    const w2 = await second.connect();
    expect(w2.client_id).toBe(w1.client_id);
    expect(rec.sent.map((m) => m.type)).not.toContain("enroll.request");
  });

  it("delivers typed inbound messages and sends typed envelopes", async () => {
    const client = makeClient({ kind: "service", enrollmentKey: server.enrollKey });
    await client.connect();
    const snapshot = new Promise((resolve) => client.on("snapshot", (env) => resolve(env.payload)));
    const seen: string[] = [];
    client.onMessage((env) => seen.push(env.type));
    const sent = client.send("subscribe", { topics: ["presence"] }, { id: "sub-1" });
    expect(sent).toMatchObject({ v: 0, type: "subscribe", id: "sub-1" });
    expect(await snapshot).toHaveProperty("robots");
    expect(seen).toContain("snapshot");
  });

  it("enrolls an operator with a single-use invite key", async () => {
    const res = await fetch(`${server.httpUrl}/api/admin/fleets/${server.fleet}/operator-invites`, {
      method: "POST",
      headers: { Authorization: `Bearer ${server.adminToken}` },
    });
    expect(res.status).toBe(200);
    const { key } = (await res.json()) as { key: string };

    const op = makeClient({ kind: "operator", name: "alice", enrollmentKey: key });
    const welcome = await op.connect();
    expect(welcome.kind).toBe("operator");
    expect(welcome.client_id).toMatch(/^o_/);

    // Single-use: a second redemption is refused and is terminal, not retried.
    const again = makeClient({ kind: "operator", enrollmentKey: key });
    await expect(again.connect()).rejects.toBeInstanceOf(FleetClientError);
    expect(again.state).toBe("closed");
  });

  it("treats a bad token as terminal instead of retrying", async () => {
    const store = new MemoryTokenStore({ token: "fp-tk-not-a-real-token", client_id: "s_x", fleet_id: "f_x" });
    const client = makeClient({ kind: "service", tokenStore: store });
    const err = await client.connect().then(
      () => null,
      (e: unknown) => e,
    );
    expect(err).toBeInstanceOf(FleetClientError);
    expect((err as FleetClientError).code).toBe("auth_failed");
    expect(client.state).toBe("closed");
  });

  it("stops for good when its token is revoked while connected", async () => {
    const rec = recordingWebSocket();
    const robot = makeClient({ kind: "robot", enrollmentKey: server.enrollKey, WebSocket: rec.WebSocket });
    const welcome = await robot.connect();
    const closed = waitForState(robot, "closed");
    const res = await fetch(`${server.httpUrl}/api/admin/fleets/${server.fleet}/clients/${welcome.client_id}/revoke`, {
      method: "POST",
      headers: { Authorization: `Bearer ${server.adminToken}` },
    });
    expect(res.status).toBe(200);
    expect(((await res.json()) as { disconnected: boolean }).disconnected).toBe(true);
    const change = await closed;
    expect(change.error?.code).toBe("auth_failed");
    await new Promise((r) => setTimeout(r, 300));
    expect(robot.state).toBe("closed");
    expect(rec.sent.filter((e) => e.type === "hello")).toHaveLength(1); // no reconnect loop
  });

  it("refuses send() while not open", () => {
    const client = makeClient({ kind: "service", enrollmentKey: server.enrollKey });
    expect(() => client.send("heartbeat", {})).toThrow(FleetClientError);
  });
});
