// A UDP endpoint that stands where an installation's STUN/TURN server would,
// for tests of who is handed which ICE servers. It is not a relay. It answers
// just enough of STUN (RFC 8489) and TURN (RFC 8656) for a peer connection to
// finish gathering at once instead of waiting out its timeouts:
//
//   Binding request   -> success, with the sender's address (a normal STUN answer)
//   Allocate request  -> 401 with a realm and nonce, as every TURN server first
//                        answers; then, to the authenticated retry, 486
//                        (Allocation Quota Reached): no relay is ever handed out
//
// It records what it was asked, which is the point: the USERNAME of an
// authenticated Allocate is the TURN credential the peer connection was given.
// The refusal is signed the way a TURN server using the shared secret signs
// it (coturn --use-auth-secret), because a client ignores an unsigned answer
// to an authenticated request.
import { createHash, createHmac } from "node:crypto";
import { createSocket, type Socket } from "node:dgram";

const MAGIC = 0x2112a442;
const BINDING_REQUEST = 0x0001;
const BINDING_SUCCESS = 0x0101;
const ALLOCATE_REQUEST = 0x0003;
const ALLOCATE_ERROR = 0x0113;
const ATTR_USERNAME = 0x0006;
const ATTR_MESSAGE_INTEGRITY = 0x0008;
const ATTR_ERROR_CODE = 0x0009;
const ATTR_REALM = 0x0014;
const ATTR_NONCE = 0x0015;
const ATTR_XOR_MAPPED_ADDRESS = 0x0020;
const REALM = "sim.test";

export interface StunStub {
  port: number;
  /** Binding requests answered. */
  readonly bindings: number;
  /** The USERNAME of every authenticated Allocate request, oldest first. */
  readonly usernames: string[];
  close(): Promise<void>;
}

function attr(type: number, value: Buffer): Buffer {
  const head = Buffer.alloc(4);
  head.writeUInt16BE(type, 0);
  head.writeUInt16BE(value.length, 2);
  return Buffer.concat([head, value, Buffer.alloc((4 - (value.length % 4)) % 4)]);
}

function message(type: number, txid: Buffer, attrs: Buffer[]): Buffer {
  const body = Buffer.concat(attrs);
  const head = Buffer.alloc(8);
  head.writeUInt16BE(type, 0);
  head.writeUInt16BE(body.length, 2);
  head.writeUInt32BE(MAGIC, 4);
  return Buffer.concat([head, txid, body]);
}

/** Appends MESSAGE-INTEGRITY under the long-term key of `username`, whose password derives from the shared secret. */
function signed(msg: Buffer, username: string, secret: string): Buffer {
  const password = createHmac("sha1", secret).update(username).digest("base64");
  const key = createHash("md5").update(`${username}:${REALM}:${password}`).digest();
  // The HMAC covers the message with its length already counting the attribute.
  const covered = Buffer.from(msg);
  covered.writeUInt16BE(msg.length - 20 + 24, 2);
  return Buffer.concat([covered, attr(ATTR_MESSAGE_INTEGRITY, createHmac("sha1", key).update(covered).digest())]);
}

function errorCode(code: number, reason: string): Buffer {
  return attr(ATTR_ERROR_CODE, Buffer.concat([Buffer.from([0, 0, Math.floor(code / 100), code % 100]), Buffer.from(reason)]));
}

function attribute(msg: Buffer, wanted: number): Buffer | undefined {
  for (let at = 20; at + 4 <= msg.length; ) {
    const type = msg.readUInt16BE(at);
    const len = msg.readUInt16BE(at + 2);
    if (type === wanted) return msg.subarray(at + 4, at + 4 + len);
    at += 4 + len + ((4 - (len % 4)) % 4);
  }
  return undefined;
}

/** Listens on a free UDP port on loopback. `turnSecret` is the one the fleet-server signs credentials with. */
export function startStunStub(turnSecret: string): Promise<StunStub> {
  return new Promise((resolve, reject) => {
    const socket: Socket = createSocket("udp4");
    const state = { bindings: 0, usernames: [] as string[] };
    socket.on("message", (msg, from) => {
      if (msg.length < 20 || msg.readUInt32BE(4) !== MAGIC) return;
      const type = msg.readUInt16BE(0);
      const txid = msg.subarray(8, 20);
      let reply: Buffer | undefined;
      if (type === BINDING_REQUEST) {
        state.bindings += 1;
        const value = Buffer.alloc(8);
        value.writeUInt8(0x01, 1); // IPv4
        value.writeUInt16BE(from.port ^ (MAGIC >>> 16), 2);
        const ip = from.address.split(".").reduce((n, part) => ((n << 8) | Number(part)) >>> 0, 0);
        value.writeUInt32BE((ip ^ MAGIC) >>> 0, 4);
        reply = message(BINDING_SUCCESS, txid, [attr(ATTR_XOR_MAPPED_ADDRESS, value)]);
      } else if (type === ALLOCATE_REQUEST) {
        const username = attribute(msg, ATTR_USERNAME);
        if (username) {
          const who = username.toString("utf8");
          state.usernames.push(who);
          reply = signed(message(ALLOCATE_ERROR, txid, [errorCode(486, "Allocation Quota Reached")]), who, turnSecret);
        } else {
          reply = message(ALLOCATE_ERROR, txid, [
            errorCode(401, "Unauthorized"),
            attr(ATTR_REALM, Buffer.from(REALM)),
            attr(ATTR_NONCE, Buffer.from("0123456789abcdef")),
          ]);
        }
      }
      if (reply) socket.send(reply, from.port, from.address);
    });
    socket.once("error", reject);
    socket.bind(0, "127.0.0.1", () => {
      resolve({
        port: socket.address().port,
        get bindings() {
          return state.bindings;
        },
        usernames: state.usernames,
        close: () => new Promise<void>((done) => socket.close(() => done())),
      });
    });
  });
}
