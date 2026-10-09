// The robot-side half of the trust model (docs/DESIGN.md D3), pure and clock-
// injected so it is unit-testable:
//
// - Twist is obeyed only when it bears the lease id this robot currently holds.
// - Deadman: ~300 ms without a valid twist → zero velocity. The robot fails
//   closed on its own; it never waits for the server to say the operator left.
// - Losing the lease or the link zeroes velocity immediately.
// - A lease is never trusted across a reconnect: the welcome of each new
//   connection states the lease the server holds for this robot, and confirm()
//   replaces what the gate believed with it (protocol/README.md, "A robot's
//   lease at connect").
// - Twist can arrive two ways: over the control-plane bus, or over the direct
//   WebRTC data channel ("p2p"). The operator sends each twist on one of them.
//   A bus twist is ignored while a data-channel twist was accepted within the
//   deadman window, so a bus twist still in flight when the operator switched
//   to the faster path cannot overwrite a newer setpoint (protocol/README.md,
//   "Teleop data plane").

import { STOPPED, type Velocity } from "./kinematics.js";

export const DEADMAN_MS = 300;

/** How a twist reached the robot. */
export type TwistVia = "bus" | "p2p";

export class TeleopGate {
  readonly deadmanMs: number;
  #leaseId: string | undefined;
  #cmd: Velocity = STOPPED;
  #cmdAtMs = Number.NEGATIVE_INFINITY;
  #p2pAtMs = Number.NEGATIVE_INFINITY;

  constructor(deadmanMs = DEADMAN_MS) {
    this.deadmanMs = deadmanMs;
  }

  /** The lease this robot currently obeys, if any. */
  get leaseId(): string | undefined {
    return this.#leaseId;
  }

  /** lease.granted for this robot (a steal re-grants with a new id). */
  grant(leaseId: string): void {
    this.#leaseId = leaseId;
    this.#halt();
  }

  /** lease.revoked: clears the lease only if it is the one held. Returns whether it was. */
  revoke(leaseId: string): boolean {
    if (leaseId !== this.#leaseId) return false;
    this.clear();
    return true;
  }

  /** Forget any lease and stop. */
  clear(): void {
    this.#leaseId = undefined;
    this.#halt();
  }

  /**
   * Stop now but keep the lease id. The link blipped: the server probably
   * still holds the lease, but only the next welcome can say so; see confirm().
   */
  halt(): void {
    this.#halt();
  }

  /**
   * The lease the welcome of a new connection states for this robot
   * (`undefined` for null, and for a welcome that does not say: an older
   * server confirms nothing). The gate ends up holding exactly that:
   *
   * - "kept": the lease it already held; twists under it are obeyed again.
   * - "granted": a lease it did not hold, taken as grant() takes one.
   * - "dropped": it held one the server no longer does; cleared and stopped.
   * - "none": it held nothing and still does.
   */
  confirm(leaseId: string | undefined): "kept" | "granted" | "dropped" | "none" {
    if (leaseId === undefined) {
      if (this.#leaseId === undefined) return "none";
      this.clear();
      return "dropped";
    }
    if (leaseId === this.#leaseId) return "kept";
    this.grant(leaseId);
    return "granted";
  }

  /**
   * Offers a twist; returns whether it was accepted: it bears the current
   * lease and, on the bus, is not shadowed by a fresh data-channel twist.
   */
  accept(leaseId: string, cmd: Velocity, nowMs: number, via: TwistVia = "bus"): boolean {
    if (this.#leaseId === undefined || leaseId !== this.#leaseId) return false;
    if (via === "p2p") this.#p2pAtMs = nowMs;
    else if (nowMs - this.#p2pAtMs <= this.deadmanMs) return false;
    this.#cmd = cmd;
    this.#cmdAtMs = nowMs;
    return true;
  }

  /** The setpoint in force at `nowMs`: the last valid twist, or zero once the deadman has fired. */
  current(nowMs: number): Velocity {
    if (this.#leaseId === undefined) return STOPPED;
    if (nowMs - this.#cmdAtMs > this.deadmanMs) return STOPPED;
    return this.#cmd;
  }

  #halt(): void {
    this.#cmd = STOPPED;
    this.#cmdAtMs = Number.NEGATIVE_INFINITY;
    this.#p2pAtMs = Number.NEGATIVE_INFINITY;
  }
}
