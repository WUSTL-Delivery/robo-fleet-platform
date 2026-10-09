"""Robot: the robot role on top of FleetClient.

A Robot owns one FleetClient (composition, not inheritance) and adds the
robot half of the wire protocol (docs/INTEGRATION.md, docs/DESIGN.md D3):

- **Manifest** on every (re)connect. The server forgets a robot's manifest when
  it drops, so the robot declares itself again each time the link comes back.
- **Telemetry**: pose in a declared frame, geographic OR local Cartesian (D7),
  plus optional battery, velocity and health. Lossy by design: dropped while the
  link is down.
- **Help**: ``request_help(reason)`` raises the robot's hand for the
  intervention queue (AUTONOMOUS -> HELP_REQUESTED).
- **Leases and twist, fail closed**: the robot obeys twist only when it bears
  the lease id it currently holds. A deadman commands zero velocity
  ``deadman_ms`` (300 ms) after the last valid twist, and a lease revoke or a
  lost link commands zero velocity immediately. The robot never waits for the
  server to say the operator left.
- **A lease is never trusted across a reconnect.** The server keeps a lease
  when the robot's link blips, but the lease may also have ended while the
  robot was away, with nobody to tell. So the ``welcome`` of every connection
  states the lease the server holds for this robot, and the robot replaces
  what it believed with that: the same lease is kept, another one is taken as
  a grant, none (or a welcome that does not say, from an older server) drops
  it. Until that welcome the robot obeys nothing (protocol/README.md, "A
  robot's lease at connect").
- **Twist over WebRTC** (optional; protocol/README.md, "Teleop data plane"):
  with the ``webrtc`` extra installed, the robot answers the lease holder's
  WebRTC offer and also takes twist from the ``twist`` data channel. Both
  transports go through the same lease check and the same deadman, so
  ``on_twist`` behaves the same on either. Without the extra, or when no peer
  connection comes up, twist arrives over the bus as before.

Every hook is shaped so a ROS 2 node can map it one to one::

    on_twist(handler)       -> publish geometry_msgs/Twist on cmd_vel
    on_lease(handler)       -> publish the teleop lease state (a latched topic)
    telemetry(pose=...)     <- subscription on odometry / fix / battery_state
    request_help(reason)    <- a std_srvs-style service
    robot.lease / .mode     -> parameters or a status topic

Handlers may be plain functions or coroutine functions. Twist handlers are
called synchronously from the event loop the moment the command is decided (a
coroutine handler is scheduled as a task), so the deadman's timing is not at
the mercy of a slow handler elsewhere.
"""

from __future__ import annotations

import asyncio
import inspect
import logging
from collections.abc import Awaitable, Callable, Mapping, Sequence
from dataclasses import dataclass
from typing import Any, Literal, Optional, Union

from .channel import Channel
from .client import Backoff, ConnectionState, Envelope, FleetClient, FleetClientError, StateChange
from .protocol import GeoPose, LocalPose, Manifest, Pose, TelemetryPayload
from .token_store import TokenStore

__all__ = [
    "DEADMAN_MS",
    "LeaseChange",
    "Mode",
    "Robot",
    "TwistCommand",
    "TwistSource",
    "TwistVia",
    "geo_pose",
    "local_pose",
]

log = logging.getLogger("fleet.robot")

#: Deadman window (DESIGN.md D3): this long without a valid twist -> zero velocity.
DEADMAN_MS = 300

#: The robot's own view of its intervention state.
Mode = Literal["autonomous", "help", "teleop"]

#: Why a twist command was issued: an operator setpoint, or one of the three stops.
TwistSource = Literal["operator", "deadman", "revoked", "disconnected"]

#: How an operator's twist reached the robot: the control-plane bus, or the WebRTC data channel.
TwistVia = Literal["bus", "p2p"]


@dataclass(frozen=True)
class TwistCommand:
    """A body-frame velocity setpoint for the robot's controller (``cmd_vel``).

    ``source`` is ``"operator"`` for a setpoint from the lease holder; every
    other source is a stop the SDK issued itself and carries zero velocity.
    """

    linear_x: float
    linear_y: float
    angular_z: float
    source: TwistSource
    #: The lease the command was issued under; None for a stop after the lease is gone.
    lease_id: Optional[str]
    #: The transport an operator setpoint arrived on; None for a stop the SDK issued.
    via: Optional[TwistVia] = None

    @property
    def is_stop(self) -> bool:
        return self.linear_x == 0 and self.linear_y == 0 and self.angular_z == 0


@dataclass(frozen=True)
class LeaseChange:
    """A lease granted to or revoked from this robot, as handed to ``on_lease``."""

    granted: bool
    lease_id: str
    #: The operator driving (set on grant).
    operator_id: Optional[str] = None
    #: Server-side expiry of the lease, epoch ms (set on grant).
    expires_at_ms: Optional[int] = None
    #: Why it ended (set on revoke): released, expired, stolen, operator_lost.
    #: None on a revoke when the lease ended while the robot was disconnected:
    #: the welcome after the reconnect says it is gone, not why.
    reason: Optional[str] = None


LeaseHandler = Callable[[LeaseChange], Union[None, Awaitable[None]]]
TwistHandler = Callable[[TwistCommand], Union[None, Awaitable[None]]]


def geo_pose(lat: float, lon: float, *, yaw_rad: float | None = None, alt_m: float | None = None) -> GeoPose:
    """A geographic pose (WGS84 lat/lon), for robots with GPS."""
    p: dict[str, Any] = {"frame": "geographic", "lat": float(lat), "lon": float(lon)}
    if yaw_rad is not None:
        p["yaw_rad"] = float(yaw_rad)
    if alt_m is not None:
        p["alt_m"] = float(alt_m)
    return p  # type: ignore[return-value]


def local_pose(
    x_m: float,
    y_m: float,
    *,
    yaw_rad: float | None = None,
    frame_id: str | None = None,
    z_m: float | None = None,
) -> LocalPose:
    """A pose in a local Cartesian frame (e.g. ROS ``map``), for robots without GPS."""
    p: dict[str, Any] = {"frame": "local", "x_m": float(x_m), "y_m": float(y_m)}
    if frame_id is not None:
        p["frame_id"] = frame_id
    if yaw_rad is not None:
        p["yaw_rad"] = float(yaw_rad)
    if z_m is not None:
        p["z_m"] = float(z_m)
    return p  # type: ignore[return-value]


class Robot:
    """One robot's connection to fleet-server.

    Example::

        robot = Robot(url, manifest={"drive": {"type": "twist", "max_v_mps": 1.0, "max_w_radps": 1.5}},
                      name="bot-1", enrollment_key=key, token_store=FileTokenStore("bot-1.json"))
        robot.on_twist(lambda cmd: drive(cmd.linear_x, cmd.angular_z))
        await robot.connect()
        await robot.telemetry(pose=local_pose(1.0, 2.0, yaw_rad=0.3, frame_id="map"), battery=87)
    """

    def __init__(
        self,
        url: str,
        *,
        manifest: Manifest | Mapping[str, Any],
        name: str | None = None,
        enrollment_key: str | None = None,
        token_store: TokenStore | None = None,
        agent: Mapping[str, Any] | None = None,
        reconnect: Backoff | None = Backoff(),
        handshake_timeout: float = 10.0,
        deadman_ms: int = DEADMAN_MS,
        data_channel: bool | None = None,
        ice_servers: Sequence[Any] | None = None,
    ) -> None:
        """
        Args:
            url: the server's WebSocket endpoint, e.g. ``ws://localhost:8080/ws``.
            manifest: what this robot can do (``protocol.Manifest``). Twist is
                obeyed only if it declares a ``drive`` block.
            name, enrollment_key, token_store, agent, reconnect, handshake_timeout:
                passed to the underlying FleetClient (kind is always ``robot``).
            deadman_ms: zero velocity this long after the last valid twist.
            data_channel: whether to answer the lease holder's WebRTC offer and
                take twist from the data channel as well as the bus. None (the
                default) turns it on when the ``webrtc`` extra (aiortc) is
                installed; True requires it (ImportError if it is missing);
                False keeps the robot on bus twist only.
            ice_servers: STUN/TURN servers for the peer connection, each
                ``{"urls": ..., "username": ..., "credential": ...}``. There is
                no default: with none, only host candidates are used, which is
                enough on loopback or one LAN.
        """
        self._client = FleetClient(
            url,
            kind="robot",
            name=name,
            enrollment_key=enrollment_key,
            token_store=token_store,
            agent=agent,
            reconnect=reconnect,
            handshake_timeout=handshake_timeout,
        )
        self._manifest: dict[str, Any] = dict(manifest)
        self._deadman_s = deadman_ms / 1000
        self._lease_id: str | None = None
        self._lease: LeaseChange | None = None
        self._mode: Mode = "autonomous"
        self._deadman: asyncio.TimerHandle | None = None
        self._last_twist: TwistCommand | None = None
        self._lease_handlers: list[LeaseHandler] = []
        self._twist_handlers: list[TwistHandler] = []
        self._tasks: set[asyncio.Task[Any]] = set()
        #: Loop time of the last data-channel twist obeyed (the bus-shadow rule).
        self._p2p_at = float("-inf")
        self._peer: Any = None  # fleet.webrtc.TwistAnswerer when the data channel is on
        if data_channel is not False:
            try:
                from .webrtc import TwistAnswerer
            except ImportError as e:
                if data_channel:
                    raise ImportError(
                        "data_channel=True needs aiortc: pip install 'fleet-sdk[webrtc]'"
                    ) from e
            else:
                self._peer = TwistAnswerer(self._client, on_twist=self._on_channel_twist, ice_servers=ice_servers)
                self._client.on("signal", self._on_signal)

        self._client.on_state(self._on_state)
        self._client.on("lease.granted", self._on_granted)
        self._client.on("lease.revoked", self._on_revoked)
        self._client.on("twist", self._on_twist_env)

    # ------------------------------------------------------------------ properties

    @property
    def client(self) -> FleetClient:
        """The underlying connection (for on/send of message types Robot does not wrap)."""
        return self._client

    @property
    def robot_id(self) -> str | None:
        return self._client.client_id

    @property
    def state(self) -> ConnectionState:
        return self._client.state

    @property
    def manifest(self) -> dict[str, Any]:
        return dict(self._manifest)

    @property
    def lease(self) -> LeaseChange | None:
        """The lease currently held (the grant), or None."""
        return self._lease

    @property
    def lease_id(self) -> str | None:
        return self._lease_id

    @property
    def mode(self) -> Mode:
        """autonomous, help (hand raised or lease lost without handback), or teleop."""
        return self._mode

    @property
    def last_twist(self) -> TwistCommand | None:
        """The last command handed to the twist handlers."""
        return self._last_twist

    @property
    def data_channel_enabled(self) -> bool:
        """Whether this robot answers WebRTC offers (the ``webrtc`` extra is installed and not turned off)."""
        return self._peer is not None

    @property
    def data_channel_open(self) -> bool:
        """Whether a twist data channel to the lease holder is open right now."""
        return self._peer is not None and self._peer.open

    # ------------------------------------------------------------------ lifecycle

    async def connect(self) -> dict[str, Any]:
        """Enrolls if needed, connects, declares the manifest; returns the welcome."""
        return await self._client.connect()

    async def close(self) -> None:
        """Stops (zero velocity if a lease was held) and closes the connection."""
        try:
            await self._client.close()
        finally:
            if self._peer is not None:
                await self._peer.aclose()

    async def run_forever(self) -> None:
        """Connects and stays connected until close() or a terminal failure."""
        await self._client.run_forever()

    # ------------------------------------------------------------------ robot -> server

    async def set_manifest(self, manifest: Manifest | Mapping[str, Any]) -> None:
        """Replaces the manifest; sends it now if connected, and on every reconnect."""
        self._manifest = dict(manifest)
        if self._client.state is ConnectionState.OPEN:
            await self._client.send("manifest", self._manifest)

    async def telemetry(
        self,
        *,
        pose: Pose | Mapping[str, Any] | None = None,
        battery: float | Mapping[str, Any] | None = None,
        velocity: Mapping[str, Any] | None = None,
        health: Mapping[str, Any] | None = None,
    ) -> bool:
        """Sends one telemetry sample. Returns False (and drops it) if not connected.

        Args:
            pose: from ``geo_pose(...)`` or ``local_pose(...)``: frame-relative, never
                bare lat/lon (D7).
            battery: percent (0-100) or ``{"pct": ..., "voltage": ...}``.
            velocity: ``{"v_mps": ..., "w_radps": ...}``.
            health: flat map of string/number/bool readings.
        """
        payload: dict[str, Any] = {}
        if pose is not None:
            frame = pose.get("frame")
            if frame not in ("geographic", "local"):
                raise ValueError("pose needs frame 'geographic' or 'local'; build it with geo_pose() or local_pose()")
            payload["pose"] = dict(pose)
        if battery is not None:
            payload["battery"] = {"pct": float(battery)} if isinstance(battery, (int, float)) else dict(battery)
        if velocity is not None:
            payload["velocity"] = dict(velocity)
        if health is not None:
            payload["health"] = dict(health)
        typed: TelemetryPayload = payload  # type: ignore[assignment]
        if self._client.state is not ConnectionState.OPEN:
            return False
        try:
            await self._client.send("telemetry", typed)
        except FleetClientError:
            return False  # the link dropped mid-send; telemetry is lossy
        return True

    async def request_help(self, reason: str, context: Mapping[str, Any] | None = None) -> None:
        """Raises the robot's hand (help.request). Raises FleetClientError if not connected."""
        if not reason or len(reason) > 256:
            raise ValueError("reason must be 1-256 characters")
        payload: dict[str, Any] = {"reason": reason}
        if context is not None:
            payload["context"] = dict(context)
        await self._client.send("help.request", payload)
        if self._mode == "autonomous":
            self._mode = "help"

    # ------------------------------------------------------------------ server -> robot

    def on_lease(self, handler: LeaseHandler) -> Callable[[], None]:
        """Calls ``handler(LeaseChange)`` when a lease for this robot is granted or revoked."""
        return _add(self._lease_handlers, handler)

    def on_twist(self, handler: TwistHandler) -> Callable[[], None]:
        """Calls ``handler(TwistCommand)`` for each valid twist and for every stop.

        Valid means it bears the lease this robot holds and the manifest declares
        a drive. It may have come over the bus or the WebRTC data channel
        (``cmd.via``); the lease check and the deadman are the same. Stops (zero velocity) come from the deadman, a revoke, a new
        lease replacing a moving one, a lost link, or a reconnect that finds the
        lease gone (source ``revoked``).
        """
        return _add(self._twist_handlers, handler)

    def channel(self, name: str) -> Channel:
        """Domain channel ``name`` on this robot's connection (``publish`` / ``on_message``).

        Broadcasts reach a robot only for channels its manifest declares under
        ``channels``; directed messages always do. See fleet/channel.py.
        """
        return self._client.channel(name)

    # ------------------------------------------------------------------ internals

    def _on_state(self, change: StateChange) -> Awaitable[None] | None:
        # Synchronous on purpose: the stop on a lost link must not wait a loop turn.
        if change.state is ConnectionState.OPEN:
            # Before anything else on this connection is handled, and in the same
            # loop turn that made channel twist obeyable again.
            self._confirm_lease(change.welcome)
            return self._declare()  # scheduled by the client as its own task
        if change.state in (ConnectionState.RECONNECTING, ConnectionState.CLOSED):
            self._halt("disconnected")
            self._p2p_at = float("-inf")
            # A blip keeps the peer (channel twist is not obeyed until the link
            # is back); a closed client will never hear a revoke, so it goes.
            if change.state is ConnectionState.CLOSED and self._peer is not None:
                self._peer.revoke()
        return None

    def _confirm_lease(self, welcome: Mapping[str, Any] | None) -> None:
        """Replaces what the robot believes about its lease with what the welcome states.

        The server does not revoke a lease because the robot dropped, so a blip
        normally finds the same lease here and the operator's twist resumes.
        But it may have expired, been handed back or lost its operator while
        the robot could not be told. Fail closed: the lease held before the
        reconnect counts for nothing unless this welcome names it. A welcome
        with ``"lease": null`` and one with no ``lease`` at all (a server that
        predates the field) are the same answer: nothing is confirmed.
        """
        stated = welcome.get("lease") if welcome is not None else None
        if isinstance(stated, Mapping) and stated.get("robot_id") == self._client.client_id:
            lease_id = stated.get("lease_id")
            if self._lease is not None and lease_id == self._lease_id:
                # Still ours. Nothing to announce; the expiry may have moved.
                self._lease = LeaseChange(
                    granted=True,
                    lease_id=self._lease.lease_id,
                    operator_id=self._lease.operator_id,
                    expires_at_ms=stated.get("expires_at_ms"),
                )
                return
            if self._grant(stated):
                return  # a lease this robot did not know of (it restarted, or the grant crossed the drop)
        if self._lease_id is not None:
            log.warning("lease %s ended while disconnected; dropping it", self._lease_id)
            self._drop(None)

    async def _declare(self) -> None:
        # Declare on every (re)connect: the server forgets the manifest on a drop.
        try:
            await self._client.send("manifest", self._manifest)
        except FleetClientError as e:
            log.warning("could not send manifest: %s", e)

    def _on_granted(self, env: Envelope) -> None:
        self._grant(env["payload"])

    def _grant(self, p: Mapping[str, Any]) -> bool:
        """Takes the lease in ``p`` (lease.granted, or the welcome's lease). False if it is not one for this robot."""
        lease_id = p.get("lease_id")
        if not isinstance(lease_id, str) or not lease_id:
            return False
        if p.get("robot_id") != self._client.client_id:
            return False
        if self._lease_id is not None and lease_id != self._lease_id:
            # A steal: the old driver's last setpoint must not carry over.
            self._halt("revoked")
            self._p2p_at = float("-inf")
        self._lease_id = lease_id
        self._mode = "teleop"
        self._lease = LeaseChange(
            granted=True,
            lease_id=lease_id,
            operator_id=p.get("operator_id"),
            expires_at_ms=p.get("expires_at_ms"),
        )
        if self._peer is not None:
            operator_id = p.get("operator_id")
            if isinstance(operator_id, str) and operator_id:
                # Only this operator's offer is answered; a peer from an earlier lease is closed.
                self._peer.grant(lease_id, operator_id)
            else:
                self._peer.revoke()
        self._emit(self._lease_handlers, self._lease)
        return True

    def _on_revoked(self, env: Envelope) -> None:
        p = env["payload"]
        lease_id = p.get("lease_id")
        if lease_id is None or lease_id != self._lease_id:
            return
        self._drop(p.get("reason"))

    def _drop(self, reason: str | None) -> None:
        """Stops and forgets the lease held. ``reason`` is None when no revocation was heard."""
        lease_id = self._lease_id
        if lease_id is None:
            return
        self._halt("revoked")
        self._lease_id = None
        self._lease = None
        self._p2p_at = float("-inf")
        if self._peer is not None:
            self._peer.revoke()  # cleanup that follows the lease decision, never the cause of it
        # Handback resumes autonomy; expiry / operator loss / steal leave it needing help.
        # So does a lease that ended unheard: without the reason, assume the cautious one.
        self._mode = "autonomous" if reason == "released" else "help"
        self._emit(self._lease_handlers, LeaseChange(granted=False, lease_id=lease_id, reason=reason))

    def _on_twist_env(self, env: Envelope) -> None:
        self._obey(env["payload"], "bus")

    def _on_channel_twist(self, payload: Mapping[str, Any]) -> bool:
        """A twist from the data channel, already newer than the last one obeyed there."""
        return self._obey(payload, "p2p")

    def _on_signal(self, env: Envelope) -> None:
        self._peer.on_signal(env["payload"])

    def _obey(self, p: Mapping[str, Any], via: TwistVia) -> bool:
        """The one gate every operator twist passes, whichever transport it came on.

        Returns whether it was obeyed.
        """
        if self._lease_id is None or p.get("lease_id") != self._lease_id:
            return False  # not the current lease: never obey
        if "drive" not in self._manifest:
            return False  # declared no drive: nothing to obey
        now = asyncio.get_running_loop().time()
        if via == "p2p":
            if self._client.state is not ConnectionState.OPEN:
                return False  # no control link: a revocation could not be heard
        elif now - self._p2p_at <= self._deadman_s:
            # A bus twist still in flight when the operator moved to the faster
            # path must not replace a newer setpoint.
            return False
        try:
            linear = p["linear"]
            cmd = TwistCommand(
                linear_x=float(linear["x_mps"]),
                linear_y=float(linear.get("y_mps", 0.0)),
                angular_z=float(p["angular"]["z_radps"]),
                source="operator",
                lease_id=self._lease_id,
                via=via,
            )
        except (KeyError, TypeError, ValueError, AttributeError):
            log.warning("ignoring malformed twist: %r", p)
            return False
        if via == "p2p":
            self._p2p_at = now
        self._arm_deadman()
        self._command(cmd)
        return True

    def _arm_deadman(self) -> None:
        if self._deadman is not None:
            self._deadman.cancel()
        self._deadman = asyncio.get_running_loop().call_later(self._deadman_s, self._deadman_fired)

    def _deadman_fired(self) -> None:
        self._deadman = None
        if self._lease_id is not None:
            self._command(_stop("deadman", self._lease_id))

    def _halt(self, source: TwistSource) -> None:
        """Zero velocity now, if the robot may be moving under a lease."""
        armed = self._deadman is not None
        if armed:
            self._deadman.cancel()  # type: ignore[union-attr]
            self._deadman = None
        if armed or self._lease_id is not None:
            self._command(_stop(source, self._lease_id if source == "disconnected" else None))

    def _command(self, cmd: TwistCommand) -> None:
        self._last_twist = cmd
        self._emit(self._twist_handlers, cmd)

    def _emit(self, handlers: list[Any], arg: Any) -> None:
        for h in list(handlers):
            try:
                result = h(arg)
            except Exception:
                log.exception("robot handler %r raised", h)
                continue
            if inspect.isawaitable(result):
                task = asyncio.ensure_future(result)
                self._tasks.add(task)
                task.add_done_callback(self._task_done)

    def _task_done(self, task: asyncio.Future[Any]) -> None:
        self._tasks.discard(task)  # type: ignore[arg-type]
        if not task.cancelled() and task.exception() is not None:
            log.error("robot handler raised", exc_info=task.exception())


def _stop(source: TwistSource, lease_id: str | None) -> TwistCommand:
    return TwistCommand(0.0, 0.0, 0.0, source=source, lease_id=lease_id)


def _add(handlers: list[Any], handler: Any) -> Callable[[], None]:
    handlers.append(handler)

    def remove() -> None:
        try:
            handlers.remove(handler)
        except ValueError:
            pass

    return remove
