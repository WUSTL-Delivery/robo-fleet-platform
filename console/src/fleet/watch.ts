// Tells the server which robot this console has open (`watch`), so the rest of
// the fleet sees it as operator.watching.
//
// The rules come from the protocol's "Watching a robot":
//   - send only the selection the operator settled on (rapid changes are
//     debounced), and never repeat what the server already has;
//   - watching belongs to the connection: a new connection watches nothing, so
//     the current selection is sent again on every open;
//   - `watch` is rate limited and a dropped one is answered at most once a
//     second: any rate_limited notice means "send the current selection again
//     after a pause";
//   - not_found means the robot is gone: stop naming it.
//
// No timers or sockets of its own beyond the injected ones, so it is tested
// with fake time (watch.test.ts). useWatch.ts binds it to the client.

export interface WatchSenderOptions {
  /** Sends one watch envelope. Returns false when the connection is not open. */
  send: (robotId: string | null, id: string) => boolean;
  debounceMs?: number;
  /** Pause before re-sending after rate_limited. The server's bucket refills in about a second. */
  retryMs?: number;
}

export const WATCH_DEBOUNCE_MS = 200;
export const WATCH_RETRY_MS = 1500;
const REF_PREFIX = "console.watch.";
/** How many sent envelopes are remembered to match an error's `ref`. */
const REMEMBERED = 8;

let seq = 0;

export class WatchSender {
  readonly #send: WatchSenderOptions["send"];
  readonly #debounceMs: number;
  readonly #retryMs: number;
  /** The robot the console is showing. */
  #desired: string | null = null;
  /** What the server holds for this connection; undefined when we cannot know. */
  #server: string | null | undefined = undefined;
  /** A robot the server said does not exist; not named again until the selection or the connection changes. */
  #refused: string | undefined;
  #sent = new Map<string, string | null>();
  #debounce: ReturnType<typeof setTimeout> | undefined;
  #pause: ReturnType<typeof setTimeout> | undefined;

  constructor(opts: WatchSenderOptions) {
    this.#send = opts.send;
    this.#debounceMs = opts.debounceMs ?? WATCH_DEBOUNCE_MS;
    this.#retryMs = opts.retryMs ?? WATCH_RETRY_MS;
  }

  /** The selection changed (null: nothing open). */
  select(robotId: string | null): void {
    if (robotId === this.#desired) return;
    this.#desired = robotId;
    this.#refused = undefined;
    clearTimeout(this.#debounce);
    this.#debounce = setTimeout(() => {
      this.#debounce = undefined;
      this.#flush();
    }, this.#debounceMs);
  }

  /** The connection opened (first connect and every reconnect): the server holds nothing for it. */
  opened(): void {
    this.#server = null;
    this.#refused = undefined;
    this.#sent.clear();
    clearTimeout(this.#pause);
    this.#pause = undefined;
    // A selection still settling is sent by its own timer.
    if (this.#debounce === undefined) this.#flush();
  }

  /** An `error` envelope from the server. Ignores every error that is not about one of our watches. */
  error(payload: { code: string; ref?: string | undefined }): void {
    const ref = payload.ref;
    if (!ref || !this.#sent.has(ref)) return;
    const robotId = this.#sent.get(ref);
    if (payload.code === "rate_limited") {
      // The server kept the last value that got through, and which that is we
      // cannot tell: notices are at most one a second.
      this.#server = undefined;
      if (this.#pause !== undefined) return;
      this.#pause = setTimeout(() => {
        this.#pause = undefined;
        this.#flush();
      }, this.#retryMs);
    } else if (payload.code === "not_found" && typeof robotId === "string") {
      // The previous value is still in place on the server; clear it rather
      // than be shown on a robot this console no longer has open.
      this.#refused = robotId;
      this.#server = undefined;
      this.#flush();
    }
  }

  dispose(): void {
    clearTimeout(this.#debounce);
    clearTimeout(this.#pause);
    this.#debounce = this.#pause = undefined;
  }

  #flush(): void {
    if (this.#pause !== undefined) return; // the pause ends in a flush
    const target = this.#desired !== null && this.#desired === this.#refused ? null : this.#desired;
    if (this.#server === target) return;
    const id = `${REF_PREFIX}${++seq}`;
    if (!this.#send(target, id)) return; // not open: opened() sends it
    this.#server = target;
    this.#sent.set(id, target);
    if (this.#sent.size > REMEMBERED) this.#sent.delete(this.#sent.keys().next().value!);
  }
}
