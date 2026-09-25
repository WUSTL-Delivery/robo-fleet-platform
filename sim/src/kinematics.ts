// Pure motion model for sim robots: no I/O, no timers, so it is unit-testable.
//
// Frames: the sim integrates in a local ENU plane (x east, y north, metres) with
// yaw counter-clockwise from +x (0 = east), the same convention telemetry's
// yaw_rad uses. Geographic output is a flat-earth projection around an origin,
// which is accurate to centimetres over the few hundred metres a sim fleet spans.

/** Body-frame velocity setpoint (geometry_msgs/Twist subset the protocol carries). */
export interface Velocity {
  /** forward, m/s */
  vx: number;
  /** left, m/s (only a holonomic base can realize it) */
  vy: number;
  /** yaw rate, rad/s, counter-clockwise positive */
  wz: number;
}

export const STOPPED: Readonly<Velocity> = Object.freeze({ vx: 0, vy: 0, wz: 0 });

/**
 * How a robot realizes a twist. Both take the same contract (D7: one control
 * type, many bodies):
 * - `diff`: differential drive; linear.y is ignored (it cannot strafe).
 * - `omni`: holonomic base; honors linear.y.
 */
export type Morphology = "diff" | "omni";

export interface Limits {
  maxV: number;
  maxW: number;
}

export interface Pose2D {
  x: number;
  y: number;
  yaw: number;
}

export interface GeoPoint {
  lat: number;
  lon: number;
}

const EARTH_RADIUS_M = 6_371_000;
const DEG = Math.PI / 180;

const clamp = (v: number, lim: number) => Math.max(-lim, Math.min(lim, v));

/** Clamps a requested twist to what this body can do. */
export function realize(cmd: Velocity, morphology: Morphology, limits: Limits): Velocity {
  const vy = morphology === "omni" ? cmd.vy : 0;
  const speed = Math.hypot(cmd.vx, vy);
  const k = speed > limits.maxV ? limits.maxV / speed : 1;
  // `+ 0` turns -0 into 0 so a clamped stop serializes as 0.
  return { vx: cmd.vx * k + 0, vy: vy * k + 0, wz: clamp(cmd.wz, limits.maxW) + 0 };
}

/** Advances a pose by a body-frame velocity over dt seconds (midpoint heading). */
export function integrate(p: Pose2D, v: Velocity, dtS: number): Pose2D {
  const mid = p.yaw + (v.wz * dtS) / 2;
  const c = Math.cos(mid);
  const s = Math.sin(mid);
  return {
    x: p.x + (v.vx * c - v.vy * s) * dtS,
    y: p.y + (v.vx * s + v.vy * c) * dtS,
    yaw: normalizeAngle(p.yaw + v.wz * dtS),
  };
}

/** Wraps an angle to (-pi, pi]. */
export function normalizeAngle(a: number): number {
  let r = a % (2 * Math.PI);
  if (r <= -Math.PI) r += 2 * Math.PI;
  else if (r > Math.PI) r -= 2 * Math.PI;
  return r;
}

/** Local ENU metres around `origin` → lat/lon. */
export function toGeo(origin: GeoPoint, x: number, y: number): GeoPoint {
  return {
    lat: origin.lat + (y / EARTH_RADIUS_M) / DEG,
    lon: origin.lon + (x / (EARTH_RADIUS_M * Math.cos(origin.lat * DEG))) / DEG,
  };
}

/** lat/lon → local ENU metres around `origin` (inverse of toGeo). */
export function fromGeo(origin: GeoPoint, p: GeoPoint): { x: number; y: number } {
  return {
    x: (p.lon - origin.lon) * DEG * EARTH_RADIUS_M * Math.cos(origin.lat * DEG),
    y: (p.lat - origin.lat) * DEG * EARTH_RADIUS_M,
  };
}

/**
 * Start positions for n robots: a sunflower (golden-angle) spiral inside
 * `radiusM`, deterministic so a restarted fleet reappears where it was. Each
 * robot faces away from the centre.
 */
export function spread(n: number, radiusM: number): Pose2D[] {
  const golden = Math.PI * (3 - Math.sqrt(5));
  return Array.from({ length: n }, (_, i) => {
    const r = n === 1 ? 0 : radiusM * Math.sqrt((i + 0.5) / n);
    const theta = i * golden;
    return { x: r * Math.cos(theta), y: r * Math.sin(theta), yaw: normalizeAngle(theta) };
  });
}
