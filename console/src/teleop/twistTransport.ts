// Where the operator's twist goes for one lease: over a direct WebRTC data
// channel to the robot when one is up, over the control-plane bus otherwise.
// The contract both ends follow is protocol/README.md, "Teleop data plane".
//
// What this module guarantees:
//   - Every twist goes out on exactly one transport, chosen at the moment it
//     is sent. It starts on the bus, so driving works from the instant the
//     lease is granted, and moves to the data channel once the robot has
//     answered a ping on it.
//   - The data channel is unordered and never retransmits, so each twist
//     carries an increasing `seq` and the robot keeps only the newest.
//   - If the channel closes, the peer connection fails, or pings go
//     unanswered for PONG_TIMEOUT_MS, twist is back on the bus at once and a
//     fresh offer is made after a pause.
//
// It carries data only. Claim, steal and handback stay server actions; the
// caller closes this when the lease ends, whatever the reason.
//
// No DOM types here on purpose: the peer connection is injected (the browser's
// by default), so the same code runs under Node against werift in the sim's
// integration test.
import type { FleetClient, TwistPayload } from "@fleet-platform/sdk";

export const TWIST_CHANNEL_LABEL = "twist";
/** Liveness probe cadence on the open channel. */
export const PING_EVERY_MS = 250;
/** A ping unanswered this long means the direct link is dead. */
export const PONG_TIMEOUT_MS = 1000;
/** An offer that has not produced a live channel by now is abandoned. */
export const CONNECT_TIMEOUT_MS = 5000;
/** Pause before offering again; doubles per consecutive failure up to the max. */
export const RETRY_AFTER_MS = 3000;
export const RETRY_MAX_MS = 30_000;
/** A stop is the one twist that must not be lost: it is repeated on the lossy channel. */
const ZERO_REPEATS = 2;
const ZERO_REPEAT_MS = 100;

/** The setpoint without its lease: the transport stamps the lease (and `seq`). */
export type TwistSetpoint = Omit<TwistPayload, "lease_id">;

export interface IceServer {
  urls: string | string[];
  username?: string;
  credential?: string;
}

export interface TwistLinkStatus {
  /** The transport the next twist will use. */
  active: "bus" | "p2p";
  /**
   * The direct link: being negotiated, live, waiting to be offered again
   * after a failure, or not possible here (no WebRTC in this environment).
   */
  direct: "connecting" | "open" | "retrying" | "unavailable";
  /** Round trip of the last answered ping, while the direct link is live. */
  rttMs?: number;
}

export const BUS_ONLY: TwistLinkStatus = { active: "bus", direct: "unavailable" };

export interface TwistTransport {
  /** Sends one setpoint on the active transport. */
  send(cmd: TwistSetpoint): void;
  readonly status: TwistLinkStatus;
  /** Tears the peer connection down and stops everything. Idempotent. */
  close(): void;
}

// The slice of RTCPeerConnection / RTCDataChannel this module uses. The
// browser's classes and werift's both fit it.
export interface ChannelLike {
  readonly readyState: string;
  onopen: ((ev: never) => void) | null | undefined;
  onclose: ((ev: never) => void) | null | undefined;
  onmessage: ((ev: { data: unknown }) => void) | null | undefined;
  send(data: string): void;
  close(): void;
}

export interface CandidateLike {
  candidate: string;
  sdpMid?: string | null;
  sdpMLineIndex?: number | null;
}

export interface PeerLike {
  readonly connectionState: string;
  readonly localDescription?: { sdp: string } | null;
  onicecandidate: ((ev: { candidate?: CandidateLike | null }) => void) | null | undefined;
  onconnectionstatechange: ((ev: never) => void) | null | undefined;
  createDataChannel(label: string, options: { ordered: boolean; maxRetransmits: number }): ChannelLike;
  createOffer(): Promise<{ sdp?: string }>;
  setLocalDescription(desc: { type: "offer"; sdp: string }): Promise<unknown>;
  setRemoteDescription(desc: { type: "answer"; sdp: string }): Promise<unknown>;
  addIceCandidate(candidate: CandidateLike): Promise<unknown>;
  close(): unknown;
}

export type CreatePeer = (config: { iceServers: IceServer[] }) => PeerLike;

export interface TwistTransportOptions {
  /** The control connection: carries the signaling and receives the robot's answers. */
  client: Pick<FleetClient, "send" | "on">;
  robotId: string;
  leaseId: string;
  /** Sends one twist payload on the control-plane bus. */
  sendBus: (payload: TwistPayload) => void;
  onStatus?: (status: TwistLinkStatus) => void;
  /**
   * STUN/TURN servers. None are needed on loopback or one LAN; the server will
   * hand a list to clients later, and it goes in here.
   */
  iceServers?: IceServer[];
  /** Defaults to the browser's RTCPeerConnection; absent there means bus only. */
  createPeer?: CreatePeer;
}

interface Session {
  id: string;
  pc: PeerLike;
  dc: ChannelLike;
  offerSent: boolean;
  /** Our candidates gathered before the offer went out. */
  localIce: CandidateLike[];
  remoteSet: boolean;
  /** The robot's candidates that arrived before its answer was applied. */
  remoteIce: CandidateLike[];
  /** A pong has come back: the channel works in both directions. */
  live: boolean;
  /** When the oldest ping still unanswered was sent. */
  waitingSince: number | undefined;
  ping: ReturnType<typeof setInterval> | undefined;
  connect: ReturnType<typeof setTimeout> | undefined;
}

function browserPeer(): CreatePeer | undefined {
  const Ctor = (globalThis as { RTCPeerConnection?: new (config: unknown) => unknown }).RTCPeerConnection;
  return Ctor ? (config) => new Ctor(config) as PeerLike : undefined;
}

let sessions = 0;
let signals = 0;

export function openTwistTransport(o: TwistTransportOptions): TwistTransport {
  const createPeer = o.createPeer ?? browserPeer();
  const iceServers = o.iceServers ?? [];
  let status: TwistLinkStatus = createPeer ? { active: "bus", direct: "connecting" } : BUS_ONLY;
  let session: Session | undefined;
  let closed = false;
  let seq = 0;
  let failures = 0;
  let retry: ReturnType<typeof setTimeout> | undefined;
  let zero: ReturnType<typeof setTimeout> | undefined;

  const setStatus = (next: TwistLinkStatus) => {
    status = next;
    if (!closed) o.onStatus?.(next);
  };

  const signal = (kind: "offer" | "answer" | "ice", data: unknown) => {
    try {
      // The id keeps a refusal (robot offline: not_found) from being read as
      // the answer to anything else; the connect timeout handles it.
      o.client.send("signal", { to: o.robotId, kind, data }, { id: `p2p.signal.${++signals}` });
    } catch {
      /* control connection down: the attempt times out and is retried */
    }
  };

  const teardown = (s: Session) => {
    clearInterval(s.ping);
    clearTimeout(s.connect);
    s.dc.onopen = s.dc.onclose = s.dc.onmessage = null;
    s.pc.onicecandidate = s.pc.onconnectionstatechange = null;
    try {
      s.dc.close();
    } catch {
      /* already closed */
    }
    try {
      void Promise.resolve(s.pc.close()).catch(() => {});
    } catch {
      /* already closed */
    }
  };

  /** The direct link is gone (or never came up): bus now, offer again later. */
  const fail = (s: Session) => {
    if (session !== s) return;
    session = undefined;
    teardown(s);
    if (closed) return;
    setStatus({ active: "bus", direct: "retrying" });
    const wait = Math.min(RETRY_MAX_MS, RETRY_AFTER_MS * 2 ** failures);
    failures += 1;
    retry = setTimeout(start, wait);
  };

  const onChannelMessage = (s: Session, raw: unknown) => {
    if (session !== s || typeof raw !== "string") return;
    let msg: { pong?: unknown };
    try {
      msg = JSON.parse(raw) as { pong?: unknown };
    } catch {
      return;
    }
    if (msg === null || typeof msg !== "object" || typeof msg.pong !== "number") return;
    s.waitingSince = undefined;
    if (!s.live) {
      s.live = true;
      failures = 0;
      clearTimeout(s.connect);
    }
    setStatus({ active: "p2p", direct: "open", rttMs: Math.max(0, Date.now() - msg.pong) });
  };

  const pingTick = (s: Session) => {
    const now = Date.now();
    if (s.waitingSince !== undefined && now - s.waitingSince > PONG_TIMEOUT_MS && s.live) return fail(s);
    try {
      s.dc.send(JSON.stringify({ ping: now }));
      s.waitingSince ??= now;
    } catch {
      fail(s);
    }
  };

  function start(): void {
    if (closed || !createPeer) return;
    let pc: PeerLike;
    let dc: ChannelLike;
    try {
      pc = createPeer({ iceServers });
      dc = pc.createDataChannel(TWIST_CHANNEL_LABEL, { ordered: false, maxRetransmits: 0 });
    } catch {
      setStatus(BUS_ONLY);
      return;
    }
    const s: Session = {
      id: `${Date.now().toString(36)}-${(++sessions).toString(36)}-${Math.random().toString(36).slice(2, 10)}`,
      pc,
      dc,
      offerSent: false,
      localIce: [],
      remoteSet: false,
      remoteIce: [],
      live: false,
      waitingSince: undefined,
      ping: undefined,
      connect: setTimeout(() => fail(s), CONNECT_TIMEOUT_MS),
    };
    session = s;
    setStatus({ active: "bus", direct: "connecting" });

    pc.onicecandidate = (ev) => {
      const c = ev.candidate;
      if (!c || !c.candidate) return; // end of candidates is not signaled
      const candidate = { candidate: c.candidate, sdpMid: c.sdpMid ?? null, sdpMLineIndex: c.sdpMLineIndex ?? null };
      if (s.offerSent) signal("ice", { session: s.id, candidate });
      else s.localIce.push(candidate);
    };
    pc.onconnectionstatechange = () => {
      if (pc.connectionState === "failed" || pc.connectionState === "closed") fail(s);
    };
    dc.onopen = () => {
      pingTick(s);
      s.ping = setInterval(() => pingTick(s), PING_EVERY_MS);
    };
    dc.onclose = () => fail(s);
    dc.onmessage = (ev) => onChannelMessage(s, ev.data);

    pc.createOffer()
      .then(async (offer) => {
        if (!offer.sdp) throw new Error("empty offer");
        await pc.setLocalDescription({ type: "offer", sdp: offer.sdp });
        if (session !== s) return;
        signal("offer", { session: s.id, lease_id: o.leaseId, type: "offer", sdp: pc.localDescription?.sdp ?? offer.sdp });
        s.offerSent = true;
        for (const candidate of s.localIce.splice(0)) signal("ice", { session: s.id, candidate });
      })
      .catch(() => fail(s));
  }

  const offSignal = o.client.on("signal", ({ payload }) => {
    const s = session;
    // `from` is stamped by the server: only this lease's robot may answer.
    if (!s || payload.from !== o.robotId) return;
    const d = payload.data as { session?: unknown; sdp?: unknown; candidate?: unknown } | null;
    if (d === null || typeof d !== "object" || d.session !== s.id) return;
    if (payload.kind === "answer" && typeof d.sdp === "string" && !s.remoteSet) {
      s.remoteSet = true;
      s.pc
        .setRemoteDescription({ type: "answer", sdp: d.sdp })
        .then(() => {
          for (const c of s.remoteIce.splice(0)) s.pc.addIceCandidate(c).catch(() => {});
        })
        .catch(() => fail(s));
    } else if (payload.kind === "ice" && isCandidate(d.candidate)) {
      if (s.remoteSet) s.pc.addIceCandidate(d.candidate).catch(() => {});
      else s.remoteIce.push(d.candidate);
    }
  });

  /** One twist on exactly one transport. */
  const emit = (cmd: TwistSetpoint) => {
    const s = session;
    if (status.active === "p2p" && s && s.dc.readyState === "open") {
      try {
        s.dc.send(JSON.stringify({ lease_id: o.leaseId, seq: ++seq, linear: cmd.linear, angular: cmd.angular }));
        return;
      } catch {
        fail(s); // and send this one on the bus instead
      }
    }
    o.sendBus({ lease_id: o.leaseId, linear: cmd.linear, angular: cmd.angular });
  };

  const repeatZero = (cmd: TwistSetpoint, left: number) => {
    zero = setTimeout(() => {
      if (closed || status.active !== "p2p") return; // the bus delivers the one already sent
      emit(cmd);
      if (left > 1) repeatZero(cmd, left - 1);
    }, ZERO_REPEAT_MS);
  };

  start();

  return {
    send(cmd) {
      if (closed) return;
      clearTimeout(zero);
      emit(cmd);
      if (status.active === "p2p" && isZero(cmd)) repeatZero(cmd, ZERO_REPEATS);
    },
    get status() {
      return status;
    },
    close() {
      if (closed) return;
      closed = true;
      clearTimeout(retry);
      clearTimeout(zero);
      offSignal();
      const s = session;
      session = undefined;
      if (s) teardown(s);
      status = { active: "bus", direct: createPeer ? "retrying" : "unavailable" };
    },
  };
}

function isZero(cmd: TwistSetpoint): boolean {
  return cmd.linear.x_mps === 0 && (cmd.linear.y_mps ?? 0) === 0 && cmd.angular.z_radps === 0;
}

function isCandidate(c: unknown): c is CandidateLike {
  return c !== null && typeof c === "object" && typeof (c as CandidateLike).candidate === "string";
}
