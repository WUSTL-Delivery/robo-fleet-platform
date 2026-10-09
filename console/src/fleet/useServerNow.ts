// React binding for serverClock: samples the server's clock from the welcome
// and from every event's send time, and returns the estimated server time,
// refreshed once a second while `ticking` (the caller has something on screen
// that counts up). Nothing is sent to the server.
import { useEffect, useRef, useState } from "react";
import type { FleetClient } from "@fleet-platform/sdk";
import { noClock, observe, serverNow, type ServerClock } from "./serverClock";

const TICK_MS = 1000;

export function useServerNow(client: FleetClient, ticking: boolean): number {
  const clock = useRef<ServerClock>(noClock);
  const read = () => serverNow(clock.current, performance.now(), Date.now());
  const [now, setNow] = useState(read);

  useEffect(() => {
    // The welcome we may have missed is an old sample; a late sample only
    // under-reads and is replaced by the first fresher one.
    clock.current = client.welcome ? observe(noClock, client.welcome.server_time_ms, performance.now()) : noClock;
    const offState = client.onState((change) => {
      // A new connection starts the estimate over, so a tab that slept (and
      // whose monotonic clock may have paused) does not keep a stale offset.
      if (change.state === "open" && change.welcome) {
        clock.current = observe(noClock, change.welcome.server_time_ms, performance.now());
      }
    });
    const offEvent = client.onEvent((event) => {
      if (event.ts_ms !== undefined) clock.current = observe(clock.current, event.ts_ms, performance.now());
    });
    return () => {
      offState();
      offEvent();
    };
  }, [client]);

  useEffect(() => {
    if (!ticking) return;
    setNow(read());
    const t = setInterval(() => setNow(read()), TICK_MS);
    return () => clearInterval(t);
    // `read` only touches the ref and the clocks, so it is not a dependency.
  }, [ticking]);

  return now;
}
