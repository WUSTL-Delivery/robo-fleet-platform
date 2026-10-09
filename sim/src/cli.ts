// npm run sim -- [options]   (run from sim/; see --help)

import { resolve } from "node:path";
import { parseArgs } from "node:util";
import { DEFAULT_ORIGIN, startFleet } from "./fleet.js";

const HELP = `fleet sim: N fake robots speaking the real protocol through the TypeScript SDK.

Usage: npm run sim -- [options]

  --url <ws-url>         server WebSocket endpoint      (env FLEET_URL, default ws://localhost:8080/ws)
  --enroll-key <key>     fleet enrollment key           (env FLEET_ENROLL_KEY; only needed on first run)
  --count <n>            number of robots               (default 5)
  --omni <n>             how many are holonomic (honor linear.y); the rest are diff-drive
                                                        (default count/5, rounded down)
  --no-drive             manifests declare no drive block (robots cannot be driven)
  --no-p2p               never answer a WebRTC offer: twist stays on the server bus
  --origin <lat,lon>     centre of the fleet            (default ${DEFAULT_ORIGIN.lat},${DEFAULT_ORIGIN.lon})
  --spread <m>           radius the robots start within (default 120)
  --frame <f>            pose frame: geographic | local (default geographic)
  --telemetry-hz <hz>    telemetry rate per robot       (default 1)
  --help-rate <r>        mean help.request per robot per minute while autonomous (default 0)
  --wander               drive slow random curves while autonomous
  --state-dir <dir>      where per-robot tokens are kept (default .sim-state)
  --ephemeral            keep tokens in memory: new identities every run
  --name-prefix <p>      robot name prefix             (default sim)
  -h, --help             show this help

Send SIGUSR2 to drop every robot's WebRTC peer (to watch a console fall back to the bus).

Robots enroll once with the fleet enrollment key and keep their token in
--state-dir, so a restarted sim reuses the same robot identities.`;

function fail(msg: string): never {
  console.error(`sim: ${msg}\n\n${HELP}`);
  process.exit(2);
}

function num(name: string, v: string | undefined, dflt: number, min = 0): number {
  if (v === undefined) return dflt;
  const n = Number(v);
  if (!Number.isFinite(n) || n < min) fail(`--${name} must be a number >= ${min}`);
  return n;
}

const options = {
  url: { type: "string" },
  "enroll-key": { type: "string" },
  count: { type: "string" },
  omni: { type: "string" },
  "no-drive": { type: "boolean" },
  "no-p2p": { type: "boolean" },
  origin: { type: "string" },
  spread: { type: "string" },
  frame: { type: "string" },
  "telemetry-hz": { type: "string" },
  "help-rate": { type: "string" },
  wander: { type: "boolean" },
  "state-dir": { type: "string" },
  ephemeral: { type: "boolean" },
  "name-prefix": { type: "string" },
  help: { type: "boolean", short: "h" },
} as const;

let values: ReturnType<typeof parseArgs<{ options: typeof options }>>["values"];
try {
  ({ values } = parseArgs({ options, strict: true }));
} catch (err) {
  fail(err instanceof Error ? err.message : String(err));
}

if (values.help) {
  console.log(HELP);
  process.exit(0);
}

const url = values.url ?? process.env.FLEET_URL ?? "ws://localhost:8080/ws";
const enrollmentKey = values["enroll-key"] ?? process.env.FLEET_ENROLL_KEY;
const count = Math.floor(num("count", values.count, 5, 1));
const omni = values.omni === undefined ? undefined : Math.floor(num("omni", values.omni, 0));
const frame = values.frame ?? "geographic";
if (frame !== "geographic" && frame !== "local") fail("--frame must be geographic or local");

let origin = DEFAULT_ORIGIN;
if (values.origin !== undefined) {
  const [lat, lon] = values.origin.split(",").map((s) => Number(s.trim()));
  if (!Number.isFinite(lat) || !Number.isFinite(lon) || Math.abs(lat!) > 90 || Math.abs(lon!) > 180) {
    fail("--origin must be lat,lon (e.g. 38.6488,-90.3108)");
  }
  origin = { lat: lat!, lon: lon! };
}

const stateDir = values.ephemeral ? null : resolve(values["state-dir"] ?? ".sim-state");
const log = (line: string) => console.log(`${new Date().toISOString().slice(11, 23)} ${line}`);

log(`connecting ${count} robots to ${url}${stateDir ? ` (tokens in ${stateDir})` : " (ephemeral identities)"}`);

let fleet: Awaited<ReturnType<typeof startFleet>>;
try {
  fleet = await startFleet({
    url,
    ...(enrollmentKey !== undefined ? { enrollmentKey } : {}),
    count,
    ...(omni !== undefined ? { omni } : {}),
    drive: !values["no-drive"],
    p2p: !values["no-p2p"],
    frame,
    origin,
    spreadM: num("spread", values.spread, 120),
    telemetryHz: num("telemetry-hz", values["telemetry-hz"], 1, 0.1),
    helpRatePerMin: num("help-rate", values["help-rate"], 0),
    wander: values.wander ?? false,
    stateDir,
    namePrefix: values["name-prefix"] ?? "sim",
    log,
  });
} catch (err) {
  console.error(`sim: ${err instanceof Error ? err.message : String(err)}`);
  if (!enrollmentKey) console.error("sim: first run needs --enroll-key (or FLEET_ENROLL_KEY)");
  process.exit(1);
}

const omniCount = fleet.robots.filter((r) => r.morphology === "omni").length;
log(`${count} robots online (${count - omniCount} diff-drive, ${omniCount} omni)`);
for (const r of fleet.robots) log(`  ${r.name}  ${r.robotId}  ${r.morphology}`);

const status = setInterval(() => {
  const online = fleet.robots.filter((r) => r.client.state === "open").length;
  const teleop = fleet.robots.filter((r) => r.mode === "teleop").length;
  const help = fleet.robots.filter((r) => r.mode === "help").length;
  log(`status: ${online}/${count} online, ${teleop} teleop, ${help} awaiting help`);
}, 10_000);

const shutdown = () => {
  clearInterval(status);
  fleet.stop();
  log("stopped");
  process.exit(0);
};
// For showing the fallback: `kill -USR2 <pid>` drops every robot's WebRTC peer
// as a dead link would. Leases are untouched; consoles fall back to the bus.
process.on("SIGUSR2", () => {
  log("SIGUSR2: dropping WebRTC peers");
  for (const r of fleet.robots) r.dropPeer();
});
process.on("SIGINT", shutdown);
process.on("SIGTERM", shutdown);
