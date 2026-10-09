// A WebSocket to the real fleet-server that behaves, for one message type, as
// a server from before `ice.request` existed: the request never reaches the
// server, and the client is answered what such a server answers to a type it
// does not know, error{code: invalid_message} naming the request
// (protocol/README.md, "ICE servers"). Everything else passes through.
import type { WebSocketConstructor } from "@fleet-platform/sdk";
import { WebSocket as WsWebSocket } from "ws";

export interface OldServerSocket {
  WebSocket: WebSocketConstructor;
  /** How many ice.request messages were answered this way. */
  readonly refused: number;
}

export function serverWithoutIce(): OldServerSocket {
  const state = { refused: 0 };
  const WebSocket = class extends WsWebSocket {
    override send(data: unknown, ...rest: unknown[]): void {
      let env: { v?: unknown; type?: unknown; id?: unknown } | undefined;
      try {
        env = typeof data === "string" ? (JSON.parse(data) as typeof env) : undefined;
      } catch {
        /* not ours to judge */
      }
      if (env?.type !== "ice.request") {
        (super.send as (...a: unknown[]) => void)(data, ...rest);
        return;
      }
      state.refused += 1;
      const reply = {
        v: 0,
        type: "error",
        payload: { code: "invalid_message", message: 'unknown message type "ice.request"', ...(typeof env.id === "string" ? { ref: env.id } : {}) },
      };
      // As a frame from the server would arrive: later, never inside send().
      setImmediate(() => this.emit("message", Buffer.from(JSON.stringify(reply)), false));
    }
  } as unknown as WebSocketConstructor;
  return {
    WebSocket,
    get refused() {
      return state.refused;
    },
  };
}
