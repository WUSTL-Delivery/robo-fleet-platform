"""The fleet_agent node: one robot's connection to fleet-server, driven by parameters.

Two threads, with a narrow seam between them:

- The **ROS thread** (main) spins the node. Subscription callbacks convert each
  message (conversions.py) and store the newest value in ``_Latest``.
- The **link thread** runs an asyncio loop that owns ``fleet.Robot``. The SDK
  does everything on the wire there: enroll once, hello, heartbeat, reconnect
  with the same token, re-send the manifest on every connect. A task on that
  loop reads ``_Latest`` and sends one telemetry sample at a fixed rate.

The node holds no websocket code of its own. New bridges follow the same shape:
an SDK callback (``robot.on_twist``, ``robot.on_lease``, ``channel.on_message``)
fires on the link thread and publishes to a ROS topic; a ROS callback hands work
to the link with ``asyncio.run_coroutine_threadsafe(..., self._loop)``.

Teleop (only when ``drive.type`` is set): the SDK decides which twist is valid
(current lease only) and issues its own stops; ``_on_twist`` passes both to a
``TwistGate`` (gate.py), which clamps to the drive limits and publishes
``geometry_msgs/Twist`` on ``cmd_vel.topic``. The deadman exists twice on
purpose: the SDK's timer on the link thread, and the gate's watchdog on a ROS
steady-clock timer on the ROS thread. Either one stops the robot alone, so a
blocked or dead link thread cannot leave it moving, and neither needs the
server. The lease state is latched on ``fleet/lease`` (fleet_agent_msgs/Lease).
With ``data_channel: auto`` and aiortc installed the SDK also takes twist from
the operator's WebRTC data channel; it reaches ``_on_twist`` the same way.

Channels (the ``channels`` parameter) are the application node's way in and out
without a socket: what arrives on channel ``<name>`` is published on
``fleet/ch/<name>/in`` and what the application publishes on
``fleet/ch/<name>/out`` is sent, both as fleet_agent_msgs/ChannelMsg with the
data as JSON text. An acked send is answered here unless the channel is in
``raw_channels`` (``_on_acked``). ``fleet/request_help``
(fleet_agent_msgs/RequestHelp) raises the robot's hand. Nothing on the ROS
thread waits for the network: an out message is handed to the link thread and
forgotten, and the service callback is a coroutine, in a callback group of its
own, that the executor resumes when the link thread has answered.

Parameters (config/fleet_agent.example.yaml documents each):
``url``, ``name``, ``token_file``, ``enroll_key``, ``drive.type``,
``drive.max_v_mps``, ``drive.max_w_radps``, ``cmd_vel.topic``,
``cmd_vel.deadman_ms``, ``data_channel``, ``channels``, ``raw_channels``,
``telemetry.fix_topic``, ``telemetry.battery_topic``, ``telemetry.rate_hz``,
``telemetry.pose_timeout_s``.
"""

from __future__ import annotations

import asyncio
import concurrent.futures
import logging
import os
import signal
import socket
import threading
import time
from typing import Any, Optional

import rclpy
from geometry_msgs.msg import Twist
from rclpy.callback_groups import MutuallyExclusiveCallbackGroup
from rclpy.clock import Clock, ClockType
from rclpy.exceptions import InvalidParameterTypeException
from rclpy.executors import ExternalShutdownException, SingleThreadedExecutor
from rclpy.node import Node
from rclpy.parameter import Parameter
from rclpy.qos import DurabilityPolicy, QoSProfile, ReliabilityPolicy, qos_profile_sensor_data
from rclpy.signals import SignalHandlerOptions
from rclpy.task import Future
from sensor_msgs.msg import BatteryState, NavSatFix

from fleet import (
    ChannelTargetNotFound,
    ConnectionState,
    Envelope,
    FileTokenStore,
    FleetClientError,
    Robot,
    StateChange,
)
from fleet.robot import LeaseChange, TwistCommand
from fleet_agent_msgs.msg import ChannelMsg, Lease
from fleet_agent_msgs.srv import RequestHelp

from . import __version__
from .conversions import (
    battery_from_state,
    build_manifest,
    channel_names,
    decode_channel_data,
    encode_channel_data,
    help_context,
    pose_from_fix,
)
from .gate import TwistGate

#: Where the lease state is latched, relative to the node's namespace.
LEASE_TOPIC = "fleet/lease"

#: The service that raises the robot's hand, relative to the node's namespace.
HELP_SERVICE = "fleet/request_help"

#: Channel ``name`` is bridged to ``fleet/ch/<name>/in`` and ``fleet/ch/<name>/out``.
CHANNEL_TOPIC_PREFIX = "fleet/ch"

#: Reliable, keep last 10, on both channel topics: every message matters, a little.
CHANNEL_QOS_DEPTH = 10

#: Out messages handed to the link thread and not yet sent or failed. Past this the
#: application is publishing faster than the link takes them, and new ones are dropped.
MAX_PENDING_OUT = 256

#: How long the server's refusal of a publish is waited for, to log it with the
#: channel and the target (the link thread waits, never the ROS thread).
PUBLISH_ERROR_WINDOW_S = 0.5

#: Envelope ids of this node's channel publishes start with this, so the general
#: server-error log can leave their refusals to the publish that is waiting for them.
_OUT_ID_PREFIX = "fa-out-"

#: fleet/request_help answers "timeout" if the link thread has not taken the request
#: after this long (it is blocked or gone; an open link takes it in milliseconds).
HELP_TIMEOUT_S = 2.0

#: Latched: reliable, and the last message is kept for subscribers that join late.
LATCHED_QOS = QoSProfile(
    depth=1, reliability=ReliabilityPolicy.RELIABLE, durability=DurabilityPolicy.TRANSIENT_LOCAL
)

#: Why the SDK stopped the robot, for the log (``TwistCommand.source``).
_SDK_STOPS = {
    "deadman": "deadman: no valid twist",
    "revoked": "lease revoked",
    "disconnected": "link to fleet-server lost",
}

#: Read for the enrollment key when the ``enroll_key`` parameter is empty, so the
#: secret can stay out of parameter files (the same variable the Python SDK examples use).
ENROLL_KEY_ENV = "FLEET_ENROLL_KEY"


class _NotAccepted(Exception):
    """An acked message this node will not answer for: nobody is there to receive it."""


class _QuietNotAccepted(logging.Filter):
    """Keeps the SDK from logging a traceback for every ``_NotAccepted`` (the node logs one line itself)."""

    def filter(self, record: logging.LogRecord) -> bool:
        return not (record.exc_info and isinstance(record.exc_info[1], _NotAccepted))


class _Latest:
    """The newest converted sample from each telemetry source, shared across threads."""

    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._pose: Optional[dict[str, Any]] = None
        self._pose_at = 0.0
        self._battery: Optional[dict[str, Any]] = None

    def set_pose(self, pose: Optional[dict[str, Any]]) -> None:
        with self._lock:
            self._pose = pose
            self._pose_at = time.monotonic()

    def set_battery(self, battery: Optional[dict[str, Any]]) -> None:
        if battery is None:
            return  # a message with no reading does not erase the last one
        with self._lock:
            self._battery = battery

    def sample(self, pose_timeout_s: float) -> dict[str, Any]:
        """Keyword arguments for ``Robot.telemetry``; empty when there is nothing to send.

        The pose is left out once it is older than ``pose_timeout_s`` (0 = never),
        so a dead position source stops being reported as the robot's position.
        Battery level changes slowly and is reported until a newer reading arrives.
        """
        with self._lock:
            out: dict[str, Any] = {}
            if self._pose is not None:
                age = time.monotonic() - self._pose_at
                if pose_timeout_s <= 0 or age <= pose_timeout_s:
                    out["pose"] = self._pose
            if self._battery is not None:
                out["battery"] = self._battery
            return out


class FleetAgent(Node):
    """Connects this robot to fleet-server: telemetry out, operator twist in, lease state
    latched, channels as topics, help as a service."""

    def __init__(self, **node_args: Any) -> None:
        super().__init__("fleet_agent", **node_args)

        url = self.declare_parameter("url", "ws://localhost:8080/ws").value
        name = self.declare_parameter("name", "").value or socket.gethostname()
        token_file = self.declare_parameter("token_file", "").value or f"~/.fleet/{name}.json"
        enroll_key = self.declare_parameter("enroll_key", "").value or os.environ.get(ENROLL_KEY_ENV, "")

        drive_type = self.declare_parameter("drive.type", "").value
        max_v_mps = self.declare_parameter("drive.max_v_mps", 0.0).value
        max_w_radps = self.declare_parameter("drive.max_w_radps", 0.0).value
        cmd_vel_topic = self.declare_parameter("cmd_vel.topic", "cmd_vel").value
        deadman_ms = int(self.declare_parameter("cmd_vel.deadman_ms", 300).value)
        if not 100 <= deadman_ms <= 1000:
            raise ValueError(f"cmd_vel.deadman_ms must be between 100 and 1000, got {deadman_ms!r}")

        data_channel = self.declare_parameter("data_channel", "auto").value
        if data_channel not in ("auto", "off"):
            raise ValueError(f"data_channel must be 'auto' or 'off', got {data_channel!r}")
        # Declared by type: an empty list as the default would not say what it is a list of.
        channels, raw_channels = channel_names(
            self.declare_parameter("channels", Parameter.Type.STRING_ARRAY).value or [],
            self.declare_parameter("raw_channels", Parameter.Type.STRING_ARRAY).value or [],
        )

        fix_topic = self.declare_parameter("telemetry.fix_topic", "").value
        battery_topic = self.declare_parameter("telemetry.battery_topic", "").value
        rate_hz = self.declare_parameter("telemetry.rate_hz", 1.0).value
        self._pose_timeout_s = float(self.declare_parameter("telemetry.pose_timeout_s", 5.0).value)
        if not rate_hz > 0:
            raise ValueError(f"telemetry.rate_hz must be greater than 0, got {rate_hz!r}")
        self._period_s = 1.0 / float(rate_hz)

        manifest = build_manifest(
            drive_type=drive_type,
            max_v_mps=float(max_v_mps),
            max_w_radps=float(max_w_radps),
            battery=bool(battery_topic),
            channels=channels,
        )

        self._latest = _Latest()
        # Sensor-data QoS (best effort) receives from reliable and best-effort publishers alike.
        if fix_topic:
            self.create_subscription(NavSatFix, fix_topic, self._on_fix, qos_profile_sensor_data)
        if battery_topic:
            self.create_subscription(BatteryState, battery_topic, self._on_battery, qos_profile_sensor_data)
        if not fix_topic:
            self.get_logger().info("telemetry.fix_topic is empty: the robot will be online without a pose")

        self._robot = Robot(
            url,
            manifest=manifest,
            name=name,
            enrollment_key=enroll_key or None,  # only used while the token file is empty
            token_store=FileTokenStore(token_file),
            agent={"name": "fleet_agent", "version": __version__},
            # cmd_vel.deadman_ms is the deadline for zero to be ON the topic, so both
            # timers (this one and the gate's watchdog) fire a little before it.
            deadman_ms=deadman_ms - int(TwistGate.EARLY_S * 1000),
            # auto: on when aiortc is installed. No ice_servers on purpose: leaving
            # them unset lets the SDK decide where they come from.
            data_channel=None if data_channel == "auto" else False,
        )
        self._robot.client.on_state(self._on_state)
        self._robot.client.on("error", self._on_server_error)

        self._connected = False
        self._ended: Optional[LeaseChange] = None
        self._lease_pub = self.create_publisher(Lease, LEASE_TOPIC, LATCHED_QOS)
        self._robot.on_lease(self._on_lease)
        self._publish_lease()

        #: None for a robot that declares no drive: it has no cmd_vel publisher at all.
        self.gate: Optional[TwistGate] = None
        if "drive" in manifest:
            if not cmd_vel_topic:
                raise ValueError("cmd_vel.topic must not be empty when drive.type is set")
            # Depth 1: a setpoint is only worth delivering while it is the newest.
            self._cmd_vel_pub = self.create_publisher(Twist, cmd_vel_topic, 1)
            self.gate = TwistGate(
                self._publish_cmd_vel,
                max_v_mps=float(max_v_mps),
                max_w_radps=float(max_w_radps),
                deadman_s=deadman_ms / 1000,
                on_stop=lambda reason: self.get_logger().warning(f"cmd_vel zeroed: {reason}"),
            )
            self._robot.on_twist(self._on_twist)
            # Steady clock: the watchdog must keep time when sim time is paused, and
            # it runs on the ROS thread so it does not need the link thread alive.
            self._watchdog = self.create_timer(
                TwistGate.TICK_S, self.gate.tick, clock=Clock(clock_type=ClockType.STEADY_TIME)
            )
            if self._robot.data_channel_enabled:
                via = "the bus or a WebRTC data channel"
            elif data_channel == "off":
                via = "the bus only (data_channel is off)"
            else:
                via = "the bus only (aiortc is not installed)"
            self.get_logger().info(
                f"operator twist goes to '{self._cmd_vel_pub.topic_name}', limited to "
                f"{max_v_mps} m/s and {max_w_radps} rad/s, deadman {deadman_ms} ms; it arrives over {via}"
            )

        self._out_lock = threading.Lock()
        self._out_pending = 0
        self._out_seq = 0
        self._in_pubs: dict[str, Any] = {}
        for channel in channels:
            self._bridge_channel(channel, raw=channel in raw_channels)
        if channels:
            # The SDK logs a traceback when an acked handler raises; _on_acked raises
            # on purpose and logs one line of its own.
            logging.getLogger("fleet.channel").addFilter(_QuietNotAccepted())

        self._steady = Clock(clock_type=ClockType.STEADY_TIME)
        # A group of its own. The callback is a coroutine, and a callback group stays
        # taken until its coroutine finishes: in the node's default group a pending
        # help request would keep every timer from running, the cmd_vel watchdog first.
        self._help_srv = self.create_service(
            RequestHelp, HELP_SERVICE, self._on_request_help, callback_group=MutuallyExclusiveCallbackGroup()
        )

        self._loop = asyncio.new_event_loop()
        self._thread = threading.Thread(target=self._link_thread, name="fleet-link", daemon=True)
        self._finished = threading.Event()
        #: Set when the link ended for a reason retrying cannot fix (bad key, revoked token, ...).
        self.fatal: Optional[FleetClientError] = None
        #: Set when the link thread died of anything else (a bug); the process exits non-zero.
        self.crashed: Optional[BaseException] = None

        self.get_logger().info(f"connecting to {url} as '{name}' (token file {token_file}), manifest {manifest}")

    # ------------------------------------------------------------------ lifecycle

    def start(self) -> None:
        """Starts the link thread: connect, then stay connected."""
        self._thread.start()

    @property
    def finished(self) -> bool:
        """True once the link has ended, by ``stop()`` or by a fatal error."""
        return self._finished.is_set()

    def stop(self) -> None:
        """Zeroes cmd_vel, closes the connection and joins the link thread. Safe to call twice."""
        if self.gate is not None:
            try:
                # Before the link is closed, and final: a twist that is still on its way
                # in must not move the robot after this.
                self.gate.close("fleet_agent is shutting down")
            except Exception as e:  # the ROS context is already gone; nothing can be published
                self.get_logger().warning(f"could not publish the final stop: {e!r}")
        if self._thread.is_alive():
            try:
                asyncio.run_coroutine_threadsafe(self._robot.close(), self._loop).result(timeout=5.0)
            except Exception as e:  # shutting down anyway; say why the close was not clean
                self.get_logger().warning(f"closing the fleet connection: {e!r}")
            self._thread.join(timeout=5.0)

    # ------------------------------------------------------------------ ROS thread

    def _on_fix(self, msg: NavSatFix) -> None:
        self._latest.set_pose(pose_from_fix(msg))

    def _on_battery(self, msg: BatteryState) -> None:
        self._latest.set_battery(battery_from_state(msg))

    def _bridge_channel(self, name: str, *, raw: bool) -> None:
        """Creates the two topics of channel ``name`` and registers its SDK handlers."""
        topic = f"{CHANNEL_TOPIC_PREFIX}/{name}"
        pub = self._in_pubs[name] = self.create_publisher(ChannelMsg, f"{topic}/in", CHANNEL_QOS_DEPTH)
        sub = self.create_subscription(
            ChannelMsg, f"{topic}/out", lambda msg: self._on_channel_out(name, msg), CHANNEL_QOS_DEPTH
        )
        channel = self._robot.channel(name)
        channel.on_message(lambda sender, data: self._channel_in(name, sender, data, acked=False))
        if not raw:
            channel.on_acked(lambda sender, data: self._on_acked(name, sender, data))
        self.get_logger().info(
            f"channel '{name}': in on '{pub.topic_name}', out on '{sub.topic_name}', acked sends are "
            + ("passed through for the application to answer" if raw else "answered by fleet_agent")
        )

    def _on_channel_out(self, name: str, msg: ChannelMsg) -> None:
        log = self.get_logger()
        try:
            data = decode_channel_data(msg.data)
        except ValueError as e:
            log.warning(f"channel '{name}': dropped an out message whose data is not JSON ({e})")
            return
        # At-most-once, like the bus itself: nothing is queued for a link that is down.
        if not self._connected:
            log.warning(
                f"channel '{name}': dropped an out message, not connected to fleet-server",
                throttle_duration_sec=5.0,
            )
            return
        with self._out_lock:
            if self._out_pending >= MAX_PENDING_OUT:
                full = True
            else:
                full = False
                self._out_pending += 1
                self._out_seq += 1
                ref = f"{_OUT_ID_PREFIX}{self._out_seq}"
        if full:
            log.warning(
                f"channel '{name}': dropped an out message, {MAX_PENDING_OUT} are still waiting to be sent",
                throttle_duration_sec=5.0,
            )
            return
        if self._submit(self._publish_channel(name, data, msg.to or None, ref)) is None:
            with self._out_lock:
                self._out_pending -= 1

    async def _on_request_help(
        self, request: RequestHelp.Request, response: RequestHelp.Response
    ) -> RequestHelp.Response:
        # A coroutine in its own callback group: the executor keeps running everything
        # else (the cmd_vel watchdog included) while the link thread does the sending.
        # A second request waits for this one.
        def answer(code: str, message: str) -> RequestHelp.Response:
            response.accepted = not code
            response.code, response.message = code, message
            log = self.get_logger()
            if code:
                log.warning(f"help request '{request.reason[:64]}' not sent: {code}: {message}")
            else:
                log.info(f"help requested: {request.reason}")
            return response

        try:
            if not 1 <= len(request.reason) <= 256:
                raise ValueError("reason must be 1 to 256 characters")
            context = help_context(request.context)
        except ValueError as e:
            return answer(RequestHelp.Response.CODE_INVALID, str(e))
        if not self._connected:
            return answer(RequestHelp.Response.CODE_OFFLINE, "not connected to fleet-server")

        done = Future()

        def settle(outcome: Optional[tuple[str, str]]) -> None:  # either thread
            if not done.done():
                done.set_result(outcome)
                executor = self.executor
                if executor is not None:
                    executor.wake()  # resume this coroutine now, not at the next timer

        sending = self._submit(self._send_help(request.reason, context))
        if sending is None:
            return answer(RequestHelp.Response.CODE_OFFLINE, "the link to fleet-server has ended")
        sending.add_done_callback(
            lambda f: settle(None if f.cancelled() or f.exception() is not None else f.result())
        )
        # In the node's default group (not this service's, which is taken until we return).
        timeout = self.create_timer(HELP_TIMEOUT_S, lambda: settle(None), clock=self._steady)
        try:
            outcome = await done
        finally:
            self.destroy_timer(timeout)
        if outcome is None:
            sending.cancel()
            return answer(RequestHelp.Response.CODE_TIMEOUT, "the link thread did not take the request")
        return answer(*outcome)

    # ------------------------------------------------------------------ either thread

    def _submit(self, coro: Any) -> Optional[concurrent.futures.Future]:
        """Hands a coroutine to the link thread; None if that thread's loop is gone."""
        try:
            return asyncio.run_coroutine_threadsafe(coro, self._loop)
        except RuntimeError:
            coro.close()
            return None

    def _publish_cmd_vel(self, x: float, y: float, wz: float) -> None:
        msg = Twist()
        msg.linear.x, msg.linear.y, msg.angular.z = x, y, wz
        self._cmd_vel_pub.publish(msg)

    def _publish_lease(self) -> None:
        msg = Lease()
        msg.stamp = self.get_clock().now().to_msg()
        msg.mode = self._robot.mode
        msg.connected = self._connected
        held = self._robot.lease
        if held is not None:
            msg.held = True
            msg.lease_id = held.lease_id
            msg.operator_id = held.operator_id or ""
        elif self._ended is not None:
            msg.lease_id = self._ended.lease_id
            msg.reason = self._ended.reason or ""
        self._lease_pub.publish(msg)

    # ------------------------------------------------------------------ link thread

    def _on_twist(self, cmd: TwistCommand) -> None:
        # Called synchronously by the SDK the moment a command is decided. Only the
        # current lease's twist gets this far; the gate checks the lease id again.
        assert self.gate is not None
        if cmd.source == "operator":
            self.gate.operator_twist(cmd.lease_id, cmd.linear_x, cmd.linear_y, cmd.angular_z)
        else:
            self.gate.stop(_SDK_STOPS.get(cmd.source, cmd.source))

    def _channel_in(self, name: str, sender: str, data: Any, *, acked: bool) -> None:
        msg = ChannelMsg()
        msg.stamp = self.get_clock().now().to_msg()
        msg.sender = sender
        msg.data = encode_channel_data(data)
        msg.acked = acked
        self._in_pubs[name].publish(msg)

    def _on_acked(self, name: str, sender: str, data: Any) -> None:
        # Returning tells the sender "accepted" ({ack: seq}, sent by the SDK); raising
        # tells it nothing, and its next re-send is tried afresh. The SDK shows each
        # acked send here once, so the application never sees a repeat.
        pub = self._in_pubs[name]
        if pub.get_subscription_count() == 0:
            # Nobody would receive it. Acking would tell a dispatcher the job was
            # taken when no application is running.
            self.get_logger().warning(
                f"channel '{name}': not acking a message from {sender}, nothing subscribes to "
                f"'{pub.topic_name}'",
                throttle_duration_sec=5.0,
            )
            raise _NotAccepted(name)
        self._channel_in(name, sender, data, acked=True)

    async def _publish_channel(self, name: str, data: Any, to: Optional[str], ref: str) -> None:
        log = self.get_logger()
        try:
            await self._robot.channel(name).publish(data, to=to, id=ref, wait=PUBLISH_ERROR_WINDOW_S)
        except ChannelTargetNotFound:
            log.warning(f"channel '{name}': {to} is not connected, the message was dropped")
        except FleetClientError as e:
            log.warning(f"channel '{name}': the message was not sent: {e}")
        finally:
            with self._out_lock:
                self._out_pending -= 1

    async def _send_help(self, reason: str, context: Optional[dict[str, Any]]) -> tuple[str, str]:
        """Sends help.request; returns (code, message) for the service, code empty when sent."""
        try:
            await self._robot.request_help(reason, context)
        except ValueError as e:
            return RequestHelp.Response.CODE_INVALID, str(e)
        except FleetClientError as e:
            return RequestHelp.Response.CODE_OFFLINE, str(e)
        self._publish_lease()  # mode is now help, unless an operator is already driving
        return "", "help.request sent"

    def _on_server_error(self, env: Envelope) -> None:
        # Everything the server refuses, so that nothing fails silently: a rate
        # limit, an oversized payload, a refused token.
        p = env["payload"]
        ref = p.get("ref")
        if isinstance(ref, str) and ref.startswith(_OUT_ID_PREFIX):
            return  # _publish_channel is waiting for this one and logs it with the channel
        self.get_logger().warning(f"fleet-server answered {p.get('code')}: {p.get('message')}")

    def _on_lease(self, change: LeaseChange) -> None:
        # The SDK has already stopped the robot if this ends or replaces a lease.
        log = self.get_logger()
        if change.granted:
            log.info(f"lease {change.lease_id} granted to operator {change.operator_id}")
        else:
            self._ended = change
            # No reason: it ended while this robot was disconnected, and the welcome
            # after the reconnect only says that it is gone.
            log.info(f"lease {change.lease_id} ended: {change.reason or 'while disconnected (reason unknown)'}")
        if self.gate is not None:
            self.gate.set_lease(change.lease_id if change.granted else None)
        self._publish_lease()

    def _link_thread(self) -> None:
        asyncio.set_event_loop(self._loop)
        try:
            self._loop.run_until_complete(self._link())
        except Exception as e:
            self.crashed = e
            self.get_logger().fatal(f"the link thread died: {e!r}")
        finally:
            self._loop.close()
            self._finished.set()

    async def _link(self) -> None:
        telemetry = asyncio.ensure_future(self._telemetry_loop())
        try:
            await self._robot.run_forever()
        except FleetClientError as e:
            self.fatal = e
            self.get_logger().fatal(f"fleet-server refused this robot and retrying cannot fix it: {e}")
        finally:
            telemetry.cancel()

    async def _telemetry_loop(self) -> None:
        # Wall-clock rate on purpose (not the ROS clock): the server and console are
        # outside the robot's time domain, and sim time may be paused.
        while True:
            await asyncio.sleep(self._period_s)
            sample = self._latest.sample(self._pose_timeout_s)
            if sample:
                # Lossy by design: returns False and drops the sample while the link is down.
                await self._robot.telemetry(**sample)

    def _on_state(self, change: StateChange) -> None:
        log = self.get_logger()
        connected = change.state is ConnectionState.OPEN
        if connected != self._connected:
            self._connected = connected
            self._publish_lease()
        if change.state is ConnectionState.ENROLLING:
            log.info("no stored token: enrolling with the fleet enrollment key")
        elif change.state is ConnectionState.OPEN:
            log.info(f"online as {self._robot.robot_id}")
        elif change.state is ConnectionState.RECONNECTING:
            log.warning(f"not connected ({change.error}); retry {change.attempt} in {change.retry_in:.1f} s")


def main(args: Optional[list[str]] = None) -> int:
    # No rclpy signal handlers: they shut the context down before the node could
    # publish a final zero velocity. The handlers here only end the loop below.
    rclpy.init(args=args, signal_handler_options=SignalHandlerOptions.NO)
    interrupted = threading.Event()
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda signum, frame: interrupted.set())
    try:
        node = FleetAgent()
    except (ValueError, InvalidParameterTypeException) as e:
        # Includes an integer given for a double parameter (write 1.0, not 1).
        print(f"fleet_agent: bad parameters: {e}")
        rclpy.try_shutdown()
        return 2
    executor = SingleThreadedExecutor()
    executor.add_node(node)
    node.start()
    try:
        # A loop, not spin(): a fatal link error has to end the process too.
        while rclpy.ok() and not node.finished and not interrupted.is_set():
            executor.spin_once(timeout_sec=0.2)
    except (KeyboardInterrupt, ExternalShutdownException):
        pass
    finally:
        node.stop()
        executor.shutdown()
        node.destroy_node()
        rclpy.try_shutdown()
    return 1 if node.fatal is not None or node.crashed is not None else 0


if __name__ == "__main__":
    raise SystemExit(main())
