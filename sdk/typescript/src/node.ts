// Node-only helpers, published as the separate `./node` entry so the main entry
// stays browser-safe. Import from "@fleet-platform/sdk/node".

import { mkdir, readFile, rename, rm, writeFile } from "node:fs/promises";
import { dirname } from "node:path";
import { parseCredentials, type StoredCredentials, type TokenStore } from "./tokenStore.js";

/**
 * Keeps credentials in a JSON file (mode 0600), for services and sim robots
 * that must keep their identity across restarts. A missing or corrupt file
 * reads as "nothing stored". Writes go through a temp file + rename so a crash
 * never leaves a half-written token.
 */
export class FileTokenStore implements TokenStore {
  constructor(readonly path: string) {}

  async load(): Promise<StoredCredentials | null> {
    try {
      return parseCredentials(await readFile(this.path, "utf8"));
    } catch (err) {
      if ((err as NodeJS.ErrnoException).code === "ENOENT") return null;
      throw err;
    }
  }

  async save(credentials: StoredCredentials): Promise<void> {
    await mkdir(dirname(this.path), { recursive: true });
    const tmp = `${this.path}.${process.pid}.tmp`;
    await writeFile(tmp, JSON.stringify(credentials, null, 2) + "\n", { mode: 0o600 });
    await rename(tmp, this.path);
  }

  async clear(): Promise<void> {
    await rm(this.path, { force: true });
  }
}
