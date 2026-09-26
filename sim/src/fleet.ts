// N sim robots on one server, each with its own identity (own enrollment,
// own token). Used by the CLI and by the integration tests, so the demo and
// the test share code.

import { join } from "node:path";
import { MemoryTokenStore, type TokenStore, type WebSocketConstructor } from "@fleet-platform/sdk";
import { FileTokenStore } from "@fleet-platform/sdk/node";
import { WebSocket as WsWebSocket } from "ws";
import { spread, type GeoPoint, type Limits, type Morphology } from "./kinematics.js";
import { SimRobot, type PoseFrame } from "./robot.js";

/** A spot on the WashU Danforth campus; any lat/lon works. */
export const DEFAULT_ORIGIN: GeoPoint = { lat: 38.6488, lon: -90.3108 };

export const LIMITS: Record<Morphology, Limits> = {
  diff: { maxV: 1.5, maxW: 2.0 },
  omni: { maxV: 1.0, maxW: 1.5 },
};

export interface FleetOptions {
  url: string;
  enrollmentKey?: string;
  count: number;
  /** How many robots (the last ones) are holonomic instead of diff-drive. Default: one in five. */
  omni?: number;
  /** false: manifests declare no drive block. Default true. */
  drive?: boolean;
  /** Pose frame: geographic around `origin` (default) or local Cartesian. */
  frame?: "geographic" | "local";
  origin?: GeoPoint;
  /** Radius in metres the start positions are spread over. Default 120. */
  spreadM?: number;
  telemetryHz?: number;
  helpRatePerMin?: number;
  wander?: boolean;
  /** Directory for per-robot token files; null keeps tokens in memory (fresh identities each run). */
  stateDir?: string | null;
  namePrefix?: string;
  WebSocket?: WebSocketConstructor;
  log?: (line: string) => void;
}

export interface Fleet {
  robots: SimRobot[];
  stop(): void;
}

export function robotName(prefix: string, i: number, count: number): string {
  return `${prefix}-${String(i + 1).padStart(Math.max(2, String(count).length), "0")}`;
}

/** Builds and connects the fleet. Rejects if any robot fails to connect (the rest are stopped). */
export async function startFleet(o: FleetOptions): Promise<Fleet> {
  const count = o.count;
  const omni = Math.min(count, o.omni ?? Math.floor(count / 5));
  const prefix = o.namePrefix ?? "sim";
  const frame: PoseFrame =
    o.frame === "local" ? { kind: "local", frameId: "sim" } : { kind: "geographic", origin: o.origin ?? DEFAULT_ORIGIN };
  const starts = spread(count, o.spreadM ?? 120);

  const robots = starts.map((start, i) => {
    const name = robotName(prefix, i, count);
    const morphology: Morphology = i >= count - omni ? "omni" : "diff";
    const tokenStore: TokenStore =
      o.stateDir == null ? new MemoryTokenStore() : new FileTokenStore(join(o.stateDir, `${name}.json`));
    return new SimRobot({
      url: o.url,
      name,
      ...(o.enrollmentKey !== undefined ? { enrollmentKey: o.enrollmentKey } : {}),
      tokenStore,
      WebSocket: o.WebSocket ?? (WsWebSocket as unknown as WebSocketConstructor),
      morphology,
      limits: LIMITS[morphology],
      drive: o.drive ?? true,
      start,
      frame,
      ...(o.telemetryHz !== undefined ? { telemetryHz: o.telemetryHz } : {}),
      ...(o.helpRatePerMin !== undefined ? { helpRatePerMin: o.helpRatePerMin } : {}),
      ...(o.wander !== undefined ? { wander: o.wander } : {}),
      ...(o.log ? { log: o.log } : {}),
    });
  });

  const results = await Promise.allSettled(robots.map((r) => r.start()));
  const failed = results.flatMap((r, i) => (r.status === "rejected" ? [`${robots[i]!.name}: ${String(r.reason)}`] : []));
  const stop = () => robots.forEach((r) => r.stop());
  if (failed.length > 0) {
    stop();
    throw new Error(`${failed.length}/${count} robots failed to connect:\n  ${failed.join("\n  ")}`);
  }
  return { robots, stop };
}
