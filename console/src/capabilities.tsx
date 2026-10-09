// Capability sections of the robot detail pane. The console renders only what
// the robot's manifest declares: no capability, no UI. Each entry pairs a
// manifest key with the section that renders it; a new capability type slots
// in as one more entry here, and nothing outside this file special-cases it.
import type { FleetClient, Manifest } from "@fleet-platform/sdk";
import type { ReactNode } from "react";
import type { RobotView } from "./fleet/model";
import type { Operators } from "./fleet/presence";
import { TeleopPanel } from "./teleop/TeleopPanel";

export interface SectionProps {
  client: FleetClient;
  robot: RobotView;
  /** This console's operator id (welcome.client_id). */
  operatorId: string | undefined;
  /** The fleet's operators, for sections that name one (the teleop driver). */
  operators: Operators;
  /**
   * Set when the operator asked to claim this robot from outside the detail
   * pane (the help queue). A new number is a new request; see TeleopPanel.
   */
  claimSeq?: number;
}

/** True when the manifest declares a drive this console has a widget for. */
export function canTeleop(robot: RobotView): boolean {
  return robot.manifest?.drive?.type === "twist";
}

interface CapabilitySection {
  key: keyof Manifest;
  /** Returns the section, or null when the declared value is not one this console can render. */
  render: (p: SectionProps) => ReactNode;
}

const SECTIONS: CapabilitySection[] = [
  {
    key: "drive",
    // The drive contract type picks the widget; twist is the only one so far.
    render: (p) =>
      canTeleop(p.robot) ? (
        <TeleopPanel
          key={p.robot.robot_id}
          client={p.client}
          robot={p.robot}
          operatorId={p.operatorId}
          operators={p.operators}
          claimSeq={p.claimSeq}
        />
      ) : null,
  },
  { key: "cameras", render: (p) => <CamerasSection {...p} /> },
  { key: "battery", render: (p) => <BatterySection {...p} /> },
];

/** The sections the robot's manifest declares, in a fixed order. */
export function CapabilitySections(p: SectionProps) {
  const manifest = p.robot.manifest;
  const declared = SECTIONS.filter((s) => manifest?.[s.key] !== undefined);
  if (declared.length === 0) {
    return (
      <p className="hint capability-none" data-testid="no-capabilities">
        This robot's manifest declares no capabilities the console can show.
      </p>
    );
  }
  return (
    <>
      {declared.map((s) => (
        <CapabilitySlot key={s.key} name={s.key}>
          {s.render(p)}
        </CapabilitySlot>
      ))}
    </>
  );
}

function CapabilitySlot({ name, children }: { name: string; children: ReactNode }) {
  return children ? <div data-capability={name}>{children}</div> : null;
}

function CamerasSection({ robot }: SectionProps) {
  const cameras = robot.manifest?.cameras ?? [];
  if (cameras.length === 0) return null;
  return (
    <section className="capability" aria-label="Cameras">
      <h3>Cameras</h3>
      <ul className="cameras">
        {cameras.map((c) => (
          <li key={c.id} className="camera-placeholder" data-camera-id={c.id}>
            <span className="camera-label">{c.label || c.id}</span>
            <span className="hint">No video yet: streams arrive with WebRTC.</span>
          </li>
        ))}
      </ul>
    </section>
  );
}

function BatterySection({ robot }: SectionProps) {
  const battery = robot.telemetry?.battery;
  const pct = battery?.pct;
  return (
    <section className="capability" aria-label="Battery">
      <h3>Battery</h3>
      {pct === undefined && battery?.voltage === undefined ? (
        <p className="hint">No battery reading yet.</p>
      ) : (
        <>
          {pct !== undefined && (
            <div
              className="battery-meter"
              role="meter"
              aria-valuemin={0}
              aria-valuemax={100}
              aria-valuenow={Math.round(pct)}
              aria-label="Battery charge"
              data-low={pct < 20}
            >
              <span style={{ width: `${Math.max(0, Math.min(100, pct))}%` }} />
            </div>
          )}
          <p className="battery-reading mono" data-testid="battery-reading">
            {pct !== undefined && `${pct.toFixed(0)}%`}
            {pct !== undefined && battery?.voltage !== undefined && ", "}
            {battery?.voltage !== undefined && `${battery.voltage.toFixed(1)} V`}
          </p>
        </>
      )}
    </section>
  );
}
