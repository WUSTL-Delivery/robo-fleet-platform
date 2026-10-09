// Teleop section of the robot detail pane: take over, drive with WASD, hand
// back; or, while another operator drives, watch read-only with the driver
// named and one explicit way in (take control from them: a steal). Rendered
// only for robots whose manifest declares a twist drive, keyed by robot id
// (see useTeleop). Which of those the operator sees is controlOf's answer.
import { useEffect, useRef } from "react";
import type { FleetClient } from "@fleet-platform/sdk";
import type { RobotView } from "../fleet/model";
import { operatorLabel, type Operators } from "../fleet/presence";
import { controlOf, noticeText } from "./control";
import { useTeleop, type DriveKey } from "./useTeleop";
import type { TwistLinkStatus } from "./twistTransport";

interface Props {
  client: FleetClient;
  robot: RobotView;
  operatorId: string | undefined;
  /** The fleet's operators, to show a driver by name. */
  operators: Operators;
  /**
   * A claim asked for elsewhere (the help queue's Claim button). Each new
   * number takes over once, exactly as the Take over button would.
   */
  claimSeq?: number;
}

const PAD: { key: DriveKey; label: string; area: string }[] = [
  { key: "forward", label: "W", area: "w" },
  { key: "left", label: "A", area: "a" },
  { key: "back", label: "S", area: "s" },
  { key: "right", label: "D", area: "d" },
];

/** Which wire twist is on, in words. The direct link is the fast path; the server relay is the fallback. */
function linkLabel(link: TwistLinkStatus): string {
  if (link.active === "p2p") return `Direct (WebRTC)${link.rttMs === undefined ? "" : `, ${Math.round(link.rttMs)} ms`}`;
  if (link.direct === "connecting") return "Server relay, connecting direct link...";
  if (link.direct === "retrying") return "Server relay, direct link down (retrying)";
  return "Server relay";
}

export function TeleopPanel({ client, robot, operatorId, operators, claimSeq }: Props) {
  const t = useTeleop(client, robot, operatorId);
  const { takeOver } = t;
  const claimHandled = useRef<number | undefined>(undefined);
  useEffect(() => {
    if (claimSeq === undefined || claimHandled.current === claimSeq) return;
    claimHandled.current = claimSeq;
    // The queue's Claim is a plain claim: if someone else got there first the
    // server refuses it, and the notice says who. It never steals.
    takeOver();
  }, [claimSeq, takeOver]);
  const drive = robot.manifest?.drive;
  const online = robot.presence === "online";
  const control = controlOf(robot, operatorId, t.phase, t.endedLeaseId);
  const nameOf = (id: string) => operatorLabel(operators, id);
  const driver = control.driverId === undefined ? undefined : nameOf(control.driverId);
  const velocity = robot.telemetry?.velocity;

  return (
    <section className="teleop" aria-label="Teleop" data-phase={t.phase} data-control={control.mode}>
      <h3>Teleop</h3>
      {control.mode === "driving" ? (
        <>
          <p className="teleop-status driving" aria-live="polite">
            You have control. Hold <kbd>W</kbd> <kbd>A</kbd> <kbd>S</kbd> <kbd>D</kbd> to drive.
          </p>
          <div className="keypad" aria-hidden="true">
            {PAD.map((p) => (
              <span key={p.key} className="key" style={{ gridArea: p.area }} data-held={t.held.has(p.key)}>
                {p.label}
              </span>
            ))}
          </div>
          <dl className="facts compact">
            <dt>Command</dt>
            <dd className="mono" data-testid="teleop-command">
              v {t.command.vx.toFixed(2)} m/s, ω {t.command.wz.toFixed(2)} rad/s
            </dd>
            <dt>Link</dt>
            <dd data-testid="teleop-link" data-transport={t.link.active} data-direct={t.link.direct}>
              {linkLabel(t.link)}
            </dd>
          </dl>
          <label htmlFor="teleop-speed">
            Speed {Math.round(t.speed * 100)}% of {drive?.max_v_mps} m/s, {drive?.max_w_radps} rad/s
          </label>
          <input
            id="teleop-speed"
            type="range"
            min={0.1}
            max={1}
            step={0.05}
            value={t.speed}
            onChange={(e) => t.setSpeed(Number(e.target.value))}
          />
          <button className="primary" onClick={t.release}>
            Hand back
          </button>
        </>
      ) : driver !== undefined ? (
        // Read-only: this console holds no lease, so it has no twist transport
        // and no drive keys. The one control is the explicit steal.
        <>
          <p className="teleop-status spectating" aria-live="polite" data-testid="teleop-driver" title={control.driverId}>
            <strong>{driver}</strong> is driving. You are watching read-only.
          </p>
          {velocity && (
            <dl className="facts compact">
              <dt>Moving</dt>
              <dd className="mono" data-testid="spectate-velocity">
                v {(velocity.v_mps ?? 0).toFixed(2)} m/s, ω {(velocity.w_radps ?? 0).toFixed(2)} rad/s
              </dd>
            </dl>
          )}
          <button
            className="primary steal"
            data-testid="teleop-steal"
            onClick={() => takeOver({ steal: true })}
            disabled={!online || control.mode === "claiming"}
            title={online ? `Revokes ${driver}'s control and gives it to you` : "Offline: nothing to drive"}
          >
            {control.mode === "claiming" ? "Taking control..." : `Take control from ${driver}`}
          </button>
        </>
      ) : (
        <>
          <p className="teleop-status" aria-live="polite">
            {control.mode === "held"
              ? "You hold this robot's lease from another session of this console."
              : control.mode === "settling"
                ? "You no longer have control."
                : online
                ? "Autonomy is in charge."
                : "Offline: nothing to drive."}
          </p>
          <button
            className="primary"
            data-testid="teleop-take-over"
            onClick={() => takeOver()}
            disabled={!online || control.mode === "claiming" || control.mode === "settling"}
          >
            {control.mode === "claiming" ? "Taking over..." : control.mode === "held" ? "Resume control" : "Take over"}
          </button>
        </>
      )}
      {t.notice && (
        <p className="hint teleop-notice" role="status" data-notice={t.notice.kind}>
          {noticeText(t.notice, nameOf)}
        </p>
      )}
    </section>
  );
}
