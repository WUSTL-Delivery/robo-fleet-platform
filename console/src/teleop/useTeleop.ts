// Teleop for one robot: take over (lease.claim), drive (twist at 10 Hz from
// the keyboard), hand back (lease.release).
//
// The console sends intent, not actuation: a body-frame velocity setpoint
// (W/S = ±linear.x, A/D = ±angular.z), clamped to what the robot's manifest
// declares. The robot realizes it and owns the safety: it obeys twist only
// under its current lease and stops on its own deadman when twists stop
// arriving (tab closed, link dropped). This hook only has to be honest:
//   - while the lease is held, renew it well before it expires;
//   - send twist only while a key is held, and one zero twist on release of
//     the last key, on window blur and when the tab is hidden, so a
//     backgrounded tab never leaves a key "held";
//   - the moment the lease is gone (revoked: stolen, expired, operator lost;
//     or the fleet state shows someone else holding it), stop sending and fall
//     back to read-only.
//
// Watching another operator drive is read-only by construction, not by a
// disabled button: without a lease of its own this hook has no transport open
// and no key handler bound (both exist only in the "driving" phase), so there
// is nothing a spectator's console could send. Taking over from the driver is
// one explicit message, lease.claim with `steal`, and the server decides it.
//
// Which wire a twist rides (the direct WebRTC data channel, or the server bus
// as the fallback) is twistTransport.ts's business: one transport is opened
// per lease and closed when the lease ends. This hook only hands it setpoints.
import { useCallback, useEffect, useRef, useState } from "react";
import type { FleetClient, Lease } from "@fleet-platform/sdk";
import type { RobotView } from "../fleet/model";
import { claimRefusedNotice, revokedNotice, withTaker, type TeleopNotice, type TeleopPhase } from "./control";
import { BUS_ONLY, openTwistTransport, type TwistLinkStatus, type TwistSetpoint, type TwistTransport } from "./twistTransport";

export type { TeleopPhase } from "./control";

export type DriveKey = "forward" | "back" | "left" | "right";

export interface TwistCommand {
  vx: number;
  wz: number;
}

export interface Teleop {
  phase: TeleopPhase;
  /** The lease this console holds, while driving. */
  lease: Lease | undefined;
  /** The lease this console last held and no longer does (handed back, revoked, lost). */
  endedLeaseId: string | undefined;
  /** Keys held right now (for the on-screen pad). */
  held: ReadonlySet<DriveKey>;
  /** The setpoint being sent right now (zero when no key is held). */
  command: TwistCommand;
  /** Which transport twist is riding right now, and the state of the direct link. */
  link: TwistLinkStatus;
  /** Fraction of the manifest's limits a full key press asks for, 0.1 to 1. */
  speed: number;
  setSpeed: (s: number) => void;
  /** Why the last attempt failed or control was lost; cleared on the next take over. */
  notice: TeleopNotice | undefined;
  /**
   * Claims the robot. A plain claim never takes it from another operator (the
   * server refuses with who holds it); `steal` does, revoking their lease.
   */
  takeOver: (opts?: { steal?: boolean }) => void;
  release: () => void;
}

/** Twist cadence while a key is held. The robot's deadman is ~300 ms. */
export const TWIST_HZ = 10;
const CLAIM_TIMEOUT_MS = 5000;
const DEFAULT_SPEED = 0.5;
const STOP: TwistSetpoint = { linear: { x_mps: 0 }, angular: { z_radps: 0 } };

const KEYS: Record<string, DriveKey> = {
  KeyW: "forward",
  ArrowUp: "forward",
  KeyS: "back",
  ArrowDown: "back",
  KeyA: "left",
  ArrowLeft: "left",
  KeyD: "right",
  ArrowRight: "right",
};

const say = (text: string): TeleopNotice => ({ kind: "text", text });

/** Keys to a body-frame twist, clamped to the manifest's limits. */
export function twistFor(held: ReadonlySet<DriveKey>, speed: number, maxV: number, maxW: number): TwistCommand {
  const lin = (held.has("forward") ? 1 : 0) - (held.has("back") ? 1 : 0);
  const ang = (held.has("left") ? 1 : 0) - (held.has("right") ? 1 : 0);
  const s = clamp(speed, 0, 1);
  return { vx: clamp(lin * s * maxV, -maxV, maxV), wz: clamp(ang * s * maxW, -maxW, maxW) };
}

function clamp(x: number, lo: number, hi: number): number {
  return Math.min(hi, Math.max(lo, x)) || 0; // `|| 0` folds -0 into 0
}

function typingInto(target: EventTarget | null): boolean {
  if (!(target instanceof HTMLElement)) return false;
  if (target.isContentEditable) return true;
  const tag = target.tagName;
  if (tag === "TEXTAREA" || tag === "SELECT") return true;
  // A range slider is fine to drive over; text fields are not.
  return tag === "INPUT" && (target as HTMLInputElement).type !== "range";
}

let seq = 0;
const nextId = (kind: string) => `teleop.${kind}.${++seq}`;

/**
 * `robot` must declare a twist drive; the caller renders the teleop section
 * only then, keyed by robot id, so a change of robot unmounts this hook
 * (which hands back any lease it holds).
 */
export function useTeleop(client: FleetClient, robot: RobotView, operatorId: string | undefined): Teleop {
  const robotId = robot.robot_id;
  const drive = robot.manifest?.drive;
  const maxV = drive?.max_v_mps ?? 0;
  const maxW = drive?.max_w_radps ?? 0;

  const [phase, setPhaseState] = useState<TeleopPhase>("idle");
  const [lease, setLeaseState] = useState<Lease | undefined>();
  const [endedLeaseId, setEndedLeaseId] = useState<string | undefined>();
  const [held, setHeld] = useState<ReadonlySet<DriveKey>>(new Set());
  const [speed, setSpeedState] = useState(DEFAULT_SPEED);
  const [notice, setNotice] = useState<TeleopNotice | undefined>();
  const [link, setLink] = useState<TwistLinkStatus>(BUS_ONLY);

  // Timers and socket handlers read these, not the render-time values.
  const phaseRef = useRef<TeleopPhase>("idle");
  const leaseRef = useRef<Lease | undefined>(undefined);
  const ttlRef = useRef(0);
  const heldRef = useRef<Set<DriveKey>>(new Set());
  const speedRef = useRef(DEFAULT_SPEED);
  const limitsRef = useRef({ maxV, maxW });
  limitsRef.current = { maxV, maxW };
  const pendingRef = useRef<{ claim?: string; release?: string }>({});
  const seenInFleetRef = useRef(false);
  const linkRef = useRef<TwistTransport | undefined>(undefined);

  const setPhase = useCallback((p: TeleopPhase) => {
    phaseRef.current = p;
    setPhaseState(p);
  }, []);
  const trySend: FleetClient["send"] = useCallback(
    ((type, payload, opts) => {
      try {
        return client.send(type, payload, opts);
      } catch {
        return undefined; // not open: the reconnect path (snapshot) reconciles
      }
    }) as FleetClient["send"],
    [client],
  );

  /**
   * The lease this console holds changed. A new lease (never a renewal) gets
   * its own twist transport; losing the lease, however it happened, closes it.
   * Closing the peer connection is cleanup: the lease is already decided.
   */
  const setLease = useCallback(
    (l: Lease | undefined) => {
      if (!l && leaseRef.current) setEndedLeaseId(leaseRef.current.lease_id);
      leaseRef.current = l;
      seenInFleetRef.current = false;
      setLeaseState(l);
      linkRef.current?.close();
      linkRef.current = l
        ? openTwistTransport({
            client,
            robotId,
            leaseId: l.lease_id,
            sendBus: (payload) => trySend("twist", payload, { id: nextId("twist") }),
            onStatus: setLink,
            // Asked before every offer: the TURN credential is short-lived. It
            // never rejects; with no answer the peer gets no servers.
            iceServers: () => client.iceServers(),
          })
        : undefined;
      setLink(linkRef.current?.status ?? BUS_ONLY);
    },
    [client, robotId, trySend],
  );

  const sendTwist = useCallback(() => {
    if (!leaseRef.current || phaseRef.current !== "driving") return;
    const { maxV: v, maxW: w } = limitsRef.current;
    const cmd = twistFor(heldRef.current, speedRef.current, v, w);
    linkRef.current?.send({ linear: { x_mps: cmd.vx }, angular: { z_radps: cmd.wz } });
  }, []);

  const setKeys = useCallback(
    (next: Set<DriveKey>) => {
      const wasMoving = heldRef.current.size > 0;
      heldRef.current = next;
      setHeld(new Set(next));
      // Respond at once rather than on the next tick; the zero on the last
      // key up is the "stop" the operator asked for.
      if (next.size > 0 || wasMoving) sendTwist();
    },
    [sendTwist],
  );

  const stopKeys = useCallback(() => {
    if (heldRef.current.size === 0) return;
    setKeys(new Set());
  }, [setKeys]);

  /** Control is gone (revoked, refused, or seen held by someone else). */
  const drop = useCallback(
    (why: TeleopNotice | undefined) => {
      heldRef.current = new Set();
      setHeld(new Set());
      pendingRef.current = {};
      setLease(undefined);
      setPhase("idle");
      setNotice(why);
    },
    [setLease, setPhase],
  );

  // Replies from the server: grants (claim and renew), revocations, errors.
  useEffect(() => {
    const offGranted = client.on("lease.granted", ({ payload, ts_ms }) => {
      if (payload.robot_id !== robotId || (operatorId && payload.operator_id !== operatorId)) return;
      const cur = leaseRef.current;
      const isRenewal = cur?.lease_id === payload.lease_id;
      if (!isRenewal && phaseRef.current !== "claiming") return;
      ttlRef.current = Math.max(0, payload.expires_at_ms - (ts_ms ?? Date.now()));
      if (!isRenewal) {
        pendingRef.current.claim = undefined;
        setLease(payload);
        setPhase("driving");
        setNotice(undefined);
      } else {
        leaseRef.current = payload;
        setLeaseState(payload);
      }
    });
    const offRevoked = client.on("lease.revoked", ({ payload }) => {
      if (payload.lease_id !== leaseRef.current?.lease_id) return;
      drop(revokedNotice(payload.reason));
    });
    const offError = client.on("error", ({ payload }) => {
      const ref = payload.ref;
      if (!ref || !ref.startsWith("teleop.")) return;
      if (ref === pendingRef.current.claim) {
        // A conflict is another operator holding the robot: nothing changed,
        // and the notice names them from the lease the error carries.
        drop(claimRefusedNotice(payload));
      } else if (ref === pendingRef.current.release) {
        // The lease was already gone; either way we no longer hold it.
        drop(undefined);
      } else if (leaseRef.current && (ref.startsWith("teleop.twist.") || ref.startsWith("teleop.renew."))) {
        drop(say(`Control lost: ${payload.message}.`));
      }
    });
    return () => {
      offGranted();
      offRevoked();
      offError();
    };
  }, [client, robotId, operatorId, drop, setLease, setPhase]);

  // The fleet state is the second witness: once it has shown our lease, a
  // different lease or none at all (e.g. the snapshot after a reconnect)
  // means we lost it even if the revocation never reached us.
  const fleetLeaseId = robot.lease?.lease_id;
  useEffect(() => {
    const mine = leaseRef.current;
    if (!mine || phaseRef.current !== "driving") return;
    if (fleetLeaseId === mine.lease_id) {
      seenInFleetRef.current = true;
      return;
    }
    if (fleetLeaseId !== undefined) drop({ kind: "stolen" });
    else if (seenInFleetRef.current) drop(say("Control lost: the robot is no longer leased to this console."));
  }, [fleetLeaseId, drop]);

  // lease.revoked does not say who stole the lease; the robot's next lease does.
  const fleetLease = robot.lease;
  useEffect(() => {
    setNotice((n) => withTaker(n, fleetLease, operatorId));
  }, [fleetLease, operatorId, notice]);

  // A robot going offline cannot be driven; stop sending.
  useEffect(() => {
    if (robot.presence === "offline") stopKeys();
  }, [robot.presence, stopKeys]);

  // Claim that is never answered.
  useEffect(() => {
    if (phase !== "claiming") return;
    const t = setTimeout(() => {
      if (phaseRef.current === "claiming") drop(say("Take over timed out: no answer from the server."));
    }, CLAIM_TIMEOUT_MS);
    return () => clearTimeout(t);
  }, [phase, drop]);

  // While driving: twist at TWIST_HZ while a key is held, renew at a third of
  // the lease's TTL, and follow the keyboard.
  useEffect(() => {
    if (phase !== "driving") return;
    const tick = setInterval(() => {
      if (heldRef.current.size > 0) sendTwist();
    }, 1000 / TWIST_HZ);
    const renewEvery = Math.min(5000, Math.max(250, ttlRef.current / 3 || 5000));
    const renew = setInterval(() => {
      const l = leaseRef.current;
      if (l) trySend("lease.renew", { lease_id: l.lease_id }, { id: nextId("renew") });
    }, renewEvery);

    const onDown = (e: KeyboardEvent) => {
      const k = KEYS[e.code];
      if (!k || e.metaKey || e.ctrlKey || e.altKey || typingInto(e.target)) return;
      e.preventDefault();
      if (heldRef.current.has(k)) return; // auto-repeat
      setKeys(new Set(heldRef.current).add(k));
    };
    const onUp = (e: KeyboardEvent) => {
      const k = KEYS[e.code];
      if (!k || !heldRef.current.has(k)) return;
      const next = new Set(heldRef.current);
      next.delete(k);
      setKeys(next);
    };
    // A tab that loses focus never sees the keyup: treat it as all keys up.
    const onHidden = () => {
      if (document.visibilityState === "hidden") stopKeys();
    };
    window.addEventListener("keydown", onDown);
    window.addEventListener("keyup", onUp);
    window.addEventListener("blur", stopKeys);
    window.addEventListener("pagehide", stopKeys);
    document.addEventListener("visibilitychange", onHidden);
    return () => {
      clearInterval(tick);
      clearInterval(renew);
      window.removeEventListener("keydown", onDown);
      window.removeEventListener("keyup", onUp);
      window.removeEventListener("blur", stopKeys);
      window.removeEventListener("pagehide", stopKeys);
      document.removeEventListener("visibilitychange", onHidden);
    };
  }, [phase, sendTwist, setKeys, stopKeys, trySend]);

  // Leaving this robot (selection changed, panel closed, signed out) while
  // holding its lease hands it back rather than leaving it to expire.
  useEffect(
    () => () => {
      const l = leaseRef.current;
      if (!l) return;
      heldRef.current = new Set();
      linkRef.current?.send(STOP);
      linkRef.current?.close();
      linkRef.current = undefined;
      trySend("lease.release", { lease_id: l.lease_id, resolution: "abandoned" }, { id: nextId("release") });
      leaseRef.current = undefined;
    },
    [trySend],
  );

  const takeOver = useCallback(
    (opts?: { steal?: boolean }) => {
      if (phaseRef.current === "claiming" || phaseRef.current === "driving") return;
      setNotice(undefined);
      const id = nextId("claim");
      pendingRef.current.claim = id;
      setPhase("claiming");
      const payload = opts?.steal === true ? { robot_id: robotId, steal: true } : { robot_id: robotId };
      if (!trySend("lease.claim", payload, { id })) drop(say("Not connected."));
    },
    [robotId, setPhase, trySend, drop],
  );

  const release = useCallback(() => {
    const l = leaseRef.current;
    if (!l || phaseRef.current !== "driving") return;
    heldRef.current = new Set();
    setHeld(new Set());
    linkRef.current?.send(STOP);
    const id = nextId("release");
    pendingRef.current.release = id;
    // The server answers a release with the robot.lease_released event, not a
    // direct reply; our lease is over the moment we ask.
    if (!trySend("lease.release", { lease_id: l.lease_id, resolution: "resolved" }, { id })) {
      drop(say("Not connected; the lease will expire on the server."));
      return;
    }
    setLease(undefined);
    setPhase("idle");
    setNotice(undefined);
  }, [setLease, setPhase, trySend, drop]);

  const setSpeed = useCallback((s: number) => {
    const v = clamp(s, 0.1, 1);
    speedRef.current = v;
    setSpeedState(v);
  }, []);

  return {
    phase,
    lease,
    endedLeaseId,
    held,
    command: phase === "driving" ? twistFor(held, speed, maxV, maxW) : { vx: 0, wz: 0 },
    link,
    speed,
    setSpeed,
    notice,
    takeOver,
    release,
  };
}
