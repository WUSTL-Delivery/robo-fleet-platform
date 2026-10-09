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

Parameters (config/fleet_agent.example.yaml documents each):
``url``, ``name``, ``token_file``, ``enroll_key``, ``drive.type``,
``drive.max_v_mps``, ``drive.max_w_radps``, ``telemetry.fix_topic``,
``telemetry.battery_topic``, ``telemetry.rate_hz``, ``telemetry.pose_timeout_s``.
"""

from __future__ import annotations

import asyncio
import os
import socket
import threading
import time
from typing import Any, Optional

import rclpy
from rclpy.exceptions import InvalidParameterTypeException
from rclpy.executors import ExternalShutdownException, SingleThreadedExecutor
from rclpy.node import Node
from rclpy.qos import qos_profile_sensor_data
from sensor_msgs.msg import BatteryState, NavSatFix

from fleet import ConnectionState, FileTokenStore, FleetClientError, Robot, StateChange

from . import __version__
from .conversions import battery_from_state, build_manifest, pose_from_fix

#: Read for the enrollment key when the ``enroll_key`` parameter is empty, so the
#: secret can stay out of parameter files (the same variable the Python SDK examples use).
ENROLL_KEY_ENV = "FLEET_ENROLL_KEY"


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
    """Connects this robot to fleet-server and reports its telemetry."""

    def __init__(self) -> None:
        super().__init__("fleet_agent")

        url = self.declare_parameter("url", "ws://localhost:8080/ws").value
        name = self.declare_parameter("name", "").value or socket.gethostname()
        token_file = self.declare_parameter("token_file", "").value or f"~/.fleet/{name}.json"
        enroll_key = self.declare_parameter("enroll_key", "").value or os.environ.get(ENROLL_KEY_ENV, "")

        drive_type = self.declare_parameter("drive.type", "").value
        max_v_mps = self.declare_parameter("drive.max_v_mps", 0.0).value
        max_w_radps = self.declare_parameter("drive.max_w_radps", 0.0).value

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
        )
        self._robot.client.on_state(self._on_state)

        self._loop = asyncio.new_event_loop()
        self._thread = threading.Thread(target=self._link_thread, name="fleet-link", daemon=True)
        self._finished = threading.Event()
        #: Set when the link ended for a reason retrying cannot fix (bad key, revoked token, ...).
        self.fatal: Optional[FleetClientError] = None

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
        """Closes the connection and joins the link thread. Safe to call twice."""
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

    # ------------------------------------------------------------------ link thread

    def _link_thread(self) -> None:
        asyncio.set_event_loop(self._loop)
        try:
            self._loop.run_until_complete(self._link())
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
        if change.state is ConnectionState.ENROLLING:
            log.info("no stored token: enrolling with the fleet enrollment key")
        elif change.state is ConnectionState.OPEN:
            log.info(f"online as {self._robot.robot_id}")
        elif change.state is ConnectionState.RECONNECTING:
            log.warning(f"not connected ({change.error}); retry {change.attempt} in {change.retry_in:.1f} s")


def main(args: Optional[list[str]] = None) -> int:
    rclpy.init(args=args)
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
        while rclpy.ok() and not node.finished:
            executor.spin_once(timeout_sec=0.2)
    except (KeyboardInterrupt, ExternalShutdownException):
        pass
    finally:
        node.stop()
        executor.shutdown()
        node.destroy_node()
        rclpy.try_shutdown()
    return 1 if node.fatal is not None else 0


if __name__ == "__main__":
    raise SystemExit(main())
