import type { SessionView } from "./App";

const LABEL: Record<SessionView["state"], string> = {
  idle: "Starting",
  enrolling: "Redeeming invite",
  connecting: "Connecting",
  open: "Connected",
  reconnecting: "Reconnecting",
  closed: "Disconnected",
};

export function Connected({ session, onSignOut }: { session: SessionView; onSignOut: () => void }) {
  const { state, welcome } = session;
  const dot = state === "open" ? "connected" : state === "closed" ? "error" : "connecting";
  return (
    <>
      <header className="topbar">
        <div className="brand">
          <img src="/favicon.svg" alt="" />
          <h1>Fleet console</h1>
        </div>
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
      <main className="shell">
        {welcome ? (
          <dl className="facts">
            <dt>Operator</dt>
            <dd className="mono">{welcome.client_id}</dd>
            <dt>Fleet</dt>
            <dd className="mono">{welcome.fleet_id}</dd>
            <dt>Role</dt>
            <dd>{welcome.kind}</dd>
            <dt>Heartbeat</dt>
            <dd>every {Math.round(welcome.heartbeat_interval_ms / 1000)}s</dd>
          </dl>
        ) : (
          <p className="hint">{session.error ?? "Waiting for the server..."}</p>
        )}
      </main>
    </>
  );
}
