// The robot's half of the teleop data plane (protocol/README.md, "Teleop data
// plane"): it answers the WebRTC offer of the operator who holds its lease and
// takes twist from the `twist` data channel.
//
// Authority does not move here. The robot answers only the operator named in
// its current lease.granted (the server stamps `from` on every signal), every
// twist on the channel still has to bear the current lease, and the peer is
// closed the moment the lease ends. The channel is data; the lease is still
// granted and revoked by the server alone.
//
// The peer connection's ICE servers are the installation's, asked of the
// server (protocol/README.md, "ICE servers"): once when a lease is taken, so
// the answer is there by the time the offer comes, and kept for that lease
// until the TURN credential in it expires.
//
// Node has no WebRTC of its own; werift is a pure TypeScript implementation.

import type { FleetClient, SignalPayload, TwistPayload } from "@fleet-platform/sdk";
import { RTCPeerConnection, type RTCDataChannel, type RTCIceServer } from "werift";

export const TWIST_CHANNEL_LABEL = "twist";
/**
 * A stored ICE answer is not used for a new peer this close to its expiry:
 * the TURN server checks the credential when the relay is allocated, a moment
 * after the peer connection is created.
 */
export const ICE_EXPIRY_MARGIN_MS = 10_000;
/** Anything larger on the channel is not a twist or a ping. */
const MAX_MESSAGE_BYTES = 1024;

export interface TwistAnswererOptions {
  /** The robot's control connection right now (it is replaced on re-enrollment). */
  client: () => FleetClient;
  /**
   * A fixed STUN/TURN list, for tests and odd setups. When it is given (an
   * empty list included) the server is never asked and every peer gets
   * exactly this. Left out, which is the normal case, the robot asks the
   * server and uses what the installation is configured with.
   */
  iceServers?: RTCIceServer[];
  /** The clock `expires_at_ms` is compared with. Default Date.now. */
  now?: () => number;
  /**
   * A twist that arrived on the data channel and is newer than the last one
   * obeyed. Returns whether the robot obeyed it (it bore the current lease).
   */
  onTwist: (twist: TwistPayload) => boolean;
  log?: (line: string) => void;
}

interface Session {
  id: string;
  operatorId: string;
  pc: RTCPeerConnection;
  answerSent: boolean;
  /** Our candidates gathered before the answer went out. */
  localIce: unknown[];
  remoteSet: boolean;
  /** The operator's candidates that arrived before the offer was applied. */
  remoteIce: IceInit[];
  channel: RTCDataChannel | undefined;
  /** Highest `seq` of a twist obeyed from this channel; anything at or below it is stale. */
  lastSeq: number;
}

interface IceInit {
  candidate: string;
  sdpMid?: string;
  sdpMLineIndex?: number;
}

/** An offer taken but not yet answered with a peer: its ICE servers are still being asked for. */
interface PendingOffer {
  id: string;
  /** The operator's candidates that arrive meanwhile; the session inherits them. */
  remoteIce: IceInit[];
}

/** The server's ICE answer for one lease. `got` settles with undefined when there was none. */
interface LeaseIce {
  leaseId: string;
  got: Promise<{ servers: RTCIceServer[]; expiresAtMs: number | undefined } | undefined>;
}

/**
 * Keeps a peer connection from asking a STUN server nobody configured.
 *
 * werift 0.25 falls back to a public STUN server (stun.l.google.com) whenever
 * the list it is given names no STUN server, the empty list included, and it
 * has no setting to turn that off. Neither side may assume a default public
 * STUN server (protocol/README.md, "Signaling"), and a robot on a closed
 * network must send nothing off it, so the fallback is removed from the
 * connection's transports. Call it after the transports exist (the remote
 * offer applied, or the local offer created) and before setLocalDescription,
 * which is what starts gathering.
 */
export function dropDefaultStun(pc: RTCPeerConnection, iceServers: readonly RTCIceServer[]): void {
  const configured = iceServers.some((s) => [s.urls].flat().some((u) => /^stuns?:/i.test(u.trim())));
  if (configured) return;
  for (const t of pc.iceTransports) (t.connection as { stunServer?: unknown }).stunServer = undefined;
}

export class TwistAnswerer {
  readonly #o: TwistAnswererOptions;
  #lease: { leaseId: string; operatorId: string } | undefined;
  #session: Session | undefined;
  #pending: PendingOffer | undefined;
  #ice: LeaseIce | undefined;
  #lastIceServers: RTCIceServer[] | undefined;
  #iceRequests = 0;
  #signals = 0;

  constructor(opts: TwistAnswererOptions) {
    this.#o = opts;
  }

  /** Whether a twist data channel is open right now. */
  get open(): boolean {
    return this.#session?.channel?.readyState === "open";
  }

  /** The ICE servers the newest peer connection was created with; undefined before the first. */
  get iceServers(): readonly RTCIceServer[] | undefined {
    return this.#lastIceServers;
  }

  /** How many times the server has been asked for ICE servers. */
  get iceRequests(): number {
    return this.#iceRequests;
  }

  /**
   * The robot holds this lease (lease.granted, or the welcome of a connection
   * that states it): only its operator may connect. A peer from an earlier
   * lease is closed. For a lease that is new to the robot the ICE servers are
   * asked for now, ahead of the offer; a renewal asks for nothing.
   */
  grant(leaseId: string, operatorId: string): void {
    if (this.#lease?.leaseId !== leaseId) {
      this.closePeer();
      this.#ice = undefined;
      if (!this.#o.iceServers) this.#askIce(leaseId);
    }
    this.#lease = { leaseId, operatorId };
  }

  /** The lease is over (released, stolen, expired, operator lost): close the peer. */
  revoke(): void {
    this.#lease = undefined;
    this.#ice = undefined; // the answer belonged to the lease
    this.closePeer();
  }

  /** Closes the peer connection, if any, and keeps the lease: the operator may offer again. */
  closePeer(): void {
    this.#pending = undefined; // an offer still waiting for its ICE servers is abandoned
    const s = this.#session;
    if (!s) return;
    this.#session = undefined;
    try {
      s.channel?.close();
    } catch {
      /* already closed */
    }
    s.pc.close().catch(() => {});
  }

  /** A `signal` relayed by the server. Anything not from this lease's operator is ignored. */
  onSignal(sig: SignalPayload): void {
    const lease = this.#lease;
    if (!lease || sig.from !== lease.operatorId) return;
    const d = sig.data as { session?: unknown; lease_id?: unknown; sdp?: unknown; candidate?: unknown } | null;
    if (d === null || typeof d !== "object" || typeof d.session !== "string") return;

    if (sig.kind === "offer") {
      if (d.lease_id !== lease.leaseId || typeof d.sdp !== "string") return;
      void this.#answer(d.session, lease, d.sdp);
      return;
    }
    if (sig.kind !== "ice") return;
    const s = this.#session?.id === d.session ? this.#session : undefined;
    const waiting = this.#pending?.id === d.session ? this.#pending : undefined;
    if (!s && !waiting) return;
    const c = d.candidate as { candidate?: unknown; sdpMid?: unknown; sdpMLineIndex?: unknown } | null;
    if (c === null || typeof c !== "object" || typeof c.candidate !== "string" || c.candidate === "") return;
    const init: IceInit = {
      candidate: c.candidate,
      ...(typeof c.sdpMid === "string" ? { sdpMid: c.sdpMid } : {}),
      ...(typeof c.sdpMLineIndex === "number" ? { sdpMLineIndex: c.sdpMLineIndex } : {}),
    };
    if (s?.remoteSet) s.pc.addIceCandidate(init).catch(() => {});
    else (s ?? waiting!).remoteIce.push(init);
  }

  /** Asks the server for this lease's ICE servers and keeps the answer while it is the lease's. */
  #askIce(leaseId: string): LeaseIce {
    this.#iceRequests += 1;
    let client: FleetClient | undefined;
    try {
      client = this.#o.client();
    } catch {
      /* no connection object: the same as no answer */
    }
    const entry: LeaseIce = {
      leaseId,
      got: (client ? client.iceConfig() : Promise.reject(new Error("no client"))).then(
        (c) => ({ servers: c.ice_servers, expiresAtMs: c.expires_at_ms }),
        (err: unknown) => {
          // No answer in 2 s, a refusal, a dropped link, or a server older
          // than the message (invalid_message). None of them is a reason to
          // refuse the offer; forget it, so the next offer asks again.
          if (this.#ice === entry) this.#ice = undefined;
          this.#o.log?.(`no ICE servers from the server (${err instanceof Error ? err.message : String(err)}); using none`);
          return undefined;
        },
      ),
    };
    this.#ice = entry;
    return entry;
  }

  /**
   * The ICE servers for a peer connection made now under `leaseId`: the fixed
   * list if one was configured, else the lease's stored answer, asked for
   * again if there is none or its credential is at its expiry. Never rejects;
   * with nothing to go on it is the empty list, stated explicitly.
   */
  async #iceFor(leaseId: string): Promise<RTCIceServer[]> {
    if (this.#o.iceServers) return this.#o.iceServers;
    const now = this.#o.now ?? Date.now;
    const fresh = (c: { expiresAtMs: number | undefined }) =>
      c.expiresAtMs === undefined || now() < c.expiresAtMs - ICE_EXPIRY_MARGIN_MS;
    let got = await (this.#ice?.leaseId === leaseId ? this.#ice : this.#askIce(leaseId)).got;
    // Asked again at most once per offer: the operator is waiting for the answer.
    if (got && !fresh(got) && this.#lease?.leaseId === leaseId) got = await this.#askIce(leaseId).got;
    return got && fresh(got) ? got.servers : [];
  }

  /** A new offer replaces whatever peer was there: one peer per lease, the newest. */
  async #answer(sessionId: string, lease: { leaseId: string; operatorId: string }, sdp: string): Promise<void> {
    this.closePeer();
    const waiting: PendingOffer = { id: sessionId, remoteIce: [] };
    this.#pending = waiting;
    const iceServers = await this.#iceFor(lease.leaseId);
    // A newer offer, a revocation or a steal while the ICE servers were asked for.
    if (this.#pending !== waiting || this.#lease?.leaseId !== lease.leaseId) return;
    this.#pending = undefined;
    const operatorId = lease.operatorId;
    this.#lastIceServers = iceServers;
    const pc = new RTCPeerConnection({
      // Always explicit, the empty list included: nothing may leave the
      // machine that the installation did not configure (dropDefaultStun does
      // the half of that which werift does not). Loopback is offered so a
      // console on the same machine connects with no network.
      iceServers,
      iceAdditionalHostAddresses: ["127.0.0.1"],
    });
    const s: Session = {
      id: sessionId,
      operatorId,
      pc,
      answerSent: false,
      localIce: [],
      remoteSet: false,
      remoteIce: waiting.remoteIce,
      channel: undefined,
      lastSeq: 0,
    };
    this.#session = s;

    pc.onIceCandidate.subscribe((c) => {
      if (!c || !c.candidate || this.#session !== s) return;
      const candidate = { candidate: c.candidate, sdpMid: c.sdpMid ?? null, sdpMLineIndex: c.sdpMLineIndex ?? null };
      if (s.answerSent) this.#signal(s, "ice", { session: s.id, candidate });
      else s.localIce.push(candidate);
    });
    pc.onDataChannel.subscribe((channel) => {
      if (this.#session !== s || channel.label !== TWIST_CHANNEL_LABEL) {
        channel.close();
        return;
      }
      s.channel = channel;
      channel.onMessage.subscribe((data) => this.#onMessage(s, channel, data));
      this.#o.log?.("twist data channel open");
    });
    pc.connectionStateChange.subscribe((state) => {
      if (this.#session !== s || (state !== "failed" && state !== "closed")) return;
      this.#o.log?.(`peer connection ${state}`);
      this.closePeer();
    });

    try {
      await pc.setRemoteDescription({ type: "offer", sdp });
      s.remoteSet = true;
      for (const c of s.remoteIce.splice(0)) pc.addIceCandidate(c).catch(() => {});
      dropDefaultStun(pc, iceServers);
      const answer = await pc.createAnswer();
      await pc.setLocalDescription(answer);
      if (this.#session !== s) return;
      this.#signal(s, "answer", { session: s.id, type: "answer", sdp: pc.localDescription?.sdp ?? answer.sdp });
      s.answerSent = true;
      for (const candidate of s.localIce.splice(0)) this.#signal(s, "ice", { session: s.id, candidate });
    } catch (err) {
      this.#o.log?.(`offer refused: ${err instanceof Error ? err.message : String(err)}`);
      if (this.#session === s) this.closePeer();
    }
  }

  #onMessage(s: Session, channel: RTCDataChannel, data: string | Buffer): void {
    if (this.#session !== s || data.length > MAX_MESSAGE_BYTES) return;
    let m: Record<string, unknown>;
    try {
      const parsed: unknown = JSON.parse(typeof data === "string" ? data : data.toString("utf8"));
      if (parsed === null || typeof parsed !== "object" || Array.isArray(parsed)) return;
      m = parsed as Record<string, unknown>;
    } catch {
      return;
    }
    if (typeof m.ping === "number") {
      // Liveness for the operator's side. It is not a twist: the deadman does not see it.
      try {
        channel.send(JSON.stringify({ pong: m.ping }));
      } catch {
        /* closing */
      }
      return;
    }
    const twist = parseTwist(m);
    if (!twist) return;
    // Unordered and unreliable: only a twist newer than the last one obeyed
    // counts. A twist refused for its lease must not move the mark, or a
    // stranger's large seq would deafen the robot to its real driver.
    if (twist.seq <= s.lastSeq) return;
    if (this.#o.onTwist(twist.payload)) s.lastSeq = twist.seq;
  }

  #signal(s: Session, kind: "answer" | "ice", data: unknown): void {
    const client = this.#o.client();
    try {
      client.send("signal", { to: s.operatorId, kind, data }, { id: `p2p.signal.${++this.#signals}` });
    } catch {
      /* control link down: the operator's attempt times out and it offers again */
    }
  }
}

/** The data-channel twist: the bus twist payload plus a positive integer `seq`. */
export function parseTwist(m: Record<string, unknown>): { seq: number; payload: TwistPayload } | undefined {
  const { lease_id, seq, linear, angular } = m as {
    lease_id?: unknown;
    seq?: unknown;
    linear?: { x_mps?: unknown; y_mps?: unknown } | null;
    angular?: { z_radps?: unknown } | null;
  };
  if (typeof lease_id !== "string" || lease_id === "") return undefined;
  if (typeof seq !== "number" || !Number.isSafeInteger(seq) || seq < 1) return undefined;
  if (typeof linear !== "object" || linear === null || typeof angular !== "object" || angular === null) return undefined;
  const { x_mps, y_mps } = linear;
  const { z_radps } = angular;
  if (!finite(x_mps) || !finite(z_radps) || (y_mps !== undefined && !finite(y_mps))) return undefined;
  return {
    seq,
    payload: { lease_id, linear: { x_mps, ...(y_mps !== undefined ? { y_mps } : {}) }, angular: { z_radps } },
  };
}

function finite(v: unknown): v is number {
  return typeof v === "number" && Number.isFinite(v);
}
