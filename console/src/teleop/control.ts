// Who has the wheel, as this console sees it, and what to tell the operator
// when that changes.
//
// Authority is the server's lease (exclusive: a steal revokes one lease and
// issues another, it never shares). This file only reads it: the robot's lease
// in the fleet state says who is driving, this console's own teleop phase says
// whether it is that driver. Everything here is pure; useTeleop.ts holds the
// lease and the timers, TeleopPanel.tsx renders the result.
import type { ErrorPayload, Lease, LeaseRevokedPayload } from "@fleet-platform/sdk";
import type { RobotView } from "../fleet/model";

export type TeleopPhase = "idle" | "claiming" | "driving";

export type ControlMode =
  /** This console holds the lease and is sending twist. */
  | "driving"
  /** A claim is out and not answered yet. */
  | "claiming"
  /** Another operator holds the lease: read-only. The only way in is an explicit steal. */
  | "spectating"
  /** The lease is this operator's, but this console is not holding it (another tab, or a reload). */
  | "held"
  /**
   * This console just gave up or lost its lease and the fleet state has not
   * caught up yet (it still lists that lease). Lasts until the next event.
   */
  | "settling"
  /** Nobody is driving and the robot is reachable. */
  | "free"
  /** Nobody is driving and the robot is offline. */
  | "offline";

export interface Control {
  mode: ControlMode;
  /** The other operator holding the lease, in "spectating"; also while a steal from them is out. */
  driverId?: string;
}

/**
 * `endedLeaseId` is the lease this console last held and no longer does (see
 * useTeleop): while the fleet state still shows that one, it is behind, and
 * the lease must not be offered back as one to resume.
 */
export function controlOf(
  robot: RobotView,
  operatorId: string | undefined,
  phase: TeleopPhase,
  endedLeaseId?: string,
): Control {
  const holder = robot.lease?.operator_id;
  const other = holder !== undefined && holder !== operatorId ? holder : undefined;
  if (phase === "driving") return { mode: "driving" };
  if (phase === "claiming") return other ? { mode: "claiming", driverId: other } : { mode: "claiming" };
  if (other) return { mode: "spectating", driverId: other };
  if (robot.lease && robot.lease.lease_id === endedLeaseId) return { mode: "settling" };
  if (holder !== undefined) return { mode: "held" };
  return { mode: robot.presence === "online" ? "free" : "offline" };
}

/** Why control was lost or a claim failed. Operators are kept as ids; the name is looked up when it is shown. */
export type TeleopNotice =
  /** Another operator took the lease. `by` is filled in once the fleet state shows the new holder. */
  | { kind: "stolen"; by?: string }
  /** A plain claim lost to the operator who holds the robot. */
  | { kind: "beaten"; by: string }
  | { kind: "text"; text: string };

const text = (t: string): TeleopNotice => ({ kind: "text", text: t });

const REVOKED: Record<Exclude<LeaseRevokedPayload["reason"], "stolen">, string> = {
  expired: "The lease expired. The robot is back in the help queue.",
  operator_lost: "The server lost this console's connection and revoked the lease.",
  released: "Control handed back.",
};

/** The notice for a lease.revoked on the lease this console held. */
export function revokedNotice(reason: LeaseRevokedPayload["reason"]): TeleopNotice | undefined {
  if (reason === "stolen") return { kind: "stolen" };
  const t = REVOKED[reason];
  return t ? text(t) : undefined;
}

/**
 * The notice for an error answering this console's lease.claim. A conflict
 * that carries the lease in the way is another operator holding the robot:
 * nothing changed, and the notice names them.
 */
export function claimRefusedNotice(error: Pick<ErrorPayload, "code" | "message" | "lease">): TeleopNotice {
  if (error.code === "conflict" && error.lease?.operator_id) return { kind: "beaten", by: error.lease.operator_id };
  return text(`Take over refused: ${error.message}.`);
}

/**
 * Names the operator who took control, once known. lease.revoked does not say
 * who stole the lease; the robot's next lease in the fleet state does. Only
 * the first new holder is recorded, so a later change of driver does not
 * rewrite who took it from us.
 */
export function withTaker(
  notice: TeleopNotice | undefined,
  lease: Lease | undefined,
  operatorId: string | undefined,
): TeleopNotice | undefined {
  if (notice?.kind !== "stolen" || notice.by !== undefined) return notice;
  if (!lease || lease.operator_id === operatorId) return notice;
  return { kind: "stolen", by: lease.operator_id };
}

/**
 * The notice in words. It says what happened, not what is true now (the status
 * line above it does that), so it stays right after the robot changes hands again.
 */
export function noticeText(notice: TeleopNotice, nameOf: (operatorId: string) => string): string {
  switch (notice.kind) {
    case "stolen":
      return `${notice.by ? nameOf(notice.by) : "Another operator"} took control from you.`;
    case "beaten":
      return `${nameOf(notice.by)} claimed it first.`;
    case "text":
      return notice.text;
  }
}
