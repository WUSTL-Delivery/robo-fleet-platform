// Channels: the opaque domain bus (docs/INTEGRATION.md §2.6). The platform never
// inspects `data`; everything shaped like an order, a waypoint list, or an edge
// report rides here. A Channel is a thin, stateless view over one channel name
// on a FleetClient:
//
//   const assignments = client.channel<Assignment>("assignment");
//   assignments.publish({ order_id: 17 }, { to: robotId });  // directed
//   assignments.publish(report, { broadcast: true });         // fan-out
//   assignments.onMessage((m) => ...);                        // m.from is server-stamped
//   await client.channel("edge_report").subscribe();          // receive broadcasts
//
// Delivery is at-most-once. Directed messages and broadcasts to robots whose
// manifest lists the channel need no subscribe; everyone else subscribes to
// `channel:<name>` to receive broadcasts.

import type { SnapshotPayload } from "./generated/protocol.js";
import type { Envelope } from "./generated/messages.js";
import type { FleetClient, SendOptions } from "./client.js";

/** Channel names as the protocol allows them. */
export const CHANNEL_NAME_PATTERN = /^[a-z0-9][a-z0-9_.-]{0,63}$/;

/** Exactly one of a connected client id in your fleet, or a broadcast. */
export type PublishTarget = { to: string } | { broadcast: true };

/** One inbound `channel.message`, with `data` typed as the channel's payload. */
export interface ChannelMessage<T = unknown> {
  channel: string;
  /** Sender's client id, stamped by the server. Reply to it with `{ to: from }`. */
  from: string;
  data: T;
  ts_ms?: number;
}

/**
 * A typed view of one channel. `T` is the payload shape you agree on with the
 * other side; the SDK does not validate it (the platform never looks inside).
 */
export class Channel<T = unknown> {
  readonly #client: FleetClient;
  readonly name: string;

  constructor(client: FleetClient, name: string) {
    if (!CHANNEL_NAME_PATTERN.test(name)) {
      throw new TypeError(`invalid channel name ${JSON.stringify(name)}: must match ${CHANNEL_NAME_PATTERN}`);
    }
    this.#client = client;
    this.name = name;
  }

  /** The subscribe topic for this channel's broadcasts. */
  get topic(): `channel:${string}` {
    return `channel:${this.name}`;
  }

  /**
   * Sends `data` to one client (`{ to }`) or to everyone listening
   * (`{ broadcast: true }`; the sender is excluded). Throws unless connected.
   * A directed send to a client that is not connected comes back as an
   * `error{not_found}` with `ref` = `opts.id`.
   */
  publish(data: T, target: PublishTarget, opts?: SendOptions): Envelope<"channel.publish"> {
    const payload =
      "to" in target
        ? { channel: this.name, to: target.to, data: data as unknown }
        : { channel: this.name, broadcast: true as const, data: data as unknown };
    return this.#client.send("channel.publish", payload, opts);
  }

  /**
   * Calls `handler` for every `channel.message` on this channel, directed or
   * broadcast. Like every stream callback, it never runs ahead of a pending
   * snapshot. Returns an unsubscribe function.
   */
  onMessage(handler: (msg: ChannelMessage<T>) => void): () => void {
    return this.#client.onChannelMessage(this.name, handler as (msg: ChannelMessage) => void);
  }

  /** Subscribes to this channel's broadcasts; resolves with the fresh snapshot. */
  subscribe(): Promise<SnapshotPayload> {
    return this.#client.subscribe([this.topic]);
  }
}
