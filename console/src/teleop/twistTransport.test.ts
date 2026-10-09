// The transport's switching rules against a scripted peer and fake time. The
// real thing (werift on both ends through fleet-server) is exercised in
// sim/test/p2p.integration.test.ts; this pins the rules that are hard to
// time there: one transport per twist, the stop repeated on the lossy channel,
// retry pacing, and that a closed transport stays silent.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { TwistPayload } from "@fleet-platform/sdk";
import pingFixture from "../../../protocol/fixtures/datachannel/ping.json";
import twistFixture from "../../../protocol/fixtures/datachannel/twist.json";
import {
  CONNECT_TIMEOUT_MS,
  PING_EVERY_MS,
  PONG_TIMEOUT_MS,
  RETRY_AFTER_MS,
  openTwistTransport,
  type ChannelLike,
  type PeerLike,
  type TwistLinkStatus,
  type TwistTransportOptions,
} from "./twistTransport";

const FORWARD = { linear: { x_mps: 0.5 }, angular: { z_radps: -0.3 } };
const STOP = { linear: { x_mps: 0 }, angular: { z_radps: 0 } };

class FakeChannel implements ChannelLike {
  readyState = "connecting";
  onopen: ChannelLike["onopen"] = null;
  onclose: ChannelLike["onclose"] = null;
  onmessage: ChannelLike["onmessage"] = null;
  sent: Record<string, unknown>[] = [];
  /** Answer pings as a robot would. */
  echo = true;
  constructor(
    readonly label: string,
    readonly options: { ordered: boolean; maxRetransmits: number },
  ) {}
  send(data: string): void {
    const msg = JSON.parse(data) as Record<string, unknown>;
    this.sent.push(msg);
    if (this.echo && typeof msg.ping === "number") this.onmessage?.({ data: JSON.stringify({ pong: msg.ping }) });
  }
  close(): void {
    this.readyState = "closed";
  }
  open(): void {
    this.readyState = "open";
    this.onopen?.(undefined as never);
  }
  die(): void {
    this.readyState = "closed";
    this.onclose?.(undefined as never);
  }
  twists(): Record<string, unknown>[] {
    return this.sent.filter((m) => "seq" in m);
  }
}

class FakePeer implements PeerLike {
  connectionState = "new";
  localDescription: { sdp: string } | null = null;
  onicecandidate: PeerLike["onicecandidate"] = null;
  onconnectionstatechange: PeerLike["onconnectionstatechange"] = null;
  channel: FakeChannel | undefined;
  closed = false;
  remote: string | undefined;
  createDataChannel(label: string, options: { ordered: boolean; maxRetransmits: number }): ChannelLike {
    this.channel = new FakeChannel(label, options);
    return this.channel;
  }
  async createOffer() {
    return { sdp: "v=0 offer" };
  }
  async setLocalDescription(desc: { sdp: string }) {
    this.localDescription = { sdp: desc.sdp };
  }
  async setRemoteDescription(desc: { sdp: string }) {
    this.remote = desc.sdp;
  }
  async addIceCandidate() {}
  close(): void {
    this.closed = true;
  }
}

type Signal = { to?: string; from?: string; kind: string; data: Record<string, unknown> };

function harness(extra: Partial<TwistTransportOptions> = {}) {
  const peers: FakePeer[] = [];
  const signals: Signal[] = [];
  const bus: TwistPayload[] = [];
  const statuses: TwistLinkStatus[] = [];
  let onSignal: ((e: { payload: Signal }) => void) | undefined;
  const client = {
    send: (_type: string, payload: Signal) => {
      signals.push(payload);
      return { v: 0, type: "signal", payload };
    },
    on: (_type: string, handler: (e: { payload: Signal }) => void) => {
      onSignal = handler;
      return () => (onSignal = undefined);
    },
  } as unknown as TwistTransportOptions["client"];
  const transport = openTwistTransport({
    client,
    robotId: "r_1",
    leaseId: "ls_7f8e9d0c",
    sendBus: (p) => bus.push(p),
    onStatus: (s) => statuses.push(s),
    createPeer: () => {
      const p = new FakePeer();
      peers.push(p);
      return p;
    },
    ...extra,
  });
  /** The robot answers the newest offer and the channel opens. */
  const connect = async () => {
    await vi.advanceTimersByTimeAsync(0);
    const offer = signals.filter((s) => s.kind === "offer").at(-1)!;
    onSignal?.({ payload: { from: "r_1", kind: "answer", data: { session: offer.data.session, type: "answer", sdp: "v=0 answer" } } });
    await vi.advanceTimersByTimeAsync(0);
    peers.at(-1)!.channel!.open();
  };
  return { transport, peers, signals, bus, statuses, connect, deliver: (s: Signal) => onSignal?.({ payload: s }), listening: () => onSignal !== undefined };
}

describe("twist transport", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(1_755_100_003_000);
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("offers an unordered, never-retransmitted `twist` channel for the lease, and drives on the bus meanwhile", async () => {
    const h = harness();
    expect(h.transport.status).toEqual({ active: "bus", direct: "connecting" });
    await vi.advanceTimersByTimeAsync(0);
    const ch = h.peers[0]!.channel!;
    expect(ch.label).toBe("twist");
    expect(ch.options).toEqual({ ordered: false, maxRetransmits: 0 });
    expect(h.signals).toHaveLength(1);
    expect(h.signals[0]).toMatchObject({ to: "r_1", kind: "offer", data: { lease_id: "ls_7f8e9d0c", type: "offer", sdp: "v=0 offer" } });
    expect(typeof h.signals[0]!.data.session).toBe("string");

    h.transport.send(FORWARD);
    expect(h.bus).toEqual([{ lease_id: "ls_7f8e9d0c", ...FORWARD }]);
  });

  it("moves to the channel on the first pong; each twist then goes on the channel only, in the fixture's shape", async () => {
    const h = harness();
    await h.connect();
    const ch = h.peers[0]!.channel!;
    expect(ch.sent[0]).toEqual({ ping: Date.now() });
    expect(Object.keys(ch.sent[0]!)).toEqual(Object.keys(pingFixture));
    expect(h.transport.status).toEqual({ active: "p2p", direct: "open", rttMs: 0 });

    h.transport.send(FORWARD);
    h.transport.send(FORWARD);
    expect(h.bus).toHaveLength(0);
    expect(ch.twists()).toEqual([
      { ...twistFixture, seq: 1 },
      { ...twistFixture, seq: 2 },
    ]);
  });

  it("stays on the bus while the channel is open but no pong has come back", async () => {
    const h = harness();
    await vi.advanceTimersByTimeAsync(0);
    h.peers[0]!.channel!.echo = false;
    await h.connect();
    h.transport.send(FORWARD);
    expect(h.transport.status.active).toBe("bus");
    expect(h.bus).toHaveLength(1);
    expect(h.peers[0]!.channel!.twists()).toHaveLength(0);
    // Never live: the attempt is abandoned at the connect timeout.
    await vi.advanceTimersByTimeAsync(CONNECT_TIMEOUT_MS);
    expect(h.peers[0]!.closed).toBe(true);
    expect(h.transport.status).toEqual({ active: "bus", direct: "retrying" });
  });

  it("ignores an answer from anyone but the leased robot, or for another session", async () => {
    const h = harness();
    await vi.advanceTimersByTimeAsync(0);
    const session = h.signals[0]!.data.session;
    h.deliver({ from: "r_other", kind: "answer", data: { session, type: "answer", sdp: "v=0 evil" } });
    h.deliver({ from: "r_1", kind: "answer", data: { session: "stale", type: "answer", sdp: "v=0 stale" } });
    await vi.advanceTimersByTimeAsync(0);
    expect(h.peers[0]!.remote).toBeUndefined();
  });

  it("repeats a stop on the channel, and a newer twist cancels the repeats", async () => {
    const h = harness();
    await h.connect();
    const ch = h.peers[0]!.channel!;
    h.transport.send(STOP);
    await vi.advanceTimersByTimeAsync(350);
    expect(ch.twists().map((t) => t.seq)).toEqual([1, 2, 3]);
    expect(ch.twists().every((t) => (t.linear as { x_mps: number }).x_mps === 0)).toBe(true);

    h.transport.send(STOP);
    h.transport.send(FORWARD);
    await vi.advanceTimersByTimeAsync(350);
    expect(ch.twists().map((t) => t.seq)).toEqual([1, 2, 3, 4, 5]);
    expect(h.bus).toHaveLength(0);
  });

  it("falls back to the bus the moment the channel closes, then offers again after the pause", async () => {
    const h = harness();
    await h.connect();
    h.transport.send(FORWARD);
    expect(h.peers[0]!.channel!.twists()).toHaveLength(1);
    h.peers[0]!.channel!.die();
    expect(h.transport.status).toEqual({ active: "bus", direct: "retrying" });
    expect(h.peers[0]!.closed).toBe(true);
    h.transport.send(FORWARD);
    expect(h.bus).toHaveLength(1);

    await vi.advanceTimersByTimeAsync(RETRY_AFTER_MS - 1);
    expect(h.peers).toHaveLength(1);
    await vi.advanceTimersByTimeAsync(1);
    expect(h.peers).toHaveLength(2);
    await h.connect();
    expect(h.transport.status.active).toBe("p2p");
    h.transport.send(FORWARD);
    expect(h.bus).toHaveLength(1);
    // seq keeps climbing across sessions (the bus twist took none).
    expect(h.peers[1]!.channel!.twists()).toEqual([{ ...twistFixture, seq: 2 }]);
    expect(h.signals.filter((s) => s.kind === "offer")[1]!.data.session).not.toBe(h.signals[0]!.data.session);
  });

  it("falls back when pings go unanswered, within the pong timeout plus one ping interval", async () => {
    const h = harness();
    await h.connect();
    const ch = h.peers[0]!.channel!;
    await vi.advanceTimersByTimeAsync(1000);
    expect(h.transport.status.active).toBe("p2p");
    ch.echo = false;
    await vi.advanceTimersByTimeAsync(PONG_TIMEOUT_MS + 2 * PING_EVERY_MS);
    expect(h.transport.status).toEqual({ active: "bus", direct: "retrying" });
    h.transport.send(FORWARD);
    expect(h.bus).toHaveLength(1);
    expect(ch.twists()).toHaveLength(0);
  });

  it("falls back when the peer connection reports failed", async () => {
    const h = harness();
    await h.connect();
    h.peers[0]!.connectionState = "failed";
    h.peers[0]!.onconnectionstatechange?.(undefined as never);
    expect(h.transport.status.active).toBe("bus");
  });

  it("is silent once closed: no twist, no retry, no status, no listener", async () => {
    const h = harness();
    await h.connect();
    const seen = h.statuses.length;
    h.transport.close();
    expect(h.peers[0]!.closed).toBe(true);
    expect(h.listening()).toBe(false);
    h.transport.send(FORWARD);
    await vi.advanceTimersByTimeAsync(60_000);
    expect(h.bus).toHaveLength(0);
    expect(h.peers).toHaveLength(1);
    expect(h.statuses).toHaveLength(seen);
    expect(h.peers[0]!.channel!.twists()).toHaveLength(0);
  });

  it("is bus only where there is no WebRTC", () => {
    // No createPeer given and no RTCPeerConnection in this environment.
    const bus: TwistPayload[] = [];
    const t = openTwistTransport({
      client: { send: () => undefined, on: () => () => {} } as unknown as TwistTransportOptions["client"],
      robotId: "r_1",
      leaseId: "ls_1",
      sendBus: (p) => bus.push(p),
    });
    expect(t.status).toEqual({ active: "bus", direct: "unavailable" });
    t.send(FORWARD);
    expect(bus).toHaveLength(1);
  });
});
