// Installation config served by fleet-server at GET /api/console/config
// (server/internal/web/console_config.go). Nothing in it is secret; it is
// fetched once, before sign-in is needed.

export interface MapHome {
  center: { lat: number; lon: number };
  radius_m: number;
  /** Keep panning and zooming inside the home area. */
  lock: boolean;
}

export interface ConsoleConfig {
  map: MapHome | null;
}

const NONE: ConsoleConfig = { map: null };
let pending: Promise<ConsoleConfig> | null = null;

/** Resolves to the server's console config, or an empty one if it can't be read. */
export function loadConsoleConfig(): Promise<ConsoleConfig> {
  pending ??= fetch("/api/console/config", { headers: { Accept: "application/json" } })
    .then((r) => (r.ok ? (r.json() as Promise<ConsoleConfig>) : NONE))
    .then((c) => (c && typeof c === "object" && "map" in c ? c : NONE))
    .catch(() => NONE); // an older server has no endpoint: behave as before
  return pending;
}

const M_PER_DEG_LAT = 111_320;

/** [[west, south], [east, north]] for radius_m around the centre, times scale. */
export function homeBounds(home: MapHome, scale = 1): [[number, number], [number, number]] {
  const r = home.radius_m * scale;
  const dLat = r / M_PER_DEG_LAT;
  const dLon = r / (M_PER_DEG_LAT * Math.max(0.01, Math.cos((home.center.lat * Math.PI) / 180)));
  const { lat, lon } = home.center;
  return [
    [Math.max(-180, lon - dLon), Math.max(-85, lat - dLat)],
    [Math.min(180, lon + dLon), Math.min(85, lat + dLat)],
  ];
}
