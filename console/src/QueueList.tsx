// The intervention queue: robots waiting for an operator, longest wait first,
// each with what it asked for (reason and context, shown as sent) and how long
// it has waited. Claim takes the lease and opens the robot's teleop; it is
// offered only for robots the console can drive (see canTeleop). The entry
// goes away for every console when anyone's claim is granted.
import { canTeleop } from "./capabilities";
import { displayName } from "./fleet/model";
import { contextRows, formatWait, waitMs, type QueueEntry } from "./fleet/queue";

interface Props {
  entries: QueueEntry[];
  synced: boolean;
  /** Estimated server time (see useServerNow); waits are measured against it. */
  serverNowMs: number;
  selectedId: string | null;
  onSelect: (robotId: string) => void;
  onClaim: (robotId: string) => void;
}

export function QueueList({ entries, synced, serverNowMs, selectedId, onSelect, onClaim }: Props) {
  return (
    <section className="queue" aria-label="Help queue">
      <header>
        <h2>Help queue</h2>
        <span className="count" data-testid="queue-count">
          {entries.length} waiting
        </span>
      </header>
      {synced && entries.length === 0 && <p className="hint pad">No robot is waiting for help.</p>}
      <ol>
        {entries.map((entry) => {
          const { robot, help } = entry;
          const waited = waitMs(entry, serverNowMs);
          const online = robot.presence === "online";
          return (
            <li
              key={robot.robot_id}
              className="queue-entry"
              data-robot-id={robot.robot_id}
              data-presence={robot.presence}
              data-selected={robot.robot_id === selectedId}
            >
              <button className="queue-open" aria-pressed={robot.robot_id === selectedId} onClick={() => onSelect(robot.robot_id)}>
                <span className="name">{displayName(robot)}</span>
                <span className="wait mono" data-testid="queue-wait" title="Time waiting for an operator">
                  {waited === undefined ? "waiting" : formatWait(waited)}
                </span>
                {help && <span className="reason">{help.reason}</span>}
                {!online && <span className="offline">offline</span>}
              </button>
              {help?.context && (
                <dl className="queue-context">
                  {contextRows(help.context).map((row) => (
                    <div key={row.key}>
                      <dt>{row.key}</dt>
                      <dd className="mono">{row.value}</dd>
                    </div>
                  ))}
                </dl>
              )}
              {canTeleop(robot) && (
                <button
                  className="primary small"
                  disabled={!online}
                  title={online ? undefined : "Offline: nothing to drive"}
                  onClick={() => onClaim(robot.robot_id)}
                >
                  Claim
                </button>
              )}
            </li>
          );
        })}
      </ol>
    </section>
  );
}
