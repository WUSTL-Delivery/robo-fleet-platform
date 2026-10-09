// Teleop section of the robot detail pane: take over, drive with WASD, hand
// back. Rendered only for robots whose manifest declares a twist drive, keyed
// by robot id (see useTeleop).
import { useEffect, useRef } from "react";
import type { FleetClient } from "@fleet-platform/sdk";
import type { RobotView } from "../fleet/model";
import { useTeleop, type DriveKey } from "./useTeleop";

interface Props {
  client: FleetClient;
  robot: RobotView;
  operatorId: string | undefined;
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

export function TeleopPanel({ client, robot, operatorId, claimSeq }: Props) {
  const t = useTeleop(client, robot, operatorId);
  const { takeOver } = t;
  const claimHandled = useRef<number | undefined>(undefined);
  useEffect(() => {
    if (claimSeq === undefined || claimHandled.current === claimSeq) return;
    claimHandled.current = claimSeq;
    takeOver();
  }, [claimSeq, takeOver]);
  const drive = robot.manifest?.drive;
  const online = robot.presence === "online";
  const otherDriver = robot.lease && robot.lease.operator_id !== operatorId ? robot.lease.operator_id : undefined;
  const driving = t.phase === "driving";

  return (
    <section className="teleop" aria-label="Teleop" data-phase={t.phase}>
      <h3>Teleop</h3>
      {driving ? (
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
      ) : (
        <>
          <p className="teleop-status" aria-live="polite">
            {otherDriver ? (
              <>
                Driven by <span className="mono">{otherDriver}</span>. Watching read-only.
              </>
            ) : online ? (
              "Autonomy is in charge."
            ) : (
              "Offline: nothing to drive."
            )}
          </p>
          <button className="primary" onClick={t.takeOver} disabled={!online || t.phase === "claiming"}>
            {t.phase === "claiming" ? "Taking over..." : otherDriver ? "Take over from them" : "Take over"}
          </button>
        </>
      )}
      {t.notice && (
        <p className="hint teleop-notice" role="status">
          {t.notice}
        </p>
      )}
    </section>
  );
}
