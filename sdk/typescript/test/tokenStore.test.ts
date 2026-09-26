import { mkdtempSync, readFileSync, rmSync, statSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { LocalStorageTokenStore, MemoryTokenStore, type StorageLike } from "../src/index.js";
import { FileTokenStore } from "../src/node.js";

const creds = { token: "fp-tk-abc", client_id: "s_1", fleet_id: "f_1" };

function fakeStorage(): StorageLike & { data: Map<string, string> } {
  const data = new Map<string, string>();
  return {
    data,
    getItem: (k) => data.get(k) ?? null,
    setItem: (k, v) => void data.set(k, v),
    removeItem: (k) => void data.delete(k),
  };
}

describe("token stores", () => {
  it("MemoryTokenStore round-trips", () => {
    const s = new MemoryTokenStore();
    expect(s.load()).toBeNull();
    s.save(creds);
    expect(s.load()).toEqual(creds);
    s.clear();
    expect(s.load()).toBeNull();
  });

  it("LocalStorageTokenStore round-trips and ignores garbage", () => {
    const storage = fakeStorage();
    const s = new LocalStorageTokenStore("k", storage);
    expect(s.load()).toBeNull();
    s.save(creds);
    expect(JSON.parse(storage.data.get("k")!)).toEqual(creds);
    expect(new LocalStorageTokenStore("k", storage).load()).toEqual(creds);
    storage.data.set("k", "{not json");
    expect(s.load()).toBeNull();
    s.clear();
    expect(storage.data.has("k")).toBe(false);
  });

  it("FileTokenStore round-trips with owner-only permissions", async () => {
    const dir = mkdtempSync(join(tmpdir(), "fleet-sdk-token-"));
    try {
      const path = join(dir, "nested", "creds.json");
      const s = new FileTokenStore(path);
      expect(await s.load()).toBeNull();
      await s.save(creds);
      expect(JSON.parse(readFileSync(path, "utf8"))).toEqual(creds);
      expect(statSync(path).mode & 0o777).toBe(0o600);
      expect(await new FileTokenStore(path).load()).toEqual(creds);
      writeFileSync(path, "garbage");
      expect(await s.load()).toBeNull();
      await s.clear();
      expect(await s.load()).toBeNull();
    } finally {
      rmSync(dir, { recursive: true, force: true });
    }
  });
});
