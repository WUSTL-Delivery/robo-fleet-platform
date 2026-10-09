// Detail pane for the selected robot: identity, presence, FSM state and the
// latest pose, then the capability sections its manifest declares (teleop,
// cameras, battery; see capabilities.tsx). No capability, no UI.
import type { FleetClient } from "@fleet-platform/sdk";
import { CapabilitySections } from "./capabilities";
import { displayName, poseOf, type RobotView } from "./fleet/model";

interface Props {
  client: FleetClient;
  /** This console's operator id (welcome.client_id), to tell our lease from others'. */
  operatorId: string | undefined;
  robot: RobotView | undefined;
  /** Passed through to the teleop section: a claim asked for from the help queue. */
  claimSeq?: number;
  onClose: () => void;
}

export function RobotPanel({ client, operatorId, robot, claimSeq, onClose }: Props) {
  if (!robot) {
    return (
      <aside className="robot-panel empty" aria-label="Robot detail">
        <p className="hint">Select a robot on the map or in the list.</p>
      </aside>
    );
  }
  const pose = poseOf(robot);
  return (
    <aside className="robot-panel" aria-label="Robot detail" data-robot-id={robot.robot_id}>
      <header>
        <h2>{displayName(robot)}</h2>
        <button className="secondary small" onClick={onClose} aria-label="Close detail">
          Close
        </button>
      </header>
      <dl className="facts">
        <dt>Robot id</dt>
        <dd className="mono">{robot.robot_id}</dd>
        <dt>Presence</dt>
        <dd>{robot.presence}</dd>
        <dt>State</dt>
        <dd>{robot.state}</dd>
        {robot.lease && (
          <>
            <dt>Driver</dt>
            <dd className="mono">{robot.lease.operator_id}</dd>
          </>
        )}
        {pose && (
          <>
            <dt>Pose</dt>
            <dd className="mono">{formatPose(pose)}</dd>
          </>
        )}
      </dl>
      <CapabilitySections client={client} robot={robot} operatorId={operatorId} claimSeq={claimSeq} />
    </aside>
  );
}

function formatPose(pose: NonNullable<ReturnType<typeof poseOf>>): string {
  const yaw = pose.yaw_rad === undefined ? "" : `, yaw ${((pose.yaw_rad * 180) / Math.PI).toFixed(0)}°`;
  if (pose.frame === "geographic") return `${pose.lat.toFixed(6)}, ${pose.lon.toFixed(6)}${yaw}`;
  return `${pose.frame_id ?? "local"}: x ${pose.x_m.toFixed(2)} m, y ${pose.y_m.toFixed(2)} m${yaw}`;
}
