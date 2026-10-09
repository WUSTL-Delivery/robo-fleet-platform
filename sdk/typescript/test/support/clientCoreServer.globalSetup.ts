// vitest globalSetup for the client integration suite: builds the real
// fleet-server binary from ../../server, starts it on a free loopback port with
// a temp sqlite db and a declared fleet + enrollment key, and hands the test
// files what they need via inject("clientCoreServer").
import { execFileSync, spawn, type ChildProcess } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { createServer } from "node:net";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import type { TestProject } from "vitest/node";

export interface ClientCoreServer {
  wsUrl: string;
  httpUrl: string;
  fleet: string;
  enrollKey: string;
  adminToken: string;
  heartbeatIntervalMs: number;
  /** ICE servers the server is configured with; nothing listens there, the SDK only has to be handed them. */
  stunUrls: string[];
  turnUrls: string[];
  turnSecret: string;
}

declare module "vitest" {
  export interface ProvidedContext {
    clientCoreServer: ClientCoreServer;
  }
}

const serverDir = fileURLToPath(new URL("../../../../server", import.meta.url));

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

export default async function setup(project: TestProject): Promise<() => Promise<void>> {
  const dir = mkdtempSync(join(tmpdir(), "fleet-sdk-client-core-"));
  const bin = join(dir, "fleet-server");
  execFileSync("go", ["build", "-o", bin, "./cmd/fleet-server"], { cwd: serverDir, stdio: "inherit" });

  const port = await freePort();
  const cfg: ClientCoreServer = {
    wsUrl: `ws://127.0.0.1:${port}/ws`,
    httpUrl: `http://127.0.0.1:${port}`,
    fleet: "sdk-client-core",
    enrollKey: "sdk-client-core-enroll-key-0123456789",
    adminToken: "sdk-client-core-admin-token-0123456789",
    // Short so a test can outlast several intervals and prove heartbeats keep it alive.
    heartbeatIntervalMs: 200,
    stunUrls: ["stun:127.0.0.1:3478"],
    turnUrls: ["turn:127.0.0.1:3478?transport=udp", "turn:127.0.0.1:3478?transport=tcp"],
    turnSecret: "sdk-client-core-turn-secret-0123456789",
  };

  let log = "";
  const proc = spawn(bin, [], {
    cwd: dir,
    env: {
      ...process.env,
      FLEET_LISTEN: `127.0.0.1:${port}`,
      FLEET_DB: join(dir, "fleet.db"),
      FLEET_HEARTBEAT_INTERVAL_MS: String(cfg.heartbeatIntervalMs),
      FLEET_SWEEP_MS: "50",
      FLEET_BOOTSTRAP_FLEET: cfg.fleet,
      FLEET_BOOTSTRAP_ENROLL_KEY: cfg.enrollKey,
      FLEET_ADMIN_TOKEN: cfg.adminToken,
      FLEET_STUN_URLS: cfg.stunUrls.join(","),
      FLEET_TURN_URLS: cfg.turnUrls.join(","),
      FLEET_TURN_SECRET: cfg.turnSecret,
      FLEET_TURN_CREDENTIAL_TTL_S: "600",
    },
    stdio: ["ignore", "pipe", "pipe"],
  });
  proc.stdout?.on("data", (b: Buffer) => (log += b.toString()));
  proc.stderr?.on("data", (b: Buffer) => (log += b.toString()));

  try {
    await waitHealthy(cfg.httpUrl, proc, () => log);
  } catch (err) {
    proc.kill("SIGKILL");
    rmSync(dir, { recursive: true, force: true });
    throw err;
  }
  project.provide("clientCoreServer", cfg);

  return async () => {
    if (proc.exitCode === null) {
      const exited = new Promise((r) => proc.once("exit", r));
      proc.kill("SIGTERM");
      await Promise.race([exited, new Promise((r) => setTimeout(r, 3000))]);
      if (proc.exitCode === null) proc.kill("SIGKILL");
    }
    rmSync(dir, { recursive: true, force: true });
  };
}
