// The robot-side half of the trust model (docs/DESIGN.md D3), pure and clock-
// injected so it is unit-testable:
//
// - Twist is obeyed only when it bears the lease id this robot currently holds.
// - Deadman: ~300 ms without a valid twist → zero velocity. The robot fails
//   closed on its own; it never waits for the server to say the operator left.
// - Losing the lease or the link zeroes velocity immediately.

import { STOPPED, type Velocity } from "./kinematics.js";

export const DEADMAN_MS = 300;

export class TeleopGate {
  readonly deadmanMs: number;
  #leaseId: string | undefined;
  #cmd: Velocity = STOPPED;
  #cmdAtMs = Number.NEGATIVE_INFINITY;

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

  /** Stop now but keep the lease (the link blipped; the server still holds it). */
  halt(): void {
    this.#halt();
  }

  /** Offers a twist; returns whether it was accepted (it bears the current lease). */
  accept(leaseId: string, cmd: Velocity, nowMs: number): boolean {
    if (this.#leaseId === undefined || leaseId !== this.#leaseId) return false;
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
  }
}
