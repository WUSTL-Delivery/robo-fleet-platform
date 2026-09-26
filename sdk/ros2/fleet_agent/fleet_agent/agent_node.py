"""``fleet_agent``: the ROS 2 side of a robot's connection to fleet-server.

What it bridges, in the four directions the protocol has:

==========================  ==================================================
Platform                    ROS
==========================  ==================================================
capability manifest         parameters (drive limits, cameras, channels)
``twist`` under a lease     a ``twist_mux`` input topic (default ``cmd_vel_fleet``)
``telemetry``               ``imu/data`` today; see "Not wired yet" below
``help.request``            ``~/request_help`` (Trigger service, or String topic)
``lease.granted/revoked``   ``~/mode`` (latched)
==========================  ==================================================

**The agent never becomes the robot's only brake.** The SDK zeroes velocity 300 ms after
the last valid setpoint, immediately on a revoke, and immediately on a lost link, without
waiting for the server (DESIGN.md D3). Local obstacle avoidance stays up throughout: this
is safeguarded velocity control, not raw actuation.

**Why it publishes onto a mux input rather than ``cmd_vel``.** The robot already
arbitrates between navigation, a tracker and the local joystick in ``twist_mux``. Adding
a fleet input keeps that arbitration in one place and keeps the physically-present
operator's joystick above the remote one. It also means handback is just going quiet: the
mux's own timeout drops the fleet input and navigation resumes.

That interaction needs one deliberate choice. The mux drops a stale input after its
``timeout`` (0.5 s on the club robot) and falls through to the *next* priority, so simply
going silent after a stop would hand a moving robot back to whatever nav goal was live.
So while a lease is held -- and for ``stop_hold_s`` after it ends -- the agent keeps
republishing the last command at ``publish_rate_hz``, zero included. Silence is reserved
for "autonomy may have the wheel back".

**Not wired yet.** delivery-robo has no ``Odometry`` publisher, no ``NavSatFix`` (the
``ublox_dgnss`` submodule is not checked out), no ``BatteryState`` and no Nav2 action, so
this node subscribes to none of them. The four methods under "Not wired yet" are the
places they land, each with its conversion already chosen; wiring one is adding its
``create_subscription`` and filling the body, not making a decision. Until then telemetry
carries health only, and the console will list the robot without placing it on the map.
"""

from __future__ import annotations

import asyncio
import os
import socket
import threading
import time
from typing import Any

import rclpy
from geometry_msgs.msg import Twist, TwistStamped
from rclpy.node import Node
from rclpy.qos import DurabilityPolicy, QoSProfile, ReliabilityPolicy, qos_profile_sensor_data
from sensor_msgs.msg import Imu
from std_msgs.msg import String
from std_srvs.srv import Trigger

from fleet import FileTokenStore, FleetClientError, LeaseChange, Robot, TwistCommand

from .conversions import clamp, yaw_from_quaternion
from .loop import AsyncioThread

__all__ = ["FleetAgent", "main"]

#: Latched, depth 1: a late subscriber (a club node starting after us) still learns the
#: current mode instead of waiting for the next change.
_LATCHED = QoSProfile(
    depth=1,
    reliability=ReliabilityPolicy.RELIABLE,
    durability=DurabilityPolicy.TRANSIENT_LOCAL,
)


class FleetAgent(Node):
    """One robot's fleet-server connection, wired to its ROS graph."""

    def __init__(self) -> None:
        """Declare parameters, build the ROS interfaces, and construct the SDK robot."""
        super().__init__("fleet_agent")

        # ---------------------------------------------------------------- parameters
        # Env fallbacks so a systemd unit can hold the secret and the launch file can
        # stay in git: the enrollment key is used exactly once, on first run.
        #
        # The fallback is applied to the *value*, not to the declared default, because a
        # params file that sets `name: ""` (as the packaged one does, meaning "use the
        # hostname") would otherwise override the default and win with an empty string.
        self._url = (
            self._param("url", "") or os.environ.get("FLEET_URL") or "ws://localhost:8080/ws"
        )
        self._name = (
            self._param("name", "") or os.environ.get("FLEET_NAME") or socket.gethostname()
        )
        enroll_key = self._param("enrollment_key", "") or os.environ.get("FLEET_ENROLL_KEY", "")
        token_file = self._param("token_file", "/var/lib/fleet_agent/token.json")

        self._max_v = float(self._param("max_v_mps", 1.0))
        self._max_w = float(self._param("max_w_radps", 1.5))
        # Comma-separated rather than string arrays: rclpy cannot infer the element type
        # of an empty list, so `declare_parameter("cameras", [])` raises outright, and
        # every workaround (an uninitialised typed parameter, a sentinel element) reads
        # worse than a split. Cameras take an optional label: "front:RealSense RGB,rear".
        cameras = _split_csv(self._param("cameras", ""))
        channels = _split_csv(self._param("channels", ""))
        behaviors = _split_csv(self._param("behaviors", ""))
        declare_battery = bool(self._param("declare_battery", False))

        cmd_vel_topic = self._param("cmd_vel_topic", "cmd_vel_fleet")
        self._stamped = bool(self._param("cmd_vel_stamped", True))
        self._twist_frame = self._param("cmd_vel_frame_id", "base_link")
        self._publish_rate = max(1.0, float(self._param("publish_rate_hz", 20.0)))
        self._stop_hold_s = max(0.0, float(self._param("stop_hold_s", 1.0)))

        self._telemetry_rate = max(0.1, float(self._param("telemetry_rate_hz", 1.0)))
        self._pose_frame_id = self._param("pose_frame_id", "map")
        imu_topic = self._param("imu_topic", "imu/data")

        # ---------------------------------------------------------------- state
        # _lock guards everything the two threads share; _pub_lock serialises the one
        # publisher that both of them write to.
        self._lock = threading.Lock()
        self._pub_lock = threading.Lock()
        self._last_cmd: tuple[float, float] = (0.0, 0.0)
        self._lease_active = False
        self._hold_until = 0.0
        self._pending: list[Any] = []  # callables to run on the ROS thread
        self._fatal: BaseException | None = None
        self._sensors: dict[str, Any] = {}

        # ---------------------------------------------------------------- ROS interfaces
        twist_type = TwistStamped if self._stamped else Twist
        self._twist_pub = self.create_publisher(twist_type, cmd_vel_topic, 10)
        self._mode_pub = self.create_publisher(String, "~/mode", _LATCHED)

        # Best-effort subscribers match both best-effort and reliable publishers, so the
        # agent does not need to know each driver's QoS to receive from it.
        self.create_subscription(Imu, imu_topic, self._on_imu, qos_profile_sensor_data)

        self.create_service(Trigger, "~/request_help", self._on_help_service)
        self.create_subscription(String, "~/request_help", self._on_help_topic, 10)

        # ---------------------------------------------------------------- SDK robot
        self._robot = Robot(
            self._url,
            manifest=_manifest(
                self._max_v, self._max_w, cameras, channels, behaviors, declare_battery,
            ),
            name=self._name,
            enrollment_key=enroll_key or None,
            token_store=FileTokenStore(token_file),
            agent={"kind": "fleet_agent", "version": "0.0.1"},
        )
        self._robot.on_twist(self._on_twist)
        self._robot.on_lease(self._on_lease)

        self._loop = AsyncioThread()
        self._tick_timer = self.create_timer(1.0 / self._publish_rate, self._tick)
        self._publish_mode("autonomous")
        self.get_logger().info(
            f"fleet_agent: {self._name} -> {self._url}, twist on "
            f"{cmd_vel_topic} ({'TwistStamped' if self._stamped else 'Twist'})"
        )

    def _param(self, name: str, default: Any) -> Any:
        """Declare a parameter with ``default`` and return its value."""
        return self.declare_parameter(name, default).value

    # ------------------------------------------------------------------ lifecycle

    def start(self) -> None:
        """Start the asyncio loop and connect. Returns as soon as the loop is running."""
        self._loop.start()
        self._loop.submit(self._run())

    async def _run(self) -> None:
        """Connect, keep telemetry flowing, and stay connected until told otherwise."""
        try:
            await self._robot.connect()
            self.get_logger().info(f"online as {self._robot.robot_id}")
            beat = asyncio.ensure_future(self._telemetry_forever())
            try:
                await self._robot.run_forever()
            finally:
                beat.cancel()
        except asyncio.CancelledError:
            raise
        except BaseException as exc:  # noqa: BLE001 - surfaced to the ROS thread below
            # auth_failed (revoked token, wrong key) and conflict (two processes on one
            # token) are terminal by design: retrying cannot fix either, so the node
            # exits loudly instead of reconnecting forever against a closed door.
            self.get_logger().error(f"fleet link failed: {exc}")
            with self._lock:
                self._fatal = exc

    async def _telemetry_forever(self) -> None:
        """Send one telemetry sample per period, for as long as the link lasts."""
        period = 1.0 / self._telemetry_rate
        while True:
            try:
                payload = self._telemetry_payload()
                if payload:
                    await self._robot.telemetry(**payload)
            except FleetClientError:
                pass  # telemetry is lossy by design; the link will come back or it will not
            except asyncio.CancelledError:
                raise
            except Exception as exc:  # noqa: BLE001 - one bad sample must not end the loop
                self.get_logger().warning(f"telemetry sample dropped: {exc}")
            await asyncio.sleep(period)

    def shutdown(self) -> None:
        """Stop the base, close the link, and stop the loop. Safe to call twice."""
        self._publish_twist(0.0, 0.0)
        try:
            self._loop.submit(self._robot.close()).result(timeout=3.0)
        except Exception as exc:  # noqa: BLE001 - shutdown is best effort
            self.get_logger().warning(f"close: {exc}")
        self._loop.stop()

    # ------------------------------------------------------------------ SDK -> ROS

    def _on_twist(self, cmd: TwistCommand) -> None:
        """Publish an operator setpoint (or an SDK stop) onto the mux input.

        Runs on the asyncio thread. It publishes immediately rather than leaving the
        command for the next tick: at 20 Hz that would add up to 50 ms to an emergency
        stop, and the stop path is exactly where latency is not acceptable.
        """
        vx = clamp(cmd.linear_x, self._max_v)
        wz = clamp(cmd.angular_z, self._max_w)
        with self._lock:
            self._last_cmd = (vx, wz)
            if cmd.is_stop and cmd.source != "operator":
                self._hold_until = time.monotonic() + self._stop_hold_s
        self._publish_twist(vx, wz)
        if cmd.source != "operator":
            self.get_logger().warning(f"stop: {cmd.source}")

    def _on_lease(self, change: LeaseChange) -> None:
        """Track who has the wheel; runs on the asyncio thread."""
        with self._lock:
            self._lease_active = change.granted
            self._last_cmd = (0.0, 0.0)
            if not change.granted:
                self._hold_until = time.monotonic() + self._stop_hold_s
        # Zero on both edges: on grant so the mux slot opens at rest rather than
        # inheriting a stale command, on release so the base is stopped before autonomy
        # is offered the wheel back.
        self._publish_twist(0.0, 0.0)
        if change.granted:
            self._queue(lambda: self._publish_mode("teleop"))
            self._queue(self._cancel_nav_goals)
            self.get_logger().info(f"teleop: operator {change.operator_id}")
        else:
            mode = "autonomous" if change.reason == "released" else "help"
            self._queue(lambda: self._publish_mode(mode))
            self.get_logger().info(f"lease ended ({change.reason}) -> {mode}")

    # ------------------------------------------------------------------ ROS -> SDK

    def _on_help_service(self, request: Any, response: Any) -> Any:
        """``~/request_help`` (Trigger): raise the robot's hand with a default reason."""
        del request
        ok, message = self._request_help("autonomy_escalation")
        response.success = ok
        response.message = message
        return response

    def _on_help_topic(self, msg: String) -> None:
        """``~/request_help`` (String): raise the robot's hand with ``msg.data`` as reason."""
        self._request_help(msg.data or "autonomy_escalation")

    def _request_help(self, reason: str) -> tuple[bool, str]:
        """Send ``help.request``, blocking briefly for the result. Runs on the ROS thread."""
        try:
            self._loop.submit(self._robot.request_help(reason[:256])).result(timeout=2.0)
        except Exception as exc:  # noqa: BLE001 - reported to the caller, never raised at ROS
            self.get_logger().error(f"help.request failed: {exc}")
            return False, str(exc)
        self.get_logger().info(f"hand raised: {reason}")
        self._publish_mode("help")
        return True, "help requested"

    # ------------------------------------------------------------------ ROS callbacks

    def _on_imu(self, msg: Imu) -> None:
        """Cache IMU yaw. The one telemetry input delivery-robo publishes today."""
        q = msg.orientation
        with self._lock:
            self._sensors["imu_yaw"] = yaw_from_quaternion(q.x, q.y, q.z, q.w)
            self._sensors["imu_at"] = time.monotonic()

    # ------------------------------------------------------------------ not wired yet

    def _on_odom(self, msg: Any) -> None:
        """Cache pose and body velocity from ``nav_msgs/Odometry``.

        TODO(ROB-32, ROB-41): no Odometry publisher exists on delivery-robo; the wheel
        encoder publishes a raw ``std_msgs/String`` on ``wheel_encoder/data`` and nothing
        fuses it yet. To wire: add ``nav_msgs`` to package.xml, subscribe on ``odom``,
        and cache ``odom_x``/``odom_y``/``odom_z`` from ``msg.pose.pose.position``,
        ``odom_yaw`` via ``yaw_from_quaternion``, ``odom_frame`` from the header, and
        ``v_mps``/``w_radps`` from ``msg.twist.twist`` -- every value through
        ``conversions.finite`` first. Then emit a ``local_pose`` in
        :meth:`_telemetry_payload`.
        """
        raise NotImplementedError("odom is not published on this robot yet")

    def _on_fix(self, msg: Any) -> None:
        """Cache the GNSS fix from ``sensor_msgs/NavSatFix``.

        TODO: the ``ublox_dgnss`` submodule is not checked out and nothing publishes
        ``fix``. To wire: add ``sensor_msgs`` NavSatFix, cache ``lat``/``lon``/``alt_m``
        through ``conversions.finite`` and the fix quality through
        ``conversions.navsat_fix_quality`` (RTK arrives as ``gbas``), then emit a
        ``geo_pose`` in :meth:`_telemetry_payload`. Prefer this over odometry once both
        exist: the campus graph is geographic.
        """
        raise NotImplementedError("fix is not published on this robot yet")

    def _on_battery(self, msg: Any) -> None:
        """Cache battery percentage and voltage from ``sensor_msgs/BatteryState``.

        TODO(ROB-24): the battery indicator is electrical-subteam work that has not
        landed. To wire: cache ``conversions.battery_pct(msg.percentage, scale)`` and
        ``finite(msg.voltage)``, emit them as ``telemetry.battery``, and flip the
        ``declare_battery`` parameter default to True so the console renders the widget.
        """
        raise NotImplementedError("battery_state is not published on this robot yet")

    def _cancel_nav_goals(self) -> None:
        """Cancel every goal on the nav action, so autonomy lets go the moment teleop starts.

        TODO(ROB-41): Nav2 is not in ``master_launch.py`` yet, so there is no action to
        cancel. To wire: add ``action_msgs`` to package.xml, create a client for
        ``<nav_action>/_action/cancel_goal``, and send an empty ``CancelGoal.Request()``
        -- an all-zero goal id with a zero stamp is the action spec's "cancel all goals",
        which avoids tracking goal handles the agent never issued.

        Best effort by design: the mux already ranks the fleet input above navigation, so
        a nav goal surviving takeover is a nuisance, not a safety problem.
        """
        self.get_logger().debug("nav goal cancel: no nav action wired yet")

    # ------------------------------------------------------------------ periodic

    def _tick(self) -> None:
        """Hold the mux slot, run queued work, and notice a terminal link failure."""
        with self._lock:
            fatal = self._fatal
            active = self._lease_active
            holding = time.monotonic() < self._hold_until
            vx, wz = self._last_cmd
            queued, self._pending = self._pending, []

        for fn in queued:
            try:
                fn()
            except Exception as exc:  # noqa: BLE001 - one bad callback must not stop the tick
                self.get_logger().error(f"queued call failed: {exc}")

        if active or holding:
            self._publish_twist(vx, wz)

        if fatal is not None:
            self.get_logger().fatal(f"shutting down: {fatal}")
            self._tick_timer.cancel()
            rclpy.try_shutdown()

    # ------------------------------------------------------------------ helpers

    def _publish_twist(self, vx: float, wz: float) -> None:
        """Publish one setpoint. Called from both threads, so it takes the publish lock."""
        if self._stamped:
            msg: Any = TwistStamped()
            msg.header.stamp = self.get_clock().now().to_msg()
            msg.header.frame_id = self._twist_frame
            body = msg.twist
        else:
            msg = Twist()
            body = msg
        body.linear.x = float(vx)
        body.angular.z = float(wz)
        with self._pub_lock:
            self._twist_pub.publish(msg)

    def _publish_mode(self, mode: str) -> None:
        """Publish the robot's intervention mode on the latched ``~/mode`` topic."""
        self._mode_pub.publish(String(data=mode))

    def _queue(self, fn: Any) -> None:
        """Hand a callable to the ROS thread, to be run by the next tick."""
        with self._lock:
            self._pending.append(fn)

    def _telemetry_payload(self) -> dict[str, Any]:
        """Build one telemetry sample from the cached sensors; runs on the asyncio thread.

        Pose, battery and velocity join this the moment their inputs exist -- see the
        "not wired yet" methods. health is a free-form diagnostic map, never platform
        vocabulary (D4): nothing on the server or in the console branches on it.
        """
        with self._lock:
            sensors = dict(self._sensors)
            teleop = self._lease_active

        health: dict[str, Any] = {"teleop": teleop}
        if sensors.get("imu_yaw") is not None:
            health["imu_yaw_rad"] = sensors["imu_yaw"]
        return {"health": health}


def _split_csv(value: str) -> list[str]:
    """Split a comma-separated parameter into a list, dropping blanks."""
    return [item.strip() for item in str(value or "").split(",") if item.strip()]


def _camera(entry: str) -> dict[str, str]:
    """Parse one ``id[:label]`` camera entry into a manifest camera."""
    cam_id, _, label = entry.partition(":")
    camera = {"id": cam_id.strip()}
    if label.strip():
        camera["label"] = label.strip()
    return camera


def _manifest(
    max_v: float,
    max_w: float,
    cameras: list[str],
    channels: list[str],
    behaviors: list[str],
    declare_battery: bool,
) -> dict[str, Any]:
    """Assemble the capability manifest the console renders this robot from.

    Declaring is the whole contract: a capability that is not in here gets no UI, and a
    ``drive`` block is what makes the SDK obey twist at all. ``declare_battery`` defaults
    off because nothing publishes a battery yet -- declaring one would render a widget
    with no reading behind it.
    """
    manifest: dict[str, Any] = {
        "drive": {"type": "twist", "max_v_mps": max_v, "max_w_radps": max_w},
    }
    if declare_battery:
        manifest["battery"] = {}
    if cameras:
        manifest["cameras"] = [_camera(entry) for entry in cameras]
    if channels:
        manifest["channels"] = channels
    if behaviors:
        manifest["behaviors"] = behaviors
    return manifest


def main(args: list[str] | None = None) -> None:
    """Spin the agent until shutdown."""
    rclpy.init(args=args)
    node = FleetAgent()
    try:
        node.start()
        rclpy.spin(node)
    except KeyboardInterrupt:
        pass
    finally:
        node.shutdown()
        node.destroy_node()
        rclpy.try_shutdown()


if __name__ == "__main__":
    main()
