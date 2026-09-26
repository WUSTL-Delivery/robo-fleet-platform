// Every robot the fleet knows, online or not, with its FSM state. Selecting a
// row selects the robot for the detail panel (and highlights its marker).
import type { RobotState } from "@fleet-platform/sdk";
import { displayName, type RobotView } from "./fleet/model";

const STATE_LABEL: Record<RobotState, string> = {
  AUTONOMOUS: "Autonomous",
  HELP_REQUESTED: "Needs help",
  TELEOP: "Teleop",
};

interface Props {
  robots: RobotView[];
  synced: boolean;
  selectedId: string | null;
  onSelect: (robotId: string) => void;
}

export function FleetList({ robots, synced, selectedId, onSelect }: Props) {
  const online = robots.filter((r) => r.presence === "online").length;
  return (
    <section className="fleet-list" aria-label="Robots">
      <header>
        <h2>Robots</h2>
        <span className="count" data-testid="fleet-count">
          {online} online / {robots.length}
        </span>
      </header>
      {!synced && <p className="hint pad">Loading fleet...</p>}
      {synced && robots.length === 0 && (
        <p className="hint pad">No robots have enrolled in this fleet yet.</p>
      )}
      <ul>
        {robots.map((r) => (
          <li key={r.robot_id}>
            <button
              className="robot-row"
              data-robot-id={r.robot_id}
              data-presence={r.presence}
              data-state={r.state}
              aria-pressed={r.robot_id === selectedId}
              onClick={() => onSelect(r.robot_id)}
            >
              <span className={`dot ${r.presence === "online" ? "connected" : "offline"}`} aria-hidden />
              <span className="name">{displayName(r)}</span>
              <span className="presence">{r.presence}</span>
              <span className={`badge state-${r.state.toLowerCase()}`}>{STATE_LABEL[r.state]}</span>
            </button>
          </li>
        ))}
      </ul>
    </section>
  );
}
