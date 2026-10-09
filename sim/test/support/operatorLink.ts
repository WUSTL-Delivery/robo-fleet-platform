// The operator's end of the teleop data plane for the integration tests: the
// console's own twist transport (what the browser runs) with werift's peer
// connection handed in where the browser's would be, and taps for the test.
import type { FleetClient, Lease, TwistPayload } from "@fleet-platform/sdk";
import { RTCPeerConnection } from "werift";
import { dropDefaultStun } from "../../src/p2p.js";
import {
  openTwistTransport,
  type ChannelLike,
  type CreatePeer,
  type IceServer,
  type IceServerSource,
  type PeerLike,
  type TwistLinkStatus,
  type TwistTransport,
} from "../../../console/src/teleop/twistTransport.js";

export interface OperatorLinkOptions {
  /** Where the transport gets its ICE servers; the console passes `() => client.iceServers()`. Default: none. */
  iceServers?: IceServerSource;
  /** true: only relayed candidates are used, so the link comes up through a TURN server or not at all. */
  relayOnly?: boolean;
}

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
  /** The ICE servers the transport handed to each peer connection, oldest first. */
  readonly iceServers: IceServer[][] = [];
  /** The newest peer connection itself, for reading the path ICE chose. */
  pc: RTCPeerConnection | undefined;

  constructor(client: FleetClient, robotId: string, lease: Lease, opts: OperatorLinkOptions = {}) {
    const createPeer: CreatePeer = (config) => {
      this.peers += 1;
      this.iceServers.push(config.iceServers);
      const pc = new RTCPeerConnection({
        // What the transport was given and nothing else, as a browser would be handed it.
        iceServers: config.iceServers,
        iceAdditionalHostAddresses: ["127.0.0.1"],
        ...(opts.relayOnly ? { iceTransportPolicy: "relay" as const } : {}),
      });
      this.pc = pc;
      const peer = pc as unknown as PeerLike;
      // A browser asks no STUN server it was not given; werift needs telling.
      const setLocal = peer.setLocalDescription.bind(peer);
      peer.setLocalDescription = (desc) => {
        dropDefaultStun(pc, config.iceServers);
        return setLocal(desc);
      };
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
      ...(opts.iceServers !== undefined ? { iceServers: opts.iceServers } : {}),
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
