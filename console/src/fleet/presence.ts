// Who is here, and who is watching or driving what.
//
// Pure selectors over FleetState. Two facts are joined: the operator entries
// the server sends (online, and the one robot each is `watching`) and the
// robots' leases (the lease's operator is the driver). Every console that has
// seen the same snapshot and events therefore shows the same people on the
// same robots.
import type { OperatorSummary } from "@fleet-platform/sdk";
import { displayName, type FleetState, type RobotView } from "./model";

export type Operators = ReadonlyMap<string, OperatorSummary>;

/** The operator's name where the entry has one, its id otherwise (and for an id we have no entry for). */
export function operatorLabel(operators: Operators, operatorId: string): string {
  return operators.get(operatorId)?.name || operatorId;
}

export interface Person {
  operator_id: string;
  label: string;
  /** This console's own operator. */
  self: boolean;
}

export interface OperatorView extends Person {
  online: boolean;
  /** The robot this operator has open. A `watching` id that is not in the robot list counts as none. */
  watching?: RobotView;
  /** Robots this operator holds the lease of, by name. */
  driving: RobotView[];
}

/** Every operator, for the presence bar: this console first, then online before offline, then by name. */
export function operatorViews(state: FleetState, selfId: string | undefined): OperatorView[] {
  const driving = new Map<string, RobotView[]>();
  for (const robot of state.robots.values()) {
    const id = robot.lease?.operator_id;
    if (!id) continue;
    const list = driving.get(id);
    if (list) list.push(robot);
    else driving.set(id, [robot]);
  }
  const views: OperatorView[] = [];
  for (const o of state.operators.values()) {
    const view: OperatorView = {
      operator_id: o.operator_id,
      label: o.name || o.operator_id,
      self: o.operator_id === selfId,
      online: o.online,
      driving: (driving.get(o.operator_id) ?? []).sort(byName),
    };
    const watching = o.online && o.watching ? state.robots.get(o.watching) : undefined;
    if (watching) view.watching = watching;
    views.push(view);
  }
  return views.sort((a, b) => {
    if (a.self !== b.self) return a.self ? -1 : 1;
    if (a.online !== b.online) return a.online ? -1 : 1;
    return a.label.localeCompare(b.label) || (a.operator_id < b.operator_id ? -1 : 1);
  });
}

const byName = (a: RobotView, b: RobotView) => displayName(a).localeCompare(displayName(b));

/** The people on one robot. */
export interface Audience {
  /** Who holds the lease. */
  driver?: Person;
  /** Online operators who have the robot open, the driver excepted, by name. */
  watchers: Person[];
}

const NOBODY: Audience = { watchers: [] };

/** The audience of every robot that has one, by robot id. */
export function audiences(state: FleetState, selfId: string | undefined): ReadonlyMap<string, Audience> {
  const out = new Map<string, Audience>();
  const person = (id: string): Person => ({
    operator_id: id,
    label: operatorLabel(state.operators, id),
    self: id === selfId,
  });
  for (const robot of state.robots.values()) {
    if (robot.lease) out.set(robot.robot_id, { driver: person(robot.lease.operator_id), watchers: [] });
  }
  for (const o of state.operators.values()) {
    if (!o.online || !o.watching || !state.robots.has(o.watching)) continue;
    let a = out.get(o.watching);
    if (!a) out.set(o.watching, (a = { watchers: [] }));
    if (a.driver?.operator_id !== o.operator_id) a.watchers.push(person(o.operator_id));
  }
  for (const a of out.values()) a.watchers.sort((x, y) => x.label.localeCompare(y.label));
  return out;
}

export function audienceOf(all: ReadonlyMap<string, Audience>, robotId: string): Audience {
  return all.get(robotId) ?? NOBODY;
}

/**
 * One line for a list row: "ada driving, 2 watching". This console's own
 * watching is left out (the row is already the selected one); its driving is
 * not. Empty when there is nothing to say.
 */
export function audienceSummary(a: Audience): string {
  const parts: string[] = [];
  if (a.driver) parts.push(`${a.driver.self ? "you" : a.driver.label} driving`);
  const others = a.watchers.filter((w) => !w.self);
  if (others.length === 1) parts.push(`${others[0]!.label} watching`);
  else if (others.length > 1) parts.push(`${others.length} watching`);
  return parts.join(", ");
}
