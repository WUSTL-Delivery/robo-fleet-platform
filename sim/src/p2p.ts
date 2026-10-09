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
// Node has no WebRTC of its own; werift is a pure TypeScript implementation.

import type { FleetClient, SignalPayload, TwistPayload } from "@fleet-platform/sdk";
import { RTCPeerConnection, type RTCDataChannel, type RTCIceServer } from "werift";

export const TWIST_CHANNEL_LABEL = "twist";
/** Anything larger on the channel is not a twist or a ping. */
const MAX_MESSAGE_BYTES = 1024;

export interface TwistAnswererOptions {
  /** The robot's control connection right now (it is replaced on re-enrollment). */
  client: () => FleetClient;
  /**
   * STUN/TURN servers. None are needed on loopback or one LAN; the list the
   * server hands out later goes in here.
   */
  iceServers?: RTCIceServer[];
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

export class TwistAnswerer {
  readonly #o: TwistAnswererOptions;
  #lease: { leaseId: string; operatorId: string } | undefined;
  #session: Session | undefined;
  #signals = 0;

  constructor(opts: TwistAnswererOptions) {
    this.#o = opts;
  }

  /** Whether a twist data channel is open right now. */
  get open(): boolean {
    return this.#session?.channel?.readyState === "open";
  }

  /** lease.granted: only this lease's operator may connect. A peer from an earlier lease is closed. */
  grant(leaseId: string, operatorId: string): void {
    if (this.#lease?.leaseId !== leaseId) this.closePeer();
    this.#lease = { leaseId, operatorId };
  }

  /** The lease is over (released, stolen, expired, operator lost): close the peer. */
  revoke(): void {
    this.#lease = undefined;
    this.closePeer();
  }

  /** Closes the peer connection, if any, and keeps the lease: the operator may offer again. */
  closePeer(): void {
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
      void this.#answer(d.session, lease.operatorId, d.sdp);
      return;
    }
    const s = this.#session;
    if (sig.kind !== "ice" || !s || s.id !== d.session) return;
    const c = d.candidate as { candidate?: unknown; sdpMid?: unknown; sdpMLineIndex?: unknown } | null;
    if (c === null || typeof c !== "object" || typeof c.candidate !== "string" || c.candidate === "") return;
    const init: IceInit = {
      candidate: c.candidate,
      ...(typeof c.sdpMid === "string" ? { sdpMid: c.sdpMid } : {}),
      ...(typeof c.sdpMLineIndex === "number" ? { sdpMLineIndex: c.sdpMLineIndex } : {}),
    };
    if (s.remoteSet) s.pc.addIceCandidate(init).catch(() => {});
    else s.remoteIce.push(init);
  }

  /** A new offer replaces whatever peer was there: one peer per lease, the newest. */
  async #answer(sessionId: string, operatorId: string, sdp: string): Promise<void> {
    this.closePeer();
    const pc = new RTCPeerConnection({
      // Explicit, so nothing leaves the machine unless a server list is given
      // (werift would otherwise default to a public STUN server). Loopback is
      // offered so a console on the same machine connects with no network.
      iceServers: this.#o.iceServers ?? [],
      iceAdditionalHostAddresses: ["127.0.0.1"],
    });
    const s: Session = {
      id: sessionId,
      operatorId,
      pc,
      answerSent: false,
      localIce: [],
      remoteSet: false,
      remoteIce: [],
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
