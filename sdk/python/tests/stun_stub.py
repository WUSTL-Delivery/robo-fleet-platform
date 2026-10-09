"""A UDP endpoint that stands where an installation's STUN/TURN server would.

For the tests of who is handed which ICE servers. It is not a relay. It
answers just enough of STUN (RFC 8489) and TURN (RFC 8656) for a peer
connection to finish gathering at once instead of waiting out its timeouts:

- Binding request -> success, with the sender's address (a normal STUN answer)
- Allocate request -> 401 with a realm and nonce, as every TURN server first
  answers; then, to the authenticated retry, 486 (Allocation Quota Reached):
  no relay is ever handed out

It records what it was asked, which is the point: the USERNAME of an
authenticated Allocate is the TURN credential the peer connection was given.
The refusal is signed the way a TURN server using the shared secret signs it
(coturn ``--use-auth-secret``), because a client ignores an unsigned answer to
an authenticated request.

The same stub exists for the sim's tests: sim/test/support/stunStub.ts.
"""

from __future__ import annotations

import base64
import hashlib
import hmac
import socket
import struct
import threading

MAGIC = 0x2112A442
BINDING_REQUEST, BINDING_SUCCESS = 0x0001, 0x0101
ALLOCATE_REQUEST, ALLOCATE_ERROR = 0x0003, 0x0113
ATTR_USERNAME, ATTR_MESSAGE_INTEGRITY, ATTR_ERROR_CODE = 0x0006, 0x0008, 0x0009
ATTR_REALM, ATTR_NONCE, ATTR_XOR_MAPPED_ADDRESS = 0x0014, 0x0015, 0x0020
REALM = b"sdk-python.test"


def _attr(type_: int, value: bytes) -> bytes:
    return struct.pack("!HH", type_, len(value)) + value + b"\0" * (-len(value) % 4)


def _message(type_: int, txid: bytes, attrs: list[bytes]) -> bytes:
    body = b"".join(attrs)
    return struct.pack("!HHI", type_, len(body), MAGIC) + txid + body


def _error(code: int, reason: bytes) -> bytes:
    return _attr(ATTR_ERROR_CODE, bytes([0, 0, code // 100, code % 100]) + reason)


def _find(msg: bytes, wanted: int) -> bytes | None:
    at = 20
    while at + 4 <= len(msg):
        type_, length = struct.unpack_from("!HH", msg, at)
        if type_ == wanted:
            return msg[at + 4 : at + 4 + length]
        at += 4 + length + (-length % 4)
    return None


def _own_address() -> str:
    """An IPv4 address of this machine that its other interfaces can send to.

    aiortc gathers from the machine's interface addresses and never from
    loopback, and a socket bound to an interface address cannot send to
    127.0.0.1 on every OS (macOS refuses). So the stub is addressed by the
    machine's own address. Nothing is sent to find it: connecting a UDP socket
    only asks the routing table. With no network at all it is loopback, and
    then aiortc has no candidate to offer either.
    """
    probe = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        probe.connect(("192.0.2.1", 9))  # TEST-NET-1: never routed anywhere
        return probe.getsockname()[0]
    except OSError:
        return "127.0.0.1"
    finally:
        probe.close()


class StunStub:
    """Listens on a free UDP port until ``close()``; reach it at ``host``:``port``."""

    def __init__(self, turn_secret: str) -> None:
        self._secret = turn_secret.encode()
        self._sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self._sock.bind(("0.0.0.0", 0))  # noqa: S104 - a test stub, reached by this machine's own address
        self.host: str = _own_address()
        self.port: int = self._sock.getsockname()[1]
        #: Binding requests answered.
        self.bindings = 0
        #: The USERNAME of every authenticated Allocate request, oldest first.
        self.usernames: list[str] = []
        self._thread = threading.Thread(target=self._serve, name="stun-stub", daemon=True)
        self._thread.start()

    def close(self) -> None:
        self._sock.close()
        self._thread.join(timeout=2)

    def _signed(self, msg: bytes, username: bytes) -> bytes:
        """Appends MESSAGE-INTEGRITY under the long-term key of ``username``."""
        password = base64.b64encode(hmac.new(self._secret, username, hashlib.sha1).digest())
        key = hashlib.md5(username + b":" + REALM + b":" + password).digest()  # noqa: S324 - RFC 8489's key
        # The HMAC covers the message with its length already counting the attribute.
        covered = msg[:2] + struct.pack("!H", len(msg) - 20 + 24) + msg[4:]
        return covered + _attr(ATTR_MESSAGE_INTEGRITY, hmac.new(key, covered, hashlib.sha1).digest())

    def _serve(self) -> None:
        while True:
            try:
                msg, addr = self._sock.recvfrom(2048)
            except OSError:
                return  # closed
            if len(msg) < 20 or struct.unpack_from("!I", msg, 4)[0] != MAGIC:
                continue
            (type_,) = struct.unpack_from("!H", msg, 0)
            txid = msg[8:20]
            reply: bytes | None = None
            if type_ == BINDING_REQUEST:
                self.bindings += 1
                ip = struct.unpack("!I", socket.inet_aton(addr[0]))[0]
                value = struct.pack("!BBHI", 0, 0x01, addr[1] ^ (MAGIC >> 16), ip ^ MAGIC)
                reply = _message(BINDING_SUCCESS, txid, [_attr(ATTR_XOR_MAPPED_ADDRESS, value)])
            elif type_ == ALLOCATE_REQUEST:
                username = _find(msg, ATTR_USERNAME)
                if username is not None:
                    self.usernames.append(username.decode())
                    reply = self._signed(_message(ALLOCATE_ERROR, txid, [_error(486, b"Allocation Quota Reached")]), username)
                else:
                    reply = _message(
                        ALLOCATE_ERROR,
                        txid,
                        [_error(401, b"Unauthorized"), _attr(ATTR_REALM, REALM), _attr(ATTR_NONCE, b"0123456789abcdef")],
                    )
            if reply is not None:
                try:
                    self._sock.sendto(reply, addr)
                except OSError:
                    return
