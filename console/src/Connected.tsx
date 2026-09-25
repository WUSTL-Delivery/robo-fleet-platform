// The signed-in console: fleet list | live map | detail pane. Selection lives
// here so the list, the map and the detail pane (and later the teleop panel)
// all agree on which robot the operator is looking at.
import { useState } from "react";
import type { FleetClient } from "@fleet-platform/sdk";
import type { SessionView } from "./App";
import { sortedRobots } from "./fleet/model";
import { useFleet } from "./fleet/useFleet";
import { FleetList } from "./FleetList";
import { FleetMap } from "./FleetMap";
import { RobotPanel } from "./RobotPanel";

const LABEL: Record<SessionView["state"], string> = {
  idle: "Starting",
  enrolling: "Redeeming invite",
  connecting: "Connecting",
  open: "Connected",
  reconnecting: "Reconnecting",
  closed: "Disconnected",
};

interface Props {
  client: FleetClient;
  session: SessionView;
  onSignOut: () => void;
}

export function Connected({ client, session, onSignOut }: Props) {
  const { state, welcome } = session;
  const dot = state === "open" ? "connected" : state === "closed" ? "error" : "connecting";
  const fleet = useFleet(client);
  const robots = sortedRobots(fleet);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const selected = selectedId ? fleet.robots.get(selectedId) : undefined;

  return (
    <div className="app">
      <header className="topbar">
        <div className="brand">
          <img src="/favicon.svg" alt="" />
          <h1>Fleet console</h1>
        </div>
        {welcome && (
          <span className="who" title={`operator ${welcome.client_id}`}>
            fleet <span className="mono">{welcome.fleet_id}</span>
          </span>
        )}
        <div className="spacer" />
        <span className="status" data-state={state} aria-live="polite">
          <span className={`dot ${dot}`} />
          {LABEL[state]}
          {state === "reconnecting" && session.retryInMs !== undefined && ` (retry in ${Math.ceil(session.retryInMs / 1000)}s)`}
        </span>
        <button className="secondary" onClick={onSignOut}>
          Sign out
        </button>
      </header>
      <div className="workspace">
        <FleetList robots={robots} synced={fleet.synced} selectedId={selectedId} onSelect={setSelectedId} />
        <FleetMap robots={robots} selectedId={selectedId} onSelect={setSelectedId} />
        <RobotPanel robot={selected} onClose={() => setSelectedId(null)} />
      </div>
    </div>
  );
}
