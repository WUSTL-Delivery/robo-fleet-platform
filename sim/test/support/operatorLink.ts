// The operator's end of the teleop data plane for the integration tests: the
// console's own twist transport (what the browser runs) with werift's peer
// connection handed in where the browser's would be, and taps for the test.
import type { FleetClient, Lease, TwistPayload } from "@fleet-platform/sdk";
import { RTCPeerConnection } from "werift";
import {
  openTwistTransport,
  type ChannelLike,
  type CreatePeer,
  type PeerLike,
  type TwistLinkStatus,
  type TwistTransport,
} from "../../../console/src/teleop/twistTransport.js";

/** The operator's end of one lease: the console transport over werift, with taps for the test. */
export class OperatorLink {
  readonly transport: TwistTransport;
  readonly statuses: TwistLinkStatus[] = [];
  readonly busSent: TwistPayload[] = [];
  /** The newest data channel, for sending what the transport never would. */
  raw: ChannelLike | undefined;
  /** true: the direct link goes silent in both directions without closing. */
  cut = false;
  peers = 0;

  constructor(client: FleetClient, robotId: string, lease: Lease) {
    const createPeer: CreatePeer = () => {
      this.peers += 1;
      const pc = new RTCPeerConnection({ iceServers: [], iceAdditionalHostAddresses: ["127.0.0.1"] });
      const peer = pc as unknown as PeerLike;
      const create = peer.createDataChannel.bind(peer);
      peer.createDataChannel = (label, options) => this.#tap(create(label, options));
      return peer;
    };
    this.transport = openTwistTransport({
      client,
      robotId,
      leaseId: lease.lease_id,
      sendBus: (payload) => {
        this.busSent.push(payload);
        client.send("twist", payload);
      },
      onStatus: (s) => this.statuses.push(s),
      createPeer,
    });
  }

  get active(): "bus" | "p2p" {
    return this.transport.status.active;
  }

  #tap(dc: ChannelLike): ChannelLike {
    const link = this;
    const tapped: ChannelLike = {
      get readyState() {
        return dc.readyState;
      },
      onopen: null,
      onclose: null,
      onmessage: null,
      send: (data) => {
        if (!link.cut) dc.send(data);
      },
      close: () => dc.close(),
    };
    dc.onopen = () => tapped.onopen?.(undefined as never);
    dc.onclose = () => tapped.onclose?.(undefined as never);
    dc.onmessage = (ev) => {
      if (!link.cut) tapped.onmessage?.(ev);
    };
    this.raw = dc;
    return tapped;
  }
}
