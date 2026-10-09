"""The robot's half of the teleop data plane: twist over a WebRTC data channel.

The contract is protocol/README.md, "Teleop data plane (twist over WebRTC)".
This module answers the WebRTC offer of the operator who holds the robot's
lease and takes twist from the ``twist`` data channel. ``Robot`` owns one
``TwistAnswerer`` and feeds it leases and ``signal`` envelopes; you do not
normally use this module directly.

Authority does not move here. The robot answers only the operator named in its
current ``lease.granted`` (the server stamps ``from`` on every signal), every
twist on the channel still has to bear the current lease, and the peer is
closed the moment the lease ends. The lease check, the deadman and the
bus-shadow rule live in ``Robot``, in the one place both transports go through.

Needs the ``webrtc`` extra (``pip install 'fleet-sdk[webrtc]'``), which brings
in aiortc. Importing this module without it raises ImportError; importing
``fleet`` does not import this module.

aiortc gathers every ICE candidate before it produces the answer, so the
answer's SDP carries them all and this side sends no ``ice`` signals. It still
accepts the operator's trickled candidates. aiortc offers no loopback host
candidate: a robot and an operator on one machine connect through one of the
machine's own interface addresses, so the machine needs at least one.
"""

from __future__ import annotations

import asyncio
import json
import logging
import math
from collections.abc import Callable, Mapping, Sequence
from dataclasses import dataclass, field
from typing import Any, Optional, Union

from aiortc import (
    RTCConfiguration,
    RTCDataChannel,
    RTCIceCandidate,
    RTCIceServer,
    RTCPeerConnection,
    RTCSessionDescription,
)
from aiortc.sdp import candidate_from_sdp

from .client import FleetClient, FleetClientError

__all__ = [
    "MAX_MESSAGE_BYTES",
    "MAX_SEQ",
    "TWIST_CHANNEL_LABEL",
    "ChannelPing",
    "ChannelTwist",
    "IceServer",
    "TwistAnswerer",
    "parse_channel_message",
]

log = logging.getLogger("fleet.webrtc")

#: The one data channel the robot accepts; any other label is closed.
TWIST_CHANNEL_LABEL = "twist"
#: Anything larger on the channel is not a twist or a ping.
MAX_MESSAGE_BYTES = 1024
#: Largest ``seq`` accepted (the largest integer JSON carries exactly).
MAX_SEQ = 2**53 - 1

#: One STUN/TURN server: ``{"urls": ..., "username": ..., "credential": ...}`` or an RTCIceServer.
IceServer = Union[Mapping[str, Any], RTCIceServer]


@dataclass(frozen=True)
class ChannelPing:
    """The operator's liveness probe. The number is opaque and is echoed back."""

    value: Union[int, float]


@dataclass(frozen=True)
class ChannelTwist:
    """A twist from the channel: ``seq`` plus the bus ``twist`` payload."""

    seq: int
    #: ``{"lease_id": ..., "linear": {"x_mps": ...}, "angular": {"z_radps": ...}}``
    payload: dict[str, Any]


def parse_channel_message(data: Union[str, bytes]) -> Union[ChannelPing, ChannelTwist, None]:
    """Parses one data-channel message; None for anything that must be ignored.

    Ignored: over 1024 bytes, not UTF-8, not JSON, not an object, or not a ping
    or a twist of the documented shape (numbers must be finite, ``seq`` a
    positive integer).
    """
    raw = data.encode("utf-8") if isinstance(data, str) else bytes(data)
    if len(raw) > MAX_MESSAGE_BYTES:
        return None
    try:
        m = json.loads(raw.decode("utf-8"), parse_constant=_no_constants)
    except (ValueError, RecursionError):
        return None
    if not isinstance(m, dict):
        return None
    ping = m.get("ping")
    if _finite(ping):
        return ChannelPing(ping)

    lease_id, seq, linear, angular = m.get("lease_id"), m.get("seq"), m.get("linear"), m.get("angular")
    if not isinstance(lease_id, str) or not lease_id:
        return None
    # 42.0 is the integer 42 to every other JSON implementation; accept it as one.
    if not _finite(seq) or seq != int(seq) or not 1 <= seq <= MAX_SEQ:
        return None
    if not isinstance(linear, dict) or not isinstance(angular, dict):
        return None
    x, y, z = linear.get("x_mps"), linear.get("y_mps"), angular.get("z_radps")
    if not _finite(x) or not _finite(z) or (y is not None and not _finite(y)):
        return None
    lin: dict[str, Any] = {"x_mps": x}
    if y is not None:
        lin["y_mps"] = y
    return ChannelTwist(seq=int(seq), payload={"lease_id": lease_id, "linear": lin, "angular": {"z_radps": z}})


def _no_constants(name: str) -> Any:
    raise ValueError(f"{name} is not JSON")  # NaN / Infinity: Python's parser would take them


def _finite(v: Any) -> bool:
    return isinstance(v, (int, float)) and not isinstance(v, bool) and math.isfinite(v)


def _ice_servers(servers: Optional[Sequence[IceServer]]) -> list[RTCIceServer]:
    out: list[RTCIceServer] = []
    for s in servers or ():
        if isinstance(s, RTCIceServer):
            out.append(s)
            continue
        urls = s.get("urls")
        if isinstance(urls, str):
            pass
        elif isinstance(urls, Sequence) and urls and all(isinstance(u, str) for u in urls):
            urls = list(urls)
        else:
            raise ValueError("an ICE server needs 'urls': a URL or a list of URLs")
        out.append(RTCIceServer(urls=urls, username=s.get("username"), credential=s.get("credential")))
    return out


def _candidate(init: Any) -> Optional[RTCIceCandidate]:
    """An ``RTCIceCandidateInit`` from a signal as aiortc's candidate; None if unusable."""
    if not isinstance(init, Mapping):
        return None
    text = init.get("candidate")
    if not isinstance(text, str) or not text.strip():
        return None  # the end of candidates is not signaled; an empty one is ignored
    text = text.strip()
    for prefix in ("a=", "candidate:"):
        if text.startswith(prefix):
            text = text[len(prefix) :]
    try:
        cand = candidate_from_sdp(text)
    except Exception:  # noqa: BLE001 - aiortc asserts on short input
        return None
    mid, index = init.get("sdpMid"), init.get("sdpMLineIndex")
    cand.sdpMid = mid if isinstance(mid, str) else None
    cand.sdpMLineIndex = index if isinstance(index, int) and not isinstance(index, bool) else None
    if cand.sdpMid is None and cand.sdpMLineIndex is None:
        cand.sdpMLineIndex = 0  # both may be null; the offer has one section, the data channel
    return cand


@dataclass(eq=False)
class _Session:
    id: str
    operator_id: str
    pc: RTCPeerConnection
    remote_set: bool = False
    #: The operator's candidates that arrived before the offer was applied.
    remote_ice: list[RTCIceCandidate] = field(default_factory=list)
    channel: Optional[RTCDataChannel] = None
    #: Highest ``seq`` of a twist obeyed from this channel; anything at or below it is stale.
    last_seq: int = 0


class TwistAnswerer:
    """Answers the lease holder's offer and hands channel twists to ``on_twist``.

    ``on_twist(payload)`` gets the bus-shaped twist payload of every twist
    newer than the last one obeyed, and returns whether the robot obeyed it.
    Only an obeyed twist advances the ``seq`` mark.
    """

    def __init__(
        self,
        client: FleetClient,
        *,
        on_twist: Callable[[dict[str, Any]], bool],
        ice_servers: Optional[Sequence[IceServer]] = None,
    ) -> None:
        """
        Args:
            client: the robot's control connection; signals are sent on it.
            on_twist: see the class docstring.
            ice_servers: STUN/TURN servers. There is no default: with none
                given, only host candidates are used and nothing leaves the
                LAN (aiortc alone would fall back to a public STUN server).
        """
        self._client = client
        self._on_twist = on_twist
        self._ice = _ice_servers(ice_servers)
        self._lease: Optional[tuple[str, str]] = None  # (lease_id, operator_id)
        self._session: Optional[_Session] = None
        self._signals = 0
        self._tasks: set[asyncio.Future[Any]] = set()

    @property
    def open(self) -> bool:
        """Whether a twist data channel is open right now."""
        s = self._session
        return s is not None and s.channel is not None and s.channel.readyState == "open"

    def grant(self, lease_id: str, operator_id: str) -> None:
        """lease.granted: only this lease's operator may connect. A peer from an earlier lease is closed."""
        if self._lease is None or self._lease[0] != lease_id:
            self.close_peer()
        self._lease = (lease_id, operator_id)

    def revoke(self) -> None:
        """The lease is over (released, stolen, expired, operator lost): close the peer."""
        self._lease = None
        self.close_peer()

    def close_peer(self) -> None:
        """Closes the peer connection, if any, and keeps the lease: the operator may offer again."""
        s = self._session
        if s is None:
            return
        self._session = None
        log.info("closing teleop peer connection (session %s)", s.id)
        if s.channel is not None:
            try:
                s.channel.close()
            except Exception:  # noqa: BLE001 - already closed
                pass
        self._spawn(self._close_pc(s.pc))

    async def aclose(self) -> None:
        """Forgets the lease, closes the peer and waits for it to be gone."""
        self.revoke()
        while self._tasks:
            await asyncio.gather(*list(self._tasks), return_exceptions=True)

    def on_signal(self, sig: Mapping[str, Any]) -> None:
        """A ``signal`` payload relayed by the server. Anything not from this lease's operator is ignored."""
        lease = self._lease
        if lease is None or sig.get("from") != lease[1]:
            return
        d = sig.get("data")
        if not isinstance(d, Mapping) or not isinstance(d.get("session"), str):
            return
        kind = sig.get("kind")
        if kind == "offer":
            sdp = d.get("sdp")
            if d.get("lease_id") != lease[0] or not isinstance(sdp, str):
                return
            self._offer(d["session"], lease[1], sdp)
            return
        s = self._session
        if kind != "ice" or s is None or s.id != d["session"]:
            return
        cand = _candidate(d.get("candidate"))
        if cand is None:
            return
        if s.remote_set:
            self._spawn(self._add_ice(s, cand))
        else:
            s.remote_ice.append(cand)

    # ------------------------------------------------------------------ internals

    def _offer(self, session_id: str, operator_id: str, sdp: str) -> None:
        """A new offer replaces whatever peer was there: one peer per lease, the newest."""
        self.close_peer()
        # Explicit, so nothing leaves the machine unless a server list was given.
        pc = RTCPeerConnection(RTCConfiguration(iceServers=list(self._ice)))
        s = _Session(id=session_id, operator_id=operator_id, pc=pc)
        self._session = s

        def on_channel(channel: RTCDataChannel) -> None:
            if self._session is not s or channel.label != TWIST_CHANNEL_LABEL or s.channel is not None:
                channel.close()
                return
            s.channel = channel
            channel.on("message", lambda data: self._on_message(s, channel, data))
            channel.on("close", lambda: self._gone(s, "data channel closed"))
            log.info("twist data channel open (session %s)", s.id)

        def on_state() -> None:
            if pc.connectionState in ("failed", "closed"):
                self._gone(s, f"peer connection {pc.connectionState}")

        pc.on("datachannel", on_channel)
        pc.on("connectionstatechange", on_state)
        self._spawn(self._answer(s, sdp))

    async def _answer(self, s: _Session, sdp: str) -> None:
        pc = s.pc
        try:
            await pc.setRemoteDescription(RTCSessionDescription(sdp=sdp, type="offer"))
            s.remote_set = True
            held, s.remote_ice = s.remote_ice, []
            for cand in held:
                await self._add_ice(s, cand)
            # Gathers every candidate before returning; they all ride in the answer.
            await pc.setLocalDescription(await pc.createAnswer())
            if self._session is not s:
                return
            self._signals += 1
            await self._client.send(
                "signal",
                {
                    "to": s.operator_id,
                    "kind": "answer",
                    "data": {"session": s.id, "type": "answer", "sdp": pc.localDescription.sdp},
                },
                id=f"p2p.signal.{self._signals}",
            )
        except FleetClientError:
            # Control link down: the operator's attempt times out and it offers again.
            if self._session is s:
                self.close_peer()
        except Exception as e:  # noqa: BLE001 - a bad offer must never break the robot
            if self._session is s:
                log.warning("offer refused: %s", e)
                self.close_peer()

    async def _add_ice(self, s: _Session, cand: RTCIceCandidate) -> None:
        if self._session is not s:
            return
        try:
            await s.pc.addIceCandidate(cand)
        except Exception as e:  # noqa: BLE001 - one bad candidate is not fatal
            log.debug("ignoring ICE candidate: %s", e)

    def _gone(self, s: _Session, why: str) -> None:
        # Keeps the lease: the operator falls back to the bus and may offer again.
        if self._session is s:
            log.info("%s (session %s)", why, s.id)
            self.close_peer()

    def _on_message(self, s: _Session, channel: RTCDataChannel, data: Union[str, bytes]) -> None:
        if self._session is not s:
            return
        msg = parse_channel_message(data)
        if isinstance(msg, ChannelPing):
            # Liveness for the operator's side. It is not a twist: the deadman does not see it.
            try:
                channel.send(json.dumps({"pong": msg.value}))
            except Exception:  # noqa: BLE001 - closing
                pass
            return
        if msg is None:
            return
        # Unordered and unreliable: only a twist newer than the last one obeyed
        # counts. A twist refused for its lease must not move the mark, or a
        # stranger's large seq would deafen the robot to its real driver.
        if msg.seq <= s.last_seq:
            return
        if self._on_twist(msg.payload):
            s.last_seq = msg.seq

    @staticmethod
    async def _close_pc(pc: RTCPeerConnection) -> None:
        try:
            await pc.close()
        except Exception as e:  # noqa: BLE001
            log.debug("closing peer connection: %s", e)

    def _spawn(self, coro: Any) -> None:
        task = asyncio.ensure_future(coro)
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)
