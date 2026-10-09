// One sim robot: a FleetClient (sdk/typescript) with a body, a lease gate and a
// deadman. It speaks the real protocol, so everything built against the sim
// works unchanged against a real robot running fleet_agent.
//
// On every (re)connect it declares its manifest. It then sends telemetry at
// `telemetryHz` with a pose integrated from the twist it last accepted, obeys
// twist only under its current lease, and zeroes velocity DEADMAN_MS after the
// last valid twist. Twist arrives over the bus or, once the operator holding
// the lease has connected to it, over a WebRTC data channel (p2p.ts); the same
// lease gate and deadman judge both. Optionally it wanders while autonomous and raises
// help.request at random.

import {
  FleetClient,
  FleetClientError,
  type Manifest,
  type Pose,
  type TelemetryPayload,
  type TwistPayload,
  type TokenStore,
  type WebSocketConstructor,
  type WelcomePayload,
} from "@fleet-platform/sdk";
import {
  STOPPED,
  integrate,
  realize,
  toGeo,
  type GeoPoint,
  type Limits,
  type Morphology,
  type Pose2D,
  type Velocity,
} from "./kinematics.js";
import { TwistAnswerer } from "./p2p.js";
import { DEADMAN_MS, TeleopGate, type TwistVia } from "./teleop.js";

/** Where the robot reports its pose: lat/lon around an origin, or a local Cartesian frame (D7). */
export type PoseFrame = { kind: "geographic"; origin: GeoPoint } | { kind: "local"; frameId: string };

/** The robot's own view of its intervention state. */
export type Mode = "autonomous" | "help" | "teleop";

export interface SimRobotOptions {
  url: string;
  name: string;
  /** Needed only when the token store is empty (first run). */
  enrollmentKey?: string;
  tokenStore: TokenStore;
  WebSocket: WebSocketConstructor;
  morphology: Morphology;
  limits: Limits;
  /** false: the manifest declares no drive block, so the console must offer no driving UI. */
  drive: boolean;
  start: Pose2D;
  frame: PoseFrame;
  telemetryHz?: number;
  physicsHz?: number;
  /** Mean help.request rate per robot per minute while autonomous (0 = never). */
  helpRatePerMin?: number;
  /** Drive slow random curves while autonomous. */
  wander?: boolean;
  deadmanMs?: number;
  /** false: never answer a WebRTC offer, so twist stays on the bus. Default true. */
  p2p?: boolean;
  /** STUN/TURN servers for the data channel. None are needed on loopback or one LAN. */
  iceServers?: { urls: string | string[]; username?: string; credential?: string }[];
  random?: () => number;
  log?: (line: string) => void;
}

const HELP_REASONS = ["low_confidence", "path_blocked", "localization_degraded"] as const;

export class SimRobot {
  readonly name: string;
  readonly morphology: Morphology;
  readonly #o: SimRobotOptions;
  readonly #gate: TeleopGate;
  readonly #rand: () => number;
  readonly #peer: TwistAnswerer | undefined;
  #accepted: Record<TwistVia, number> = { bus: 0, p2p: 0 };
  #lastVia: TwistVia | undefined;
  #client: FleetClient;
  #pose: Pose2D;
  #vel: Velocity = STOPPED;
  #mode: Mode = "autonomous";
  #battery: number;
  #wanderW = 0;
  #lastStepMs = 0;
  #physics: ReturnType<typeof setInterval> | undefined;
  #telemetry: ReturnType<typeof setInterval> | undefined;
  #stopped = false;

  constructor(opts: SimRobotOptions) {
    this.#o = opts;
    this.name = opts.name;
    this.morphology = opts.morphology;
    this.#gate = new TeleopGate(opts.deadmanMs ?? DEADMAN_MS);
    this.#rand = opts.random ?? Math.random;
    this.#pose = { ...opts.start };
    this.#battery = 60 + this.#rand() * 40;
    this.#client = this.#makeClient();
    if (opts.p2p !== false && opts.drive) {
      this.#peer = new TwistAnswerer({
        client: () => this.#client,
        ...(opts.iceServers ? { iceServers: opts.iceServers } : {}),
        onTwist: (twist) => this.#onTwist(twist, "p2p"),
        log: (line) => this.#log(line),
      });
    }
  }

  get client(): FleetClient {
    return this.#client;
  }
  get robotId(): string | undefined {
    return this.#client.clientId;
  }
  get mode(): Mode {
    return this.#mode;
  }
  get pose(): Pose2D {
    return { ...this.#pose };
  }
  get velocity(): Velocity {
    return this.#vel;
  }
  get leaseId(): string | undefined {
    return this.#gate.leaseId;
  }
  /** Twists obeyed so far, by the transport they arrived on. */
  get twistsAccepted(): Readonly<Record<TwistVia, number>> {
    return { ...this.#accepted };
  }
  /** The transport of the last twist obeyed under the current lease. */
  get twistVia(): TwistVia | undefined {
    return this.#lastVia;
  }
  /** Whether the operator's twist data channel is open. */
  get peerOpen(): boolean {
    return this.#peer?.open ?? false;
  }
  /** Drops the WebRTC peer as a dead link would, keeping the lease. */
  dropPeer(): void {
    this.#peer?.closePeer();
  }

  /**
   * Connects (enrolling on first run) and starts the physics and telemetry
   * loops. If a stored token is refused (e.g. the server's database was reset),
   * it clears the store and enrolls afresh once.
   */
  async start(): Promise<WelcomePayload> {
    let welcome: WelcomePayload;
    try {
      welcome = await this.#client.connect();
    } catch (err) {
      const stale = err instanceof FleetClientError && err.code === "auth_failed" && this.#o.enrollmentKey;
      if (!stale || !(await this.#o.tokenStore.load())) throw err;
      this.#log("stored token refused; re-enrolling");
      await this.#o.tokenStore.clear();
      this.#client = this.#makeClient();
      welcome = await this.#client.connect();
    }
    this.#startLoops();
    return welcome;
  }

  stop(): void {
    this.#stopped = true;
    clearInterval(this.#physics);
    clearInterval(this.#telemetry);
    this.#peer?.revoke();
    this.#client.close();
  }

  /** The manifest this robot declares. */
  manifest(): Manifest {
    const m: Manifest = {
      cameras: [{ id: "front", label: "Sim camera (no stream)" }],
      battery: {},
    };
    if (this.#o.drive) m.drive = { type: "twist", max_v_mps: this.#o.limits.maxV, max_w_radps: this.#o.limits.maxW };
    return m;
  }

  /** The pose as the wire carries it, in this robot's configured frame. */
  wirePose(): Pose {
    const p = this.#pose;
    const yaw_rad = round(p.yaw, 4);
    if (this.#o.frame.kind === "local") {
      return { frame: "local", frame_id: this.#o.frame.frameId, x_m: round(p.x, 3), y_m: round(p.y, 3), yaw_rad };
    }
    const g = toGeo(this.#o.frame.origin, p.x, p.y);
    return { frame: "geographic", lat: round(g.lat, 8), lon: round(g.lon, 8), yaw_rad };
  }

  telemetry(): TelemetryPayload {
    const v = this.#vel;
    return {
      pose: this.wirePose(),
      velocity: { v_mps: round(Math.sign(v.vx || 1) * Math.hypot(v.vx, v.vy), 3), w_radps: round(v.wz, 3) },
      battery: { pct: round(this.#battery, 1) },
      health: {
        sim: true,
        morphology: this.morphology,
        mode: this.#mode,
        ...(this.#mode === "teleop" && this.#lastVia ? { twist_via: this.#lastVia } : {}),
      },
    };
  }

  // ---------------------------------------------------------------- internals

  #makeClient(): FleetClient {
    const o = this.#o;
    const client = new FleetClient({
      url: o.url,
      kind: "robot",
      name: o.name,
      tokenStore: o.tokenStore,
      WebSocket: o.WebSocket,
      agent: { name: "fleet-sim", version: "0.0.0" },
      ...(o.enrollmentKey !== undefined ? { enrollmentKey: o.enrollmentKey } : {}),
    });

    client.onState((s) => {
      if (s.state === "open") {
        // Declare on every (re)connect. The lease is kept: the server does not
        // revoke it when a robot's link drops, so the operator's twists resume.
        client.send("manifest", this.manifest());
      } else if (s.state === "reconnecting" || s.state === "closed") {
        this.#gate.halt(); // fail closed while the link is down
        if (s.state === "closed" && !this.#stopped && s.error) this.#log(`closed: ${s.error.code} ${s.error.message}`);
      }
    });

    client.on("lease.granted", ({ payload }) => {
      if (payload.robot_id !== client.clientId) return;
      const renewal = payload.lease_id === this.#gate.leaseId;
      this.#peer?.grant(payload.lease_id, payload.operator_id);
      if (renewal) return; // same lease, later expiry: nothing to reset
      this.#gate.grant(payload.lease_id);
      this.#lastVia = undefined;
      this.#mode = "teleop";
      this.#log(`lease granted to ${payload.operator_id}`);
    });

    client.on("lease.revoked", ({ payload }) => {
      if (!this.#gate.revoke(payload.lease_id)) return;
      this.#peer?.revoke(); // cleanup: the lease is already gone
      this.#lastVia = undefined;
      // Handback resumes autonomy; expiry / operator loss put the robot back in the queue.
      this.#mode = payload.reason === "released" ? "autonomous" : "help";
      this.#log(`lease revoked (${payload.reason})`);
    });

    client.on("twist", ({ payload }) => void this.#onTwist(payload, "bus"));
    client.on("signal", ({ payload }) => this.#peer?.onSignal(payload));

    return client;
  }

  /** One twist from either transport, judged by the same gate. */
  #onTwist(payload: TwistPayload, via: TwistVia): boolean {
    if (!this.#o.drive) return false; // declared no drive: nothing to obey
    // The data channel outlives a control-link blip, but a robot that cannot
    // hear a revocation must not be driven: fail closed until it is back.
    if (via === "p2p" && this.#client.state !== "open") return false;
    const cmd = { vx: payload.linear.x_mps, vy: payload.linear.y_mps ?? 0, wz: payload.angular.z_radps };
    if (!this.#gate.accept(payload.lease_id, cmd, Date.now(), via)) return false;
    this.#accepted[via] += 1;
    this.#lastVia = via;
    return true;
  }

  #startLoops(): void {
    if (this.#physics) return;
    const physicsHz = this.#o.physicsHz ?? 20;
    const telemetryHz = this.#o.telemetryHz ?? 1;
    this.#lastStepMs = Date.now();
    this.#physics = setInterval(() => this.#step(), 1000 / physicsHz);
    this.#telemetry = setInterval(() => {
      if (this.#client.state === "open") this.#client.send("telemetry", this.telemetry());
    }, 1000 / telemetryHz);
    this.#physics.unref?.();
    this.#telemetry.unref?.();
  }

  #step(): void {
    const now = Date.now();
    const dt = Math.min(0.5, (now - this.#lastStepMs) / 1000);
    this.#lastStepMs = now;

    let cmd: Velocity = STOPPED;
    if (this.#mode === "teleop") cmd = this.#gate.current(now);
    else if (this.#mode === "autonomous" && this.#o.wander && this.#o.drive) cmd = this.#wander(dt);

    this.#vel = realize(cmd, this.morphology, this.#o.limits);
    this.#pose = integrate(this.#pose, this.#vel, dt);
    const speed = Math.hypot(this.#vel.vx, this.#vel.vy);
    this.#battery = Math.max(5, this.#battery - dt * (0.002 + 0.01 * speed));

    this.#maybeRequestHelp(dt);
  }

  /** Slow random curves that bend back toward the start point. */
  #wander(dt: number): Velocity {
    this.#wanderW += (this.#rand() - 0.5) * 0.6 * dt;
    this.#wanderW = Math.max(-0.3, Math.min(0.3, this.#wanderW));
    const { x, y, yaw } = this.#pose;
    const dx = this.#o.start.x - x;
    const dy = this.#o.start.y - y;
    let homing = 0;
    if (Math.hypot(dx, dy) > 30) {
      const err = Math.atan2(dy, dx) - yaw;
      homing = 0.5 * Math.atan2(Math.sin(err), Math.cos(err));
    }
    return { vx: Math.min(0.6, this.#o.limits.maxV), vy: 0, wz: this.#wanderW + homing };
  }

  #maybeRequestHelp(dt: number): void {
    const rate = this.#o.helpRatePerMin ?? 0;
    if (rate <= 0 || this.#mode !== "autonomous" || this.#client.state !== "open") return;
    if (this.#rand() >= (rate / 60) * dt) return;
    const reason = HELP_REASONS[Math.floor(this.#rand() * HELP_REASONS.length)] ?? HELP_REASONS[0];
    this.#client.send("help.request", { reason, context: { sim: true } });
    this.#mode = "help";
    this.#log(`help requested (${reason})`);
  }

  #log(line: string): void {
    this.#o.log?.(`${this.name}: ${line}`);
  }
}

function round(v: number, digits: number): number {
  const k = 10 ** digits;
  return Math.round(v * k) / k + 0;
}
