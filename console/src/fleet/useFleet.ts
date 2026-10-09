// React binding for the fleet model: subscribes the client to presence, events
// and telemetry once, feeds every snapshot and event into fleetReducer, and
// hands the resulting FleetState to the view.
import { useEffect, useReducer, useRef } from "react";
import type { FleetClient } from "@fleet-platform/sdk";
import { emptyFleet, fleetReducer, needsResync, type FleetState } from "./model";

const TOPICS = ["presence", "events", "telemetry"] as const;
/** Coalesces a burst of robots coming online into one snapshot request. */
const RESYNC_DELAY_MS = 250;

export function useFleet(client: FleetClient): FleetState {
  const [state, dispatch] = useReducer(fleetReducer, emptyFleet);
  const known = useRef<ReadonlySet<string>>(new Set());
  known.current = new Set(state.robots.keys());

  useEffect(() => {
    // A new client (sign-out, sign-in) starts from nothing. A snapshot already
    // received seeds the state; later ones arrive through onSnapshot,
    // including after every reconnect.
    dispatch({ kind: "reset" });
    if (client.snapshot) dispatch({ kind: "snapshot", snapshot: client.snapshot });

    // When a robot we have not seen appears, or one comes back online, ask for
    // a fresh snapshot (see needsResync): re-subscribing to the same topics
    // answers with one.
    let resync: ReturnType<typeof setTimeout> | undefined;
    const scheduleResync = () => {
      if (resync !== undefined) return;
      resync = setTimeout(() => {
        resync = undefined;
        if (client.state === "open") client.subscribe(TOPICS).catch(() => {});
      }, RESYNC_DELAY_MS);
    };

    const offSnapshot = client.onSnapshot((snapshot) => dispatch({ kind: "snapshot", snapshot }));
    const offEvent = client.onEvent((event) => {
      if (needsResync(event, known.current)) scheduleResync();
      dispatch({ kind: "event", event, atMs: Date.now() });
    });
    // Topics are remembered by the client and re-sent on reconnect; subscribing
    // again (StrictMode remount) is additive and harmless.
    client.subscribe(TOPICS).catch(() => {
      /* a closed client is surfaced through onState by App */
    });
    return () => {
      clearTimeout(resync);
      offSnapshot();
      offEvent();
    };
  }, [client]);

  return state;
}
