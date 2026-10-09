// Who is at a console right now, and what each of them is on: the robot they
// drive, or the one they have open. Selecting a person's robot opens it here,
// which is how one operator joins another on a takeover.
import { displayName } from "./fleet/model";
import type { OperatorView } from "./fleet/presence";

interface Props {
  /** Every operator of the fleet, this console's first (see operatorViews). */
  operators: OperatorView[];
  selectedId: string | null;
  onSelect: (robotId: string) => void;
}

export function PresenceBar({ operators, selectedId, onSelect }: Props) {
  const online = operators.filter((o) => o.online);
  const offline = operators.filter((o) => !o.online);
  return (
    <section className="presence-bar" aria-label="Operators">
      <ul>
        {online.map((o) => (
          <li
            key={o.operator_id}
            className="operator"
            data-operator-id={o.operator_id}
            data-self={o.self}
            data-activity={o.driving.length > 0 ? "driving" : o.watching ? "watching" : "idle"}
            title={`operator ${o.operator_id}`}
          >
            <span className="dot connected" aria-hidden />
            <span className="name">
              {o.label}
              {o.self && <span className="you"> (you)</span>}
            </span>
            {o.driving.map((r) => (
              <RobotLink key={`d:${r.robot_id}`} verb="driving" id={r.robot_id} name={displayName(r)} selectedId={selectedId} onSelect={onSelect} />
            ))}
            {/* Watching the robot you drive is said once, as driving. */}
            {o.watching && !o.driving.includes(o.watching) && (
              <RobotLink verb="watching" id={o.watching.robot_id} name={displayName(o.watching)} selectedId={selectedId} onSelect={onSelect} />
            )}
          </li>
        ))}
      </ul>
      {offline.length > 0 && (
        <span className="offline-count" data-testid="operators-offline" title={offline.map((o) => o.label).join(", ")}>
          {offline.length} offline
        </span>
      )}
    </section>
  );
}

interface LinkProps {
  verb: "driving" | "watching";
  id: string;
  name: string;
  selectedId: string | null;
  onSelect: (robotId: string) => void;
}

function RobotLink({ verb, id, name, selectedId, onSelect }: LinkProps) {
  return (
    <button className="on-robot" data-verb={verb} data-robot-id={id} aria-pressed={id === selectedId} onClick={() => onSelect(id)}>
      {verb} <span className="robot">{name}</span>
    </button>
  );
}
