// WatchSender against fake time: what goes on the wire, and when.
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { WATCH_DEBOUNCE_MS, WATCH_RETRY_MS, WatchSender } from "./watch";

interface Sent {
  robotId: string | null;
  id: string;
}

function harness() {
  const sent: Sent[] = [];
  const wire = { open: true };
  const sender = new WatchSender({
    send: (robotId, id) => {
      if (!wire.open) return false;
      sent.push({ robotId, id });
      return true;
    },
  });
  const settle = () => vi.advanceTimersByTime(WATCH_DEBOUNCE_MS);
  return { sent, wire, sender, settle, last: () => sent[sent.length - 1]! };
}

beforeEach(() => vi.useFakeTimers());
afterEach(() => vi.useRealTimers());

describe("WatchSender", () => {
  it("sends nothing while nothing is selected", () => {
    const h = harness();
    h.sender.opened();
    h.settle();
    expect(h.sent).toEqual([]);
  });

  it("sends the selection once it settles, with an envelope id", () => {
    const h = harness();
    h.sender.opened();
    h.sender.select("r1");
    expect(h.sent).toEqual([]);
    h.settle();
    expect(h.sent.map((s) => s.robotId)).toEqual(["r1"]);
    expect(h.last().id).toMatch(/\S/);
  });

  it("debounces rapid changes down to the one the operator settled on", () => {
    const h = harness();
    h.sender.opened();
    h.sender.select("r1");
    vi.advanceTimersByTime(WATCH_DEBOUNCE_MS / 2);
    h.sender.select("r2");
    vi.advanceTimersByTime(WATCH_DEBOUNCE_MS / 2);
    h.sender.select("r3");
    h.settle();
    expect(h.sent.map((s) => s.robotId)).toEqual(["r3"]);
  });

  it("does not repeat what the server already has", () => {
    const h = harness();
    h.sender.opened();
    h.sender.select("r1");
    h.settle();
    h.sender.select("r2");
    h.sender.select("r1"); // back before it settled
    h.settle();
    expect(h.sent.map((s) => s.robotId)).toEqual(["r1"]);
  });

  it("clears with null when the selection closes", () => {
    const h = harness();
    h.sender.opened();
    h.sender.select("r1");
    h.settle();
    h.sender.select(null);
    h.settle();
    expect(h.sent.map((s) => s.robotId)).toEqual(["r1", null]);
  });

  it("sends the selection again on every open, at once, and nothing when there is none", () => {
    const h = harness();
    h.sender.opened();
    h.sender.select("r1");
    h.settle();
    h.sender.opened(); // reconnect: the new connection watches nothing
    expect(h.sent.map((s) => s.robotId)).toEqual(["r1", "r1"]);
    h.sender.select(null);
    h.settle();
    h.sender.opened();
    expect(h.sent.map((s) => s.robotId)).toEqual(["r1", "r1", null]);
  });

  it("a selection made while disconnected goes out when the connection opens", () => {
    const h = harness();
    h.wire.open = false;
    h.sender.select("r1");
    h.settle();
    expect(h.sent).toEqual([]);
    h.wire.open = true;
    h.sender.opened();
    expect(h.sent.map((s) => s.robotId)).toEqual(["r1"]);
  });

  it("re-sends the current selection after a pause when rate limited", () => {
    const h = harness();
    h.sender.opened();
    h.sender.select("r1");
    h.settle();
    const dropped = h.last().id;
    h.sender.select("r2");
    h.settle();
    h.sender.error({ code: "rate_limited", ref: dropped }); // which value got through is unknown
    vi.advanceTimersByTime(WATCH_RETRY_MS - 1);
    expect(h.sent.map((s) => s.robotId)).toEqual(["r1", "r2"]);
    vi.advanceTimersByTime(1);
    expect(h.sent.map((s) => s.robotId)).toEqual(["r1", "r2", "r2"]);
  });

  it("holds new selections until the rate-limit pause is over, then sends only the latest", () => {
    const h = harness();
    h.sender.opened();
    h.sender.select("r1");
    h.settle();
    h.sender.error({ code: "rate_limited", ref: h.last().id });
    h.sender.select("r2");
    h.settle();
    h.sender.select("r3");
    h.settle();
    expect(h.sent.map((s) => s.robotId)).toEqual(["r1"]);
    vi.advanceTimersByTime(WATCH_RETRY_MS);
    expect(h.sent.map((s) => s.robotId)).toEqual(["r1", "r3"]);
  });

  it("stops naming a robot the server says is gone, and clears the old value", () => {
    const h = harness();
    h.sender.opened();
    h.sender.select("r1");
    h.settle();
    h.sender.select("r_gone");
    h.settle();
    h.sender.error({ code: "not_found", ref: h.last().id });
    expect(h.sent.map((s) => s.robotId)).toEqual(["r1", "r_gone", null]);
    vi.advanceTimersByTime(10 * WATCH_RETRY_MS);
    expect(h.sent).toHaveLength(3);
    h.sender.select("r2"); // a new selection is sent as usual
    h.settle();
    expect(h.last().robotId).toBe("r2");
  });

  it("ignores errors that are not about its own watches", () => {
    const h = harness();
    h.sender.opened();
    h.sender.select("r1");
    h.settle();
    h.sender.error({ code: "rate_limited", ref: "teleop.twist.4" });
    h.sender.error({ code: "not_found", ref: "teleop.claim.1" });
    h.sender.error({ code: "conflict" });
    vi.advanceTimersByTime(10 * WATCH_RETRY_MS);
    expect(h.sent.map((s) => s.robotId)).toEqual(["r1"]);
  });

  it("sends nothing after dispose", () => {
    const h = harness();
    h.sender.opened();
    h.sender.select("r1");
    h.sender.dispose();
    vi.advanceTimersByTime(10 * WATCH_RETRY_MS);
    expect(h.sent).toEqual([]);
  });
});
