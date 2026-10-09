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

The peer connection's ICE servers are the installation's, asked of the server
(protocol/README.md, "ICE servers"): once when a lease is taken, so the answer
is there by the time the offer comes, and kept for that lease until the TURN
credential in it expires. A list given to ``Robot(ice_servers=...)`` replaces
that: it is used as given and the server is never asked.

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
import functools
import json
import logging
import math
import time
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
    "GATHER_TIMEOUT_S",
    "ICE_EXPIRY_MARGIN_MS",
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
#: A stored ICE answer is not used for a new peer this close to its expiry: the
#: TURN server checks the credential when the relay is allocated, a moment after
#: the peer connection is created.
ICE_EXPIRY_MARGIN_MS = 10_000
#: How long the answer waits for STUN and TURN servers that do not reply. The
#: operator abandons an offer that has no live channel after 5 s, and aiortc
#: answers only when gathering is over, so the wait must be well inside that.
GATHER_TIMEOUT_S = 2.0

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


def _bound_gathering(pc: RTCPeerConnection, seconds: float) -> None:
    """Caps how long ``pc`` waits for ICE servers that do not reply. Call before setLocalDescription.

    aiortc asks the STUN server from every interface address of the machine
    and finishes gathering only when each has answered or five seconds have
    passed. One interface that cannot reach the server (a VPN, a container
    bridge) is enough to hold the answer back those five seconds, which is
    the operator's whole connect timeout: the direct path would then never
    come up on such a machine. Whatever was gathered by the deadline is used;
    a server that answers later is simply not a candidate for this session.

    The timeout is a parameter of aioice's gatherer that aiortc does not pass
    on, so it is set on the connection underneath. If a later aiortc is laid
    out differently this does nothing and the five seconds stand.
    """
    try:
        connection = pc.sctp.transport.transport.iceGatherer._connection  # noqa: SLF001
        gather = connection.get_component_candidates
        if not isinstance(gather, functools.partial):
            connection.get_component_candidates = functools.partial(gather, timeout=seconds)
    except AttributeError as e:
        log.debug("cannot bound ICE gathering on this aiortc: %s", e)


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
class _Pending:
    """An offer taken but not yet answered with a peer: its ICE servers are still being asked for."""

    id: str
    #: The operator's candidates that arrive meanwhile; the session inherits them.
    remote_ice: list[RTCIceCandidate] = field(default_factory=list)


@dataclass(eq=False)
class _LeaseIce:
    """The server's ICE answer for one lease. ``got`` resolves to None when there was none."""

    lease_id: str
    #: ``(servers, expires_at_ms or None)``, or None.
    got: "asyncio.Future[Optional[tuple[list[RTCIceServer], Optional[float]]]]"


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
        now_ms: Optional[Callable[[], float]] = None,
    ) -> None:
        """
        Args:
            client: the robot's control connection; signals are sent on it,
                and the ICE servers are asked for on it.
            on_twist: see the class docstring.
            ice_servers: None (the default) asks the server for the
                installation's STUN/TURN servers when a lease is taken. A
                list, the empty one included, is used as given for every peer
                connection and the server is never asked. Either way the peer
                connection is created with an explicit list, so nothing is
                asked of a server nobody configured (aiortc alone would fall
                back to a public STUN server).
            now_ms: the clock ``expires_at_ms`` is compared with, in epoch
                milliseconds. Default: the system clock.
        """
        self._client = client
        self._on_twist = on_twist
        self._static: Optional[list[RTCIceServer]] = _ice_servers(ice_servers) if ice_servers is not None else None
        self._now_ms: Callable[[], float] = now_ms if now_ms is not None else (lambda: time.time() * 1000)
        self._ice: Optional[_LeaseIce] = None
        #: The ICE servers the newest peer connection was created with; None before the first.
        self.peer_ice_servers: Optional[list[RTCIceServer]] = None
        #: How many times the server has been asked for ICE servers.
        self.ice_requests = 0
        self._lease: Optional[tuple[str, str]] = None  # (lease_id, operator_id)
        self._session: Optional[_Session] = None
        self._pending: Optional[_Pending] = None
        self._signals = 0
        self._tasks: set[asyncio.Future[Any]] = set()

    @property
    def open(self) -> bool:
        """Whether a twist data channel is open right now."""
        s = self._session
        return s is not None and s.channel is not None and s.channel.readyState == "open"

    def grant(self, lease_id: str, operator_id: str) -> None:
        """The robot holds this lease (lease.granted, or the welcome of a connection that states it).

        Only its operator may connect. A peer from an earlier lease is closed.
        For a lease that is new to the robot the ICE servers are asked for
        now, ahead of the offer; a renewal asks for nothing.
        """
        if self._lease is None or self._lease[0] != lease_id:
            self.close_peer()
            self._ice = None
            if self._static is None:
                self._ask_ice(lease_id)
        self._lease = (lease_id, operator_id)

    def revoke(self) -> None:
        """The lease is over (released, stolen, expired, operator lost): close the peer."""
        self._lease = None
        self._ice = None  # the answer belonged to the lease
        self.close_peer()

    def close_peer(self) -> None:
        """Closes the peer connection, if any, and keeps the lease: the operator may offer again."""
        self._pending = None  # an offer still waiting for its ICE servers is abandoned
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
            tasks = list(self._tasks)
            await asyncio.gather(*tasks, return_exceptions=True)
            # Not left to the done callbacks: awaiting tasks that are already
            # finished does not yield to the loop, so they would never run
            # and this loop would spin.
            self._tasks.difference_update(tasks)

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
            self._offer(d["session"], lease, sdp)
            return
        if kind != "ice":
            return
        s = self._session if self._session is not None and self._session.id == d["session"] else None
        waiting = self._pending if self._pending is not None and self._pending.id == d["session"] else None
        if s is None and waiting is None:
            return
        cand = _candidate(d.get("candidate"))
        if cand is None:
            return
        if s is not None and s.remote_set:
            self._spawn(self._add_ice(s, cand))
        else:
            (s if s is not None else waiting).remote_ice.append(cand)  # type: ignore[union-attr]

    # ------------------------------------------------------------------ internals

    def _ask_ice(self, lease_id: str) -> _LeaseIce:
        """Asks the server for this lease's ICE servers and keeps the answer while it is the lease's."""
        self.ice_requests += 1
        entry = _LeaseIce(lease_id=lease_id, got=asyncio.get_running_loop().create_future())

        async def ask() -> None:
            got: Optional[tuple[list[RTCIceServer], Optional[float]]] = None
            try:
                cfg = await self._client.ice_config()
                expires = cfg.get("expires_at_ms")
                got = (
                    _ice_servers(cfg.get("ice_servers")),
                    float(expires) if isinstance(expires, (int, float)) and not isinstance(expires, bool) else None,
                )
            except Exception as e:  # noqa: BLE001 - see below
                # No answer in 2 s, a refusal, a dropped link, a server older
                # than the message (invalid_message), or a list aiortc cannot
                # use. None of them is a reason to refuse the offer; forget
                # it, so the next offer asks again.
                if self._ice is entry:
                    self._ice = None
                log.info("no ICE servers from the server (%s); using none", e)
            entry.got.set_result(got)

        self._ice = entry
        self._spawn(ask())
        return entry

    async def _ice_for(self, lease_id: str) -> list[RTCIceServer]:
        """The ICE servers for a peer connection made now under ``lease_id``. Never raises.

        The fixed list if one was configured; else the lease's stored answer,
        asked for again if there is none or its credential is at its expiry.
        With nothing to go on it is the empty list, stated explicitly.
        """
        if self._static is not None:
            return list(self._static)

        def fresh(got: tuple[list[RTCIceServer], Optional[float]]) -> bool:
            return got[1] is None or self._now_ms() < got[1] - ICE_EXPIRY_MARGIN_MS

        entry = self._ice if self._ice is not None and self._ice.lease_id == lease_id else self._ask_ice(lease_id)
        got = await entry.got
        # Asked again at most once per offer: the operator is waiting for the answer.
        if got is not None and not fresh(got) and self._lease is not None and self._lease[0] == lease_id:
            got = await self._ask_ice(lease_id).got
        return list(got[0]) if got is not None and fresh(got) else []

    def _offer(self, session_id: str, lease: tuple[str, str], sdp: str) -> None:
        """A new offer replaces whatever peer was there: one peer per lease, the newest."""
        self.close_peer()
        waiting = _Pending(id=session_id)
        self._pending = waiting
        self._spawn(self._begin(waiting, lease, sdp))

    async def _begin(self, waiting: _Pending, lease: tuple[str, str], sdp: str) -> None:
        servers = await self._ice_for(lease[0])
        # A newer offer, a revocation or a steal while the ICE servers were asked for.
        if self._pending is not waiting or self._lease is None or self._lease[0] != lease[0]:
            return
        self._pending = None
        self.peer_ice_servers = servers
        # Always explicit, the empty list included: nothing is asked of a
        # server the installation did not configure.
        pc = RTCPeerConnection(RTCConfiguration(iceServers=list(servers)))
        s = _Session(id=waiting.id, operator_id=lease[1], pc=pc, remote_ice=waiting.remote_ice)
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
        await self._answer(s, sdp)

    async def _answer(self, s: _Session, sdp: str) -> None:
        pc = s.pc
        try:
            await pc.setRemoteDescription(RTCSessionDescription(sdp=sdp, type="offer"))
            s.remote_set = True
            held, s.remote_ice = s.remote_ice, []
            for cand in held:
                await self._add_ice(s, cand)
            # Gathers every candidate before returning; they all ride in the answer.
            _bound_gathering(pc, GATHER_TIMEOUT_S)
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
