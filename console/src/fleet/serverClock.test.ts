import { describe, expect, it } from "vitest";
import { noClock, observe, serverNow } from "./serverClock";

const SERVER = 1_755_100_000_000;

describe("serverClock", () => {
  it("falls back to the wall clock before any sample", () => {
    expect(serverNow(noClock, 5000, 123)).toBe(123);
  });

  it("ticks with the monotonic clock and ignores the wall clock once sampled", () => {
    // The server said SERVER when the tab's monotonic clock read 1000.
    const clock = observe(noClock, SERVER, 1000);
    // 30 s later, with a wall clock that is an hour wrong.
    expect(serverNow(clock, 31_000, SERVER + 3_600_000)).toBe(SERVER + 30_000);
  });

  it("keeps the sample with the least delay", () => {
    // Sent at SERVER, seen 400 ms late.
    let clock = observe(noClock, SERVER, 1400);
    expect(serverNow(clock, 1400, 0)).toBe(SERVER);
    // Sent 1 s later, seen only 20 ms late: closer to the truth.
    clock = observe(clock, SERVER + 1000, 2020);
    expect(serverNow(clock, 2020, 0)).toBe(SERVER + 1000);
    // A slower one afterwards does not drag the estimate back.
    clock = observe(clock, SERVER + 2000, 3300);
    expect(serverNow(clock, 3300, 0)).toBe(SERVER + 2280);
  });

  it("trails the server by at most the delay of its best sample, never leads", () => {
    const delay = 20;
    const clock = observe(noClock, SERVER, 1000 + delay);
    const trueServerNow = SERVER + 5000;
    const estimate = serverNow(clock, 1000 + 5000, 0);
    expect(trueServerNow - estimate).toBe(delay);
  });

  it("ignores a sample that is not a number", () => {
    const clock = observe(noClock, SERVER, 1000);
    expect(observe(clock, Number.NaN, 2000)).toBe(clock);
  });
});
