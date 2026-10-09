// vitest globalSetup for the sim integration suite: builds the real
// fleet-server binary from the repo's server/, starts it on a free loopback port with
// a temp sqlite db and a declared fleet + enrollment key, and hands the test
// files what they need via inject("fleetServer"). A second server from the same
// binary, inject("shortLeaseServer"), expires leases after two seconds, for the
// tests that need a lease to run out.
import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import type { TestProject } from "vitest/node";

export interface FleetServer {
  wsUrl: string;
  httpUrl: string;
  fleet: string;
  enrollKey: string;
  adminToken: string;
  heartbeatIntervalMs: number;
  /** How long a lease lives without a renewal (the server's lease_ttl_ms). */
  leaseTtlMs: number;
}

declare module "vitest" {
  export interface ProvidedContext {
    fleetServer: FleetServer;
    shortLeaseServer: FleetServer;
  }
}

const serverDir = fileURLToPath(new URL("../../../server", import.meta.url));

function freePort(): Promise<number> {
  return new Promise((resolve, reject) => {
    const srv = createServer();
    srv.once("error", reject);
    srv.listen(0, "127.0.0.1", () => {
      const addr = srv.address();
      if (typeof addr !== "object" || addr === null) return reject(new Error("no port"));
      srv.close(() => resolve(addr.port));
    });
  });
}

async function waitHealthy(httpUrl: string, proc: ChildProcess, output: () => string): Promise<void> {
  const deadline = Date.now() + 15_000;
  while (Date.now() < deadline) {
    if (proc.exitCode !== null) throw new Error(`fleet-server exited ${proc.exitCode}:\n${output()}`);
    try {
      const res = await fetch(`${httpUrl}/healthz`);
      if (res.ok) return;
    } catch {
      /* not up yet */
    }
    await new Promise((r) => setTimeout(r, 50));
  }
  throw new Error(`fleet-server not healthy in 15s:\n${output()}`);
}

/** Starts the binary on a free port with its own database. Returns its config and how to stop it. */
async function start(bin: string, fleet: string, leaseTtlMs: number): Promise<{ cfg: FleetServer; stop: () => Promise<void> }> {
  const dir = mkdtempSync(join(tmpdir(), "fleet-sim-"));
  const port = await freePort();
  const cfg: FleetServer = {
    wsUrl: `ws://127.0.0.1:${port}/ws`,
    httpUrl: `http://127.0.0.1:${port}`,
    fleet,
    enrollKey: "sim-enroll-key-0123456789",
    adminToken: "sim-admin-token-0123456789",
    heartbeatIntervalMs: 500,
    leaseTtlMs,
  };

  let log = "";
  const proc = spawn(bin, [], {
    cwd: dir,
    env: {
      ...process.env,
      FLEET_LISTEN: `127.0.0.1:${port}`,
      FLEET_DB: join(dir, "fleet.db"),
      FLEET_HEARTBEAT_INTERVAL_MS: String(cfg.heartbeatIntervalMs),
      FLEET_LEASE_TTL_MS: String(leaseTtlMs),
      FLEET_SWEEP_MS: "50",
      FLEET_BOOTSTRAP_FLEET: cfg.fleet,
      FLEET_BOOTSTRAP_ENROLL_KEY: cfg.enrollKey,
      FLEET_ADMIN_TOKEN: cfg.adminToken,
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  proc.stdout?.on("data", (b: Buffer) => (log += b.toString()));
  proc.stderr?.on("data", (b: Buffer) => (log += b.toString()));

  const stop = async () => {
    if (proc.exitCode === null) {
      const exited = new Promise((r) => proc.once("exit", r));
      proc.kill("SIGTERM");
      await Promise.race([exited, new Promise((r) => setTimeout(r, 3000))]);
      if (proc.exitCode === null) proc.kill("SIGKILL");
    }
    rmSync(dir, { recursive: true, force: true });
  };
  try {
    await waitHealthy(cfg.httpUrl, proc, () => log);
  } catch (err) {
    proc.kill("SIGKILL");
    rmSync(dir, { recursive: true, force: true });
    throw err;
  }
  return { cfg, stop };
}

export default async function setup(project: TestProject): Promise<() => Promise<void>> {
  const binDir = mkdtempSync(join(tmpdir(), "fleet-sim-bin-"));
  const bin = join(binDir, "fleet-server");
  execFileSync("go", ["build", "-o", bin, "./cmd/fleet-server"], { cwd: serverDir, stdio: "inherit" });

  // 15 s is the server's default lease TTL.
  const main = await start(bin, "sim", 15_000);
  let short: Awaited<ReturnType<typeof start>>;
  try {
    short = await start(bin, "sim-short-lease", 2_000);
  } catch (err) {
    await main.stop();
    rmSync(binDir, { recursive: true, force: true });
    throw err;
  }
  project.provide("fleetServer", main.cfg);
  project.provide("shortLeaseServer", short.cfg);

  return async () => {
    await Promise.all([main.stop(), short.stop()]);
    rmSync(binDir, { recursive: true, force: true });
  };
}
