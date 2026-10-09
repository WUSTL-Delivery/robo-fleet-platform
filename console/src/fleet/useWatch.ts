// React binding for WatchSender: reports the robot the console has open, and
// reports it again on every (re)connect while one is open.
import { useEffect, useRef } from "react";
import type { FleetClient } from "@fleet-platform/sdk";
import { WatchSender } from "./watch";

export function useWatch(client: FleetClient, robotId: string | null): void {
  const sender = useRef<WatchSender | undefined>(undefined);

  useEffect(() => {
    const s = new WatchSender({
      send: (id, envelopeId) => {
        try {
          client.send("watch", { robot_id: id }, { id: envelopeId });
          return true;
        } catch {
          return false; // not open: the next "open" sends the selection
        }
      },
    });
    sender.current = s;
    const offState = client.onState((change) => {
      if (change.state === "open") s.opened();
    });
    const offError = client.on("error", ({ payload }) => s.error(payload));
    return () => {
      offState();
      offError();
      s.dispose();
      sender.current = undefined;
    };
  }, [client]);

  useEffect(() => {
    sender.current?.select(robotId);
  }, [client, robotId]);
}
