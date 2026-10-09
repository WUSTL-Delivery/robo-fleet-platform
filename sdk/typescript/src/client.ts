// FleetClient: the connection core every TypeScript client (service, sim robot,
// console operator) is built on. It owns exactly the identity + liveness part of
// the wire protocol (docs/INTEGRATION.md §2.2, §2.3):
//
//   1. Enroll ONCE: if the TokenStore is empty, open a one-shot socket, send
//      `enroll.request {enrollment_key, kind, name}`, persist the
//      `enroll.response` credentials. Operators pass kind "operator" and their
//      single-use invite key as the enrollment key.
//   2. Hello on EVERY connect: `hello {token, agent}` → `welcome`.
//   3. Heartbeat every `welcome.heartbeat_interval_ms` while open.
//   4. Reconnect with exponential backoff + jitter using the SAME token. It
//      never re-enrolls on reconnect (re-enrolling would mint a new identity).
//
// Everything else (subscribe/snapshot, events, channels, leases, signaling) is
// layered on top through this small surface:
//
//   client.send(type, payload, {id?})  typed outbound envelope; throws unless open
//   client.on(type, handler)           typed inbound envelopes of one type
//   client.onMessage(handler)          every inbound envelope (AnyEnvelope)
//   client.onState(handler)            connection-state changes; "open" carries
//                                      the welcome and fires on EVERY (re)connect,
//                                      which is where a layer re-subscribes
//   client.state / .welcome / .clientId
//
// Each on*/onState call returns an unsubscribe function.
//
// Snapshot-then-stream (docs/INTEGRATION.md §2.5) is built in on top of that:
//
//   await client.subscribe(["presence", "events"])  resolves with the snapshot
//   client.onSnapshot(h)                  every snapshot, including reconnects
//   client.onEvent(h) / onEvent(name, h)  `event` envelopes, data typed per name
//   client.onPresence(h) / onTelemetry(h) shorthands for the common event names
//   client.onLayer(h)                     layer.declare / layer.update
//   client.channel(name)                  publish / onMessage / subscribe (channel.ts)
//
// Topics are remembered. On EVERY (re)connect the client re-sends one
// `subscribe` with all of them before any onState("open") handler runs, and
// stream callbacks (events, channel messages, layers) are held back while a
// snapshot is outstanding, so a consumer always sees a fresh snapshot before any
// event that follows it. Stream envelopes still held when a socket dies are
// dropped: the next snapshot supersedes them. The raw on()/onMessage() handlers
// are not gated; they see every frame the moment it arrives.
//
// Direct messages (lease.granted, lease.revoked, twist, signal) are not topic
// streams; they arrive only for the robot or operator involved and are read
// with client.on(type, handler).
//
// Reconnect policy: only transient failures retry — network drop, server
// restart, handshake timeout, heartbeat lapse (`rate_limited`). Any other server
// refusal is terminal and closes the client: `auth_failed` (bad/revoked token,
// including one revoked while connected; bad enrollment key), `conflict`
// (another connection took over this identity, so reconnecting would just kick
// it back and ping-pong; also a reused invite key), and the rest. The client
// never clears the TokenStore by itself.
//
// Browser-safe: uses the global WebSocket unless one is injected (Node 20 has no
// global WebSocket; pass e.g. the `ws` package's constructor there).

import type {
  AgentInfo,
  ClientKind,
  ErrorPayload,
  EventName,
  HelpDetails,
  Lease,
  LeaseRevokedPayload,
  SnapshotPayload,
  TelemetryPayload,
  WelcomePayload,
} from "./generated/protocol.js";
import { PROTOCOL_VERSION, type AnyEnvelope, type Envelope, type MessagePayloads, type MessageType } from "./generated/messages.js";
import { Channel, type ChannelMessage } from "./channel.js";
import { MemoryTokenStore, type StoredCredentials, type TokenStore } from "./tokenStore.js";

/** Minimal WebSocket surface the client uses (WHATWG WebSocket and `ws` both fit). */
export interface WebSocketLike {
  readonly readyState: number;
  send(data: string): void;
  close(code?: number, reason?: string): void;
  // Handler params are `any` so both the DOM and `ws` event types are assignable.
  onopen: ((ev: any) => void) | null;
  onmessage: ((ev: any) => void) | null;
  onclose: ((ev: any) => void) | null;
  onerror: ((ev: any) => void) | null;
}

export type WebSocketConstructor = new (url: string) => WebSocketLike;

const WS_OPEN = 1;

/** Failure codes worth retrying; everything else closes the client. */
const RETRYABLE: ReadonlySet<FleetClientError["code"]> = new Set(["network", "timeout", "rate_limited"]);

/**
 * - `idle`: constructed, connect() not called yet
 * - `enrolling`: exchanging the enrollment key for a token (first run only)
 * - `connecting`: socket opening / hello sent, waiting for welcome
 * - `open`: welcome received, heartbeating; send() works
 * - `reconnecting`: connection lost, waiting out the backoff delay
 * - `closed`: terminal; close() was called or the server refused us
 */
export type ConnectionState = "idle" | "enrolling" | "connecting" | "open" | "reconnecting" | "closed";

export interface StateChange {
  state: ConnectionState;
  /** Set when state is "open": the welcome for this connection. */
  welcome?: WelcomePayload;
  /** Set when state is "reconnecting": delay before the next attempt, and its 1-based number. */
  retryInMs?: number;
  attempt?: number;
  /** Why the previous connection ended or why the client closed, when known. */
  error?: FleetClientError;
}

/** An error the client surfaces; `code` is the server error code when there is one. */
export class FleetClientError extends Error {
  readonly code: ErrorPayload["code"] | "network" | "timeout" | "closed" | "protocol";
  constructor(code: FleetClientError["code"], message: string) {
    super(message);
    this.name = "FleetClientError";
    this.code = code;
  }
}

export interface ReconnectOptions {
  /** First retry delay. Default 250 ms. */
  initialDelayMs?: number;
  /** Cap on the delay. Default 10 000 ms. */
  maxDelayMs?: number;
  /** Growth per failed attempt. Default 2. */
  factor?: number;
}

export interface FleetClientOptions {
  /** The server's WebSocket endpoint, e.g. "ws://localhost:8080/ws". */
  url: string;
  /** Client kind to enroll as. Only used when enrolling. */
  kind: ClientKind;
  /** Display name sent with enroll.request. */
  name?: string;
  /** Enrollment key (robot/service) or single-use invite key (operator). Needed only when no token is stored. */
  enrollmentKey?: string;
  /** Where credentials persist. Default: in memory. */
  tokenStore?: TokenStore;
  /** Sent with enroll.request and every hello. */
  agent?: AgentInfo;
  /** WebSocket constructor to use instead of the global one. */
  WebSocket?: WebSocketConstructor;
  /** Backoff tuning, or false to never reconnect after a drop. */
  reconnect?: ReconnectOptions | false;
  /** How long to wait for welcome / enroll.response after the socket opens. Default 10 000 ms. */
  handshakeTimeoutMs?: number;
}

export interface SendOptions {
  /** Correlation id; echoed back as `ref` on an error reply. */
  id?: string;
}

type Handler<E> = (env: E) => void;

/**
 * Subscribe topics: the well-known ones, or `channel:<name>` for a channel's
 * broadcasts (see Channel.subscribe).
 */
export type Topic = "presence" | "events" | "telemetry" | "layers" | `channel:${string}`;

/** The `data` each event name carries, as the server emits it. */
export interface FleetEventData {
  "robot.online": undefined;
  "robot.offline": undefined;
  /** The robot's telemetry payload, verbatim. */
  "robot.telemetry": TelemetryPayload;
  /**
   * The queue entry: the robot's help.request (reason, context) plus
   * requested_at_ms, the server time it entered the queue. The same object is
   * `help` on a HELP_REQUESTED robot's snapshot entry.
   */
  "robot.help_requested": HelpDetails;
  "robot.lease_granted": Lease;
  /** Handback: reason is "released". */
  "robot.lease_released": LeaseRevokedPayload;
  /** Expiry, steal, or operator loss. `help` is set when the robot went back to HELP_REQUESTED. */
  "robot.lease_revoked": LeaseRevokedPayload;
}

// Compile-time guard: FleetEventData covers exactly the protocol's event names.
type Exactly<A, B> = [A] extends [B] ? ([B] extends [A] ? true : never) : never;
const _eventNamesCovered: Exactly<keyof FleetEventData, EventName> = true;
void _eventNamesCovered;

/** One `event` envelope, narrowed by its name. */
export type FleetEvent<N extends EventName = EventName> = {
  [K in N]: {
    event: K;
    robot_id: string;
    data: FleetEventData[K];
    /** Server send time of the envelope, when present. */
    ts_ms?: number;
  };
}[N];

export type PresenceEvent = FleetEvent<"robot.online" | "robot.offline">;
export type TelemetryEvent = FleetEvent<"robot.telemetry">;

/** layer.declare / layer.update envelopes, as delivered to `layers` subscribers. */
export type LayerEnvelope = Envelope<"layer.declare"> | Envelope<"layer.update">;

type Waiter = { resolve: (s: SnapshotPayload) => void; reject: (e: Error) => void };

export class FleetClient {
  readonly #opts: FleetClientOptions;
  readonly #store: TokenStore;
  readonly #WS: WebSocketConstructor;
  readonly #reconnect: Required<ReconnectOptions> | false;

  #state: ConnectionState = "idle";
  #welcome: WelcomePayload | undefined;
  #credentials: StoredCredentials | null = null;
  #ws: WebSocketLike | undefined;
  #heartbeat: ReturnType<typeof setInterval> | undefined;
  #retryTimer: ReturnType<typeof setTimeout> | undefined;
  #attempt = 0;
  #connectPromise: Promise<WelcomePayload> | undefined;
  #firstOpen: { resolve: (w: WelcomePayload) => void; reject: (e: Error) => void } | undefined;

  readonly #typed = new Map<string, Set<Handler<never>>>();
  readonly #any = new Set<Handler<AnyEnvelope>>();
  readonly #stateHandlers = new Set<Handler<StateChange>>();

  // Snapshot-then-stream state.
  readonly #topics = new Set<string>();
  #subSeq = 0;
  /** subscribe sends on the current socket still waiting for their snapshot, in send order. */
  #inflight: { id: string; waiters: Waiter[]; added: string[] }[] = [];
  /** subscribe() calls made while not open; the next (re)connect's snapshot answers them. */
  #deferred: Waiter[] = [];
  /** Stream envelopes held back while a snapshot is outstanding. */
  #held: AnyEnvelope[] = [];
  #snapshot: SnapshotPayload | undefined;
  readonly #snapshotHandlers = new Set<Handler<SnapshotPayload>>();
  readonly #eventHandlers = new Map<string, Set<Handler<FleetEvent>>>(); // "*" = every event
  readonly #channelHandlers = new Map<string, Set<Handler<ChannelMessage>>>(); // "*" = every channel
  readonly #layerHandlers = new Set<Handler<LayerEnvelope>>();

  constructor(opts: FleetClientOptions) {
    this.#opts = opts;
    this.#store = opts.tokenStore ?? new MemoryTokenStore();
    const WS = opts.WebSocket ?? (globalThis as { WebSocket?: WebSocketConstructor }).WebSocket;
    if (!WS) throw new Error("FleetClient: no global WebSocket; pass options.WebSocket (e.g. from the `ws` package)");
    this.#WS = WS;
    this.#reconnect =
      opts.reconnect === false
        ? false
        : { initialDelayMs: 250, maxDelayMs: 10_000, factor: 2, ...opts.reconnect };
  }

  get state(): ConnectionState {
    return this.#state;
  }

  /** The welcome of the current connection (undefined until the first one). */
  get welcome(): WelcomePayload | undefined {
    return this.#welcome;
  }

  /** This client's id: from the store or the latest welcome. */
  get clientId(): string | undefined {
    return this.#welcome?.client_id ?? this.#credentials?.client_id;
  }

  /**
   * Enrolls if needed, then connects. Resolves with the first welcome; rejects
   * on a terminal failure (e.g. auth_failed, conflict) or close() before the first
   * welcome. Network failures before the first welcome are retried with backoff.
   * After that, reconnects happen in the background; watch onState.
   * Calling it again returns the same promise.
   */
  connect(): Promise<WelcomePayload> {
    if (this.#connectPromise) return this.#connectPromise;
    this.#connectPromise = new Promise<WelcomePayload>((resolve, reject) => {
      this.#firstOpen = { resolve, reject };
    });
    // A caller that only uses onState should not get an unhandled rejection.
    this.#connectPromise.catch(() => {});
    void this.#attemptConnect();
    return this.#connectPromise;
  }

  /** Closes the connection and stops reconnecting. Terminal. */
  close(): void {
    this.#terminate(new FleetClientError("closed", "client closed"));
  }

  /**
   * Sends one envelope. Throws unless state is "open". Returns the envelope
   * sent (with ts_ms filled in).
   */
  send<T extends MessageType>(type: T, payload: MessagePayloads[T], opts: SendOptions = {}): Envelope<T> {
    const ws = this.#ws;
    if (this.#state !== "open" || !ws || ws.readyState !== WS_OPEN) {
      throw new FleetClientError("closed", `cannot send ${type}: connection is ${this.#state}`);
    }
    const env = envelope(type, payload, opts.id);
    ws.send(JSON.stringify(env));
    return env;
  }

  /** Subscribes to inbound envelopes of one type. Returns an unsubscribe function. */
  on<T extends MessageType>(type: T, handler: Handler<Envelope<T>>): () => void {
    let set = this.#typed.get(type);
    if (!set) this.#typed.set(type, (set = new Set()));
    set.add(handler as Handler<never>);
    return () => set.delete(handler as Handler<never>);
  }

  /** Subscribes to every inbound envelope. Returns an unsubscribe function. */
  onMessage(handler: Handler<AnyEnvelope>): () => void {
    this.#any.add(handler);
    return () => this.#any.delete(handler);
  }

  /** Subscribes to connection-state changes. Returns an unsubscribe function. */
  onState(handler: Handler<StateChange>): () => void {
    this.#stateHandlers.add(handler);
    return () => this.#stateHandlers.delete(handler);
  }

  // ------------------------------------------------ snapshot-then-stream API

  /** Topics this client has subscribed to (re-sent on every reconnect). */
  get topics(): readonly string[] {
    return [...this.#topics];
  }

  /** The latest snapshot received, if any. */
  get snapshot(): SnapshotPayload | undefined {
    return this.#snapshot;
  }

  /**
   * Subscribes to `topics` (additive) and resolves with the snapshot that
   * answers it. The topics are remembered and re-subscribed on every reconnect,
   * each time delivering a fresh snapshot to onSnapshot before further stream
   * callbacks. Called while not open, it resolves with the next connection's
   * snapshot. Rejects if the server refuses the subscribe or the client closes.
   */
  subscribe(topics: readonly Topic[]): Promise<SnapshotPayload> {
    if (topics.length === 0) return Promise.reject(new TypeError("subscribe: topics must not be empty"));
    if (this.#state === "closed") {
      return Promise.reject(new FleetClientError("closed", "cannot subscribe: client is closed"));
    }
    const added = topics.filter((t) => !this.#topics.has(t));
    for (const t of added) this.#topics.add(t);
    return new Promise<SnapshotPayload>((resolve, reject) => {
      const waiter: Waiter = { resolve, reject };
      const ws = this.#ws;
      if (this.#state === "open" && ws && ws.readyState === WS_OPEN) this.#sendSubscribe(ws, [...topics], [waiter], added);
      else this.#deferred.push(waiter);
    });
  }

  /** Every snapshot this client receives, including the one after each reconnect. */
  onSnapshot(handler: Handler<SnapshotPayload>): () => void {
    this.#snapshotHandlers.add(handler);
    return () => this.#snapshotHandlers.delete(handler);
  }

  /** `event` envelopes: every one, or only those with the given name (data typed accordingly). */
  onEvent(handler: Handler<FleetEvent>): () => void;
  onEvent<N extends EventName>(name: N, handler: Handler<FleetEvent<N>>): () => void;
  onEvent(a: EventName | Handler<FleetEvent>, b?: Handler<never>): () => void {
    const key = typeof a === "string" ? a : "*";
    const handler = (typeof a === "string" ? b : a) as Handler<FleetEvent>;
    return addTo(this.#eventHandlers, key, handler);
  }

  /** robot.online / robot.offline (topic "presence"). */
  onPresence(handler: Handler<PresenceEvent>): () => void {
    const offOn = this.onEvent("robot.online", handler);
    const offOff = this.onEvent("robot.offline", handler);
    return () => {
      offOn();
      offOff();
    };
  }

  /** robot.telemetry (topic "telemetry"); `data` is the robot's telemetry payload. */
  onTelemetry(handler: Handler<TelemetryEvent>): () => void {
    return this.onEvent("robot.telemetry", handler);
  }

  /** `channel.message` on one channel, or on every channel when `channel` is undefined. */
  onChannelMessage(channel: string | undefined, handler: Handler<ChannelMessage>): () => void {
    return addTo(this.#channelHandlers, channel ?? "*", handler);
  }

  /** layer.declare / layer.update (topic "layers"). */
  onLayer(handler: Handler<LayerEnvelope>): () => void {
    this.#layerHandlers.add(handler);
    return () => this.#layerHandlers.delete(handler);
  }

  /** A typed view of one channel. Throws on an invalid channel name. */
  channel<T = unknown>(name: string): Channel<T> {
    return new Channel<T>(this, name);
  }

  // ---------------------------------------------------------------- internals

  #sendSubscribe(ws: WebSocketLike, topics: string[], waiters: Waiter[], added: string[] = []): void {
    const id = `sub-${++this.#subSeq}`;
    this.#inflight.push({ id, waiters, added });
    ws.send(JSON.stringify(envelope("subscribe", { topics }, id)));
  }

  /** Stream half of inbound dispatch: snapshots, subscribe errors, gated events. */
  #stream(env: AnyEnvelope): void {
    switch (env.type) {
      case "snapshot": {
        const entry = this.#inflight.shift();
        this.#snapshot = env.payload;
        for (const h of [...this.#snapshotHandlers]) safeCall(h, env.payload);
        for (const w of entry?.waiters ?? []) w.resolve(env.payload);
        if (this.#inflight.length === 0) this.#flushHeld();
        return;
      }
      case "error": {
        const i = env.payload.ref === undefined ? -1 : this.#inflight.findIndex((e) => e.id === env.payload.ref);
        if (i < 0) return;
        const [entry] = this.#inflight.splice(i, 1);
        // Refused: forget the topics this call introduced so reconnects do not re-send them.
        for (const t of entry!.added) this.#topics.delete(t);
        const err = new FleetClientError(env.payload.code, `subscribe: ${env.payload.message}`);
        for (const w of entry!.waiters) w.reject(err);
        if (this.#inflight.length === 0) this.#flushHeld();
        return;
      }
      case "event":
      case "channel.message":
      case "layer.declare":
      case "layer.update":
        if (this.#inflight.length > 0) this.#held.push(env);
        else this.#deliver(env);
        return;
      default:
        return;
    }
  }

  #flushHeld(): void {
    const held = this.#held;
    this.#held = [];
    for (const env of held) this.#deliver(env);
  }

  #deliver(env: AnyEnvelope): void {
    if (env.type === "event") {
      const p = env.payload;
      const ev = { event: p.event, robot_id: p.robot_id ?? "", data: p.data } as FleetEvent;
      if (env.ts_ms !== undefined) ev.ts_ms = env.ts_ms;
      for (const key of [p.event, "*"]) {
        const set = this.#eventHandlers.get(key);
        if (set) for (const h of [...set]) safeCall(h, ev);
      }
    } else if (env.type === "channel.message") {
      const msg: ChannelMessage = { ...env.payload };
      if (env.ts_ms !== undefined) msg.ts_ms = env.ts_ms;
      for (const key of [msg.channel, "*"]) {
        const set = this.#channelHandlers.get(key);
        if (set) for (const h of [...set]) safeCall(h, msg);
      }
    } else if (env.type === "layer.declare" || env.type === "layer.update") {
      for (const h of [...this.#layerHandlers]) safeCall(h, env);
    }
  }

  /** The socket that carried the in-flight subscribes is gone: they wait for the next connection. */
  #onSocketLost(): void {
    for (const e of this.#inflight) this.#deferred.push(...e.waiters);
    this.#inflight = [];
    this.#held = [];
  }

  async #attemptConnect(): Promise<void> {
    if (this.#closed()) return;
    let creds: StoredCredentials | null;
    try {
      creds = this.#credentials ?? (await this.#store.load());
      if (!creds) {
        this.#setState({ state: "enrolling" });
        creds = await this.#enroll();
        if (this.#closed()) return;
        await this.#store.save(creds);
      }
    } catch (err) {
      this.#onAttemptFailed(asClientError(err));
      return;
    }
    this.#credentials = creds;
    if (this.#closed()) return;
    this.#openSession(creds.token);
  }

  // A method, not an inline check: state changes across awaits and TS would
  // otherwise keep the pre-await narrowing.
  #closed(): boolean {
    return this.#state === "closed";
  }

  /** One-shot enroll socket: enroll.request → enroll.response, then the server closes it. */
  #enroll(): Promise<StoredCredentials> {
    const key = this.#opts.enrollmentKey;
    if (!key) {
      return Promise.reject(
        new FleetClientError("auth_failed", "no stored token and no enrollmentKey to enroll with"),
      );
    }
    return new Promise((resolve, reject) => {
      const ws = new this.#WS(this.#opts.url);
      let settled = false;
      const finish = (err: FleetClientError | null, creds?: StoredCredentials) => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        ws.onopen = ws.onmessage = ws.onerror = ws.onclose = null;
        try {
          ws.close();
        } catch {
          /* already closed */
        }
        if (err) reject(err);
        else resolve(creds!);
      };
      const timer = setTimeout(
        () => finish(new FleetClientError("timeout", "enroll: no response")),
        this.#opts.handshakeTimeoutMs ?? 10_000,
      );
      ws.onopen = () => {
        const payload: MessagePayloads["enroll.request"] = { enrollment_key: key, kind: this.#opts.kind };
        if (this.#opts.name !== undefined) payload.name = this.#opts.name;
        if (this.#opts.agent !== undefined) payload.agent = this.#opts.agent;
        ws.send(JSON.stringify(envelope("enroll.request", payload)));
      };
      ws.onmessage = (ev) => {
        const env = parseEnvelope(ev.data);
        if (env?.type === "enroll.response") {
          const { token, client_id, fleet_id } = env.payload;
          finish(null, { token, client_id, fleet_id });
        } else if (env?.type === "error") {
          finish(new FleetClientError(env.payload.code, `enroll: ${env.payload.message}`));
        } else {
          finish(new FleetClientError("protocol", `enroll: unexpected ${env?.type ?? "frame"}`));
        }
      };
      ws.onerror = () => finish(new FleetClientError("network", "enroll: socket error"));
      ws.onclose = () => finish(new FleetClientError("network", "enroll: socket closed before response"));
    });
  }

  /** Opens the session socket: hello → welcome → heartbeat, until it drops. */
  #openSession(token: string): void {
    this.#setState({ state: "connecting" });
    const ws = new this.#WS(this.#opts.url);
    this.#ws = ws;
    let welcomed = false;
    let lastError: FleetClientError | undefined;

    const handshakeTimer = setTimeout(() => {
      lastError = new FleetClientError("timeout", "no welcome after hello");
      ws.close();
    }, this.#opts.handshakeTimeoutMs ?? 10_000);

    ws.onopen = () => {
      const payload: MessagePayloads["hello"] = { token };
      if (this.#opts.agent !== undefined) payload.agent = this.#opts.agent;
      ws.send(JSON.stringify(envelope("hello", payload)));
    };

    ws.onmessage = (ev) => {
      if (this.#ws !== ws) return;
      const env = parseEnvelope(ev.data);
      if (!env) return;
      if (env.type === "welcome" && !welcomed) {
        welcomed = true;
        clearTimeout(handshakeTimer);
        this.#onWelcome(ws, env.payload);
      } else if (env.type === "error") {
        // Before welcome any error is the handshake's answer. After it, errors
        // are replies to our sends and do not close the socket, except these
        // three, which the server sends right before it closes (auth_failed:
        // this client's token was revoked).
        const c = env.payload.code;
        if (!welcomed || c === "conflict" || c === "rate_limited" || c === "auth_failed") {
          lastError = new FleetClientError(c, env.payload.message);
        }
      }
      this.#dispatch(env);
      if (this.#ws === ws) this.#stream(env);
    };

    ws.onerror = () => {
      lastError ??= new FleetClientError("network", "socket error");
    };

    ws.onclose = (ev: { code?: number; reason?: string } | undefined) => {
      clearTimeout(handshakeTimer);
      if (this.#ws !== ws) return; // superseded or closed by us
      this.#ws = undefined;
      this.#stopHeartbeat();
      this.#onSocketLost();
      const err =
        lastError ??
        new FleetClientError("network", `socket closed (${ev?.code ?? "?"}${ev?.reason ? `: ${ev.reason}` : ""})`);
      this.#onAttemptFailed(err);
    };
  }

  #onWelcome(ws: WebSocketLike, welcome: WelcomePayload): void {
    this.#welcome = welcome;
    this.#attempt = 0;
    this.#stopHeartbeat();
    this.#heartbeat = setInterval(() => {
      if (ws.readyState === WS_OPEN) ws.send(JSON.stringify(envelope("heartbeat", {})));
    }, welcome.heartbeat_interval_ms);
    // Re-subscribe before any onState("open") handler runs, so the fresh
    // snapshot is the first stream delivery on this connection.
    if (this.#topics.size > 0) {
      const waiters = this.#deferred;
      this.#deferred = [];
      this.#sendSubscribe(ws, [...this.#topics], waiters);
    }
    this.#setState({ state: "open", welcome });
    const first = this.#firstOpen;
    this.#firstOpen = undefined;
    first?.resolve(welcome);
  }

  /** A connect attempt or a live connection ended: retry, or give up if terminal. */
  #onAttemptFailed(err: FleetClientError): void {
    if (this.#state === "closed") return;
    if (!RETRYABLE.has(err.code) || this.#reconnect === false) {
      this.#terminate(err);
      return;
    }
    const r = this.#reconnect;
    this.#attempt += 1;
    const base = Math.min(r.maxDelayMs, r.initialDelayMs * r.factor ** (this.#attempt - 1));
    const delay = Math.round(base / 2 + Math.random() * (base / 2)); // jitter in [base/2, base]
    this.#setState({ state: "reconnecting", retryInMs: delay, attempt: this.#attempt, error: err });
    this.#retryTimer = setTimeout(() => {
      this.#retryTimer = undefined;
      void this.#attemptConnect();
    }, delay);
  }

  #terminate(err: FleetClientError): void {
    if (this.#state === "closed") return;
    clearTimeout(this.#retryTimer);
    this.#retryTimer = undefined;
    this.#stopHeartbeat();
    const ws = this.#ws;
    this.#ws = undefined;
    if (ws) {
      try {
        ws.close(1000, "client closed");
      } catch {
        /* already closed */
      }
    }
    this.#onSocketLost();
    const pending = this.#deferred;
    this.#deferred = [];
    this.#setState({ state: "closed", error: err });
    for (const w of pending) w.reject(err);
    const first = this.#firstOpen;
    this.#firstOpen = undefined;
    first?.reject(err);
  }

  #stopHeartbeat(): void {
    clearInterval(this.#heartbeat);
    this.#heartbeat = undefined;
  }

  #setState(change: StateChange): void {
    this.#state = change.state;
    for (const h of [...this.#stateHandlers]) safeCall(h, change);
  }

  #dispatch(env: AnyEnvelope): void {
    const set = this.#typed.get(env.type);
    if (set) for (const h of [...set]) safeCall(h as Handler<AnyEnvelope>, env);
    for (const h of [...this.#any]) safeCall(h, env);
  }
}

// ------------------------------------------------------------------ helpers

function addTo<K, H>(map: Map<K, Set<H>>, key: K, handler: H): () => void {
  let set = map.get(key);
  if (!set) map.set(key, (set = new Set()));
  set.add(handler);
  return () => set.delete(handler);
}

function envelope<T extends MessageType>(type: T, payload: MessagePayloads[T], id?: string): Envelope<T> {
  const env = { v: PROTOCOL_VERSION, type, ts_ms: Date.now(), payload } as Envelope<T>;
  if (id !== undefined) env.id = id;
  return env;
}

/** Parses one inbound frame; null when it is not a v0 envelope. */
function parseEnvelope(data: unknown): AnyEnvelope | null {
  let text: string;
  if (typeof data === "string") text = data;
  else if (data instanceof ArrayBuffer || ArrayBuffer.isView(data)) text = new TextDecoder().decode(data as ArrayBuffer);
  else text = String(data);
  try {
    const v: unknown = JSON.parse(text);
    if (typeof v !== "object" || v === null) return null;
    const e = v as { v?: unknown; type?: unknown; payload?: unknown };
    if (e.v !== PROTOCOL_VERSION || typeof e.type !== "string") return null;
    if (typeof e.payload !== "object" || e.payload === null) return null;
    return v as AnyEnvelope;
  } catch {
    return null;
  }
}

function asClientError(err: unknown): FleetClientError {
  if (err instanceof FleetClientError) return err;
  return new FleetClientError("network", err instanceof Error ? err.message : String(err));
}

/** A throwing listener must not break the others or the connection loop. */
function safeCall<E>(h: Handler<E>, arg: E): void {
  try {
    h(arg);
  } catch (err) {
    const report = (globalThis as { reportError?: (e: unknown) => void }).reportError;
    if (report) report(err);
    else console.error(err);
  }
}
