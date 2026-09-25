// Pluggable persistence for the credentials a client gets from enrollment.
//
// A token is minted once by `enroll.request` and must survive restarts: the
// server never shows it again, and re-enrolling mints a NEW identity (a new
// client_id). So the client loads from a TokenStore before connecting and only
// enrolls when the store is empty.
//
// Browser-safe: nothing here touches Node APIs. A file-backed store for Node
// lives in the separate `./node` entry.

/** What enrollment hands back and every later `hello` needs. */
export interface StoredCredentials {
  token: string;
  client_id: string;
  fleet_id: string;
}

/**
 * Where a client keeps its credentials between runs. Methods may be sync or
 * async. `load` returns null when nothing is stored (the client then enrolls).
 */
export interface TokenStore {
  load(): StoredCredentials | null | Promise<StoredCredentials | null>;
  save(credentials: StoredCredentials): void | Promise<void>;
  clear(): void | Promise<void>;
}

/** Keeps credentials in memory only: lost when the process or tab goes away. */
export class MemoryTokenStore implements TokenStore {
  #credentials: StoredCredentials | null;

  constructor(initial: StoredCredentials | null = null) {
    this.#credentials = initial;
  }

  load(): StoredCredentials | null {
    return this.#credentials;
  }

  save(credentials: StoredCredentials): void {
    this.#credentials = { ...credentials };
  }

  clear(): void {
    this.#credentials = null;
  }
}

/** The subset of the Web Storage API this store needs. */
export interface StorageLike {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
  removeItem(key: string): void;
}

/**
 * Keeps credentials in `localStorage` (or any Storage-like object) under one
 * key, as JSON. The console's default. A corrupt or foreign value reads as
 * "nothing stored".
 */
export class LocalStorageTokenStore implements TokenStore {
  readonly key: string;
  readonly #storage: StorageLike;

  constructor(key = "fleet-platform.credentials", storage?: StorageLike) {
    const s = storage ?? (globalThis as { localStorage?: StorageLike }).localStorage;
    if (!s) throw new Error("LocalStorageTokenStore: no localStorage available; pass a storage object");
    this.key = key;
    this.#storage = s;
  }

  load(): StoredCredentials | null {
    const raw = this.#storage.getItem(this.key);
    return raw === null ? null : parseCredentials(raw);
  }

  save(credentials: StoredCredentials): void {
    this.#storage.setItem(this.key, JSON.stringify(credentials));
  }

  clear(): void {
    this.#storage.removeItem(this.key);
  }
}

/** Parses stored JSON, returning null unless it has the three string fields. */
export function parseCredentials(raw: string): StoredCredentials | null {
  try {
    const v: unknown = JSON.parse(raw);
    if (typeof v !== "object" || v === null) return null;
    const { token, client_id, fleet_id } = v as Record<string, unknown>;
    if (typeof token !== "string" || typeof client_id !== "string" || typeof fleet_id !== "string") return null;
    return { token, client_id, fleet_id };
  } catch {
    return null;
  }
}
