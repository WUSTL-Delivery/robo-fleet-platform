// An estimate of the server's clock, for showing how long a queue entry has
// waited. `requested_at_ms` is on the server's clock, and this browser's wall
// clock may be minutes off, so the wall clock is not used at all once the
// server has been heard from.
//
// Every server envelope carries its send time (`ts_ms`), and `welcome` carries
// `server_time_ms`. Each one is a sample: "the server's clock read S when this
// tab's monotonic clock (performance.now) read M". Server time is then
// S + (monotonic now - M), which ticks locally with no round trip and is
// unaffected by the wall clock being wrong or being changed.
//
// A sample is seen late by its network delay, so S - M is always a little
// under the true offset; the largest one seen is the closest. What is left is
// the delay of the fastest message received: waits read short by that much
// (milliseconds on a LAN), never long.

export interface ServerClock {
  /** Server epoch ms minus local monotonic ms, from the best sample; undefined before any. */
  offsetMs: number | undefined;
}

export const noClock: ServerClock = { offsetMs: undefined };

/** Folds in one sample: the server said `serverMs` when the monotonic clock read `monoMs`. */
export function observe(clock: ServerClock, serverMs: number, monoMs: number): ServerClock {
  if (!Number.isFinite(serverMs) || !Number.isFinite(monoMs)) return clock;
  const offsetMs = serverMs - monoMs;
  if (clock.offsetMs !== undefined && clock.offsetMs >= offsetMs) return clock;
  return { offsetMs };
}

/**
 * The server's clock now. Before the first sample there is nothing better
 * than the wall clock, so that is the fallback.
 */
export function serverNow(clock: ServerClock, monoMs: number, wallMs: number): number {
  return clock.offsetMs === undefined ? wallMs : clock.offsetMs + monoMs;
}
