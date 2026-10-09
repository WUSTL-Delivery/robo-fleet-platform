// The signed-in console: help queue + fleet list | live map | detail pane.
// Selection lives here so the queue, the list, the map and the detail pane
// (and its teleop section) all agree on which robot the operator is looking at.
import { useCallback, useRef, useState } from "react";
import type { FleetClient } from "@fleet-platform/sdk";
import type { SessionView } from "./App";
import { sortedRobots } from "./fleet/model";
import { interventionQueue } from "./fleet/queue";
import { useFleet } from "./fleet/useFleet";
import { useServerNow } from "./fleet/useServerNow";
import { FleetList } from "./FleetList";
import { FleetMap } from "./FleetMap";
import { QueueList } from "./QueueList";
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

  const queue = interventionQueue(fleet);
  const serverNowMs = useServerNow(client, queue.length > 0);

  // Claiming from the queue selects the robot and asks its teleop section to
  // take over. The request lives only until the selection next changes, so
  // coming back to a robot later never claims it again.
  const claimCounter = useRef(0);
  const [claim, setClaim] = useState<{ robotId: string; seq: number } | null>(null);
  const select = useCallback((robotId: string | null) => {
    setClaim(null);
    setSelectedId(robotId);
  }, []);
  const claimFromQueue = useCallback((robotId: string) => {
    setSelectedId(robotId);
    setClaim({ robotId, seq: ++claimCounter.current });
  }, []);

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
        <div className="sidebar">
          <QueueList
            entries={queue}
            synced={fleet.synced}
            serverNowMs={serverNowMs}
            selectedId={selectedId}
            onSelect={select}
            onClaim={claimFromQueue}
          />
          <FleetList robots={robots} synced={fleet.synced} selectedId={selectedId} onSelect={select} />
        </div>
        <FleetMap robots={robots} selectedId={selectedId} onSelect={select} />
        <RobotPanel
          client={client}
          operatorId={welcome?.client_id}
          robot={selected}
          claimSeq={claim && claim.robotId === selectedId ? claim.seq : undefined}
          onClose={() => select(null)}
        />
      </div>
    </div>
  );
}
