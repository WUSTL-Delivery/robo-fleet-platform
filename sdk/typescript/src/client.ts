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
// Reconnect policy: only transient failures retry — network drop, server
// restart, handshake timeout, heartbeat lapse (`rate_limited`). Any other server
// refusal is terminal and closes the client: `auth_failed` (bad/revoked token,
// bad enrollment key), `conflict` (another connection took over this identity,
// so reconnecting would just kick it back and ping-pong; also a reused invite
// key), and the rest. The client never clears the TokenStore by itself.
//
// Browser-safe: uses the global WebSocket unless one is injected (Node 20 has no
// global WebSocket; pass e.g. the `ws` package's constructor there).

import type { AgentInfo, ClientKind, ErrorPayload, WelcomePayload } from "./generated/protocol.js";
import { PROTOCOL_VERSION, type AnyEnvelope, type Envelope, type MessagePayloads, type MessageType } from "./generated/messages.js";
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

  // ---------------------------------------------------------------- internals

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
        // two, which the server sends right before it closes.
        const c = env.payload.code;
        if (!welcomed || c === "conflict" || c === "rate_limited") {
          lastError = new FleetClientError(c, env.payload.message);
        }
      }
      this.#dispatch(env);
    };

    ws.onerror = () => {
      lastError ??= new FleetClientError("network", "socket error");
    };

    ws.onclose = (ev: { code?: number; reason?: string } | undefined) => {
      clearTimeout(handshakeTimer);
      if (this.#ws !== ws) return; // superseded or closed by us
      this.#ws = undefined;
      this.#stopHeartbeat();
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
    this.#setState({ state: "closed", error: err });
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
