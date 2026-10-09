"""Launch test: the node against a real fleet-server.

Covers the connection half of fleet_agent: enroll once and persist the token,
declare the manifest built from parameters, stay online on heartbeats, come back
as the same robot after the server restarts, and turn NavSatFix + BatteryState
into telemetry the console can draw.
"""

import asyncio
import json
import os
import stat
import sys
import unittest

import launch
import launch_testing.actions
import pytest
import rclpy
from sensor_msgs.msg import BatteryState, NavSatFix, NavSatStatus

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import fleet_harness as harness  # noqa: E402

NAME = "online-bot"
FIX_TOPIC = "/test/gps/fix"
BATTERY_TOPIC = "/test/battery_state"
MANIFEST = {"drive": {"type": "twist", "max_v_mps": 1.5, "max_w_radps": 2.0}, "battery": {}}


@pytest.mark.launch_test
def generate_test_description():
    server = harness.FleetServer.start()
    agent = harness.agent_node(
        server,
        NAME,
        {
            "drive.type": "twist",
            "drive.max_v_mps": 1.5,
            "drive.max_w_radps": 2.0,
            "telemetry.fix_topic": FIX_TOPIC,
            "telemetry.battery_topic": BATTERY_TOPIC,
            "telemetry.rate_hz": 10.0,
            "telemetry.pose_timeout_s": 1.0,
        },
    )
    return launch.LaunchDescription([agent, launch_testing.actions.ReadyToTest()]), {"server": server}


class TestOnline(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        rclpy.init()
        cls.node = rclpy.create_node("test_online")
        cls.fix_pub = cls.node.create_publisher(NavSatFix, FIX_TOPIC, 10)
        cls.battery_pub = cls.node.create_publisher(BatteryState, BATTERY_TOPIC, 10)

    @classmethod
    def tearDownClass(cls):
        cls.node.destroy_node()
        rclpy.shutdown()

    def test_enrolls_once_and_declares_manifest_from_params(self, server):
        async def scenario():
            async with harness.watching(server) as watcher:
                return await watcher.robot(NAME, lambda r: r.get("manifest") == MANIFEST)

        summary = asyncio.run(scenario())
        token_file = server.token_file(NAME)
        stored = json.loads(token_file.read_text())
        self.assertEqual(stored["client_id"], summary["robot_id"])
        self.assertEqual(stat.S_IMODE(token_file.stat().st_mode), 0o600)

    def test_fix_and_battery_become_telemetry(self, server):
        fix = NavSatFix()
        fix.status.status = NavSatStatus.STATUS_FIX
        fix.latitude, fix.longitude, fix.altitude = 38.6488, -90.3108, 160.5
        battery = BatteryState(percentage=0.87, voltage=12.5)

        def publish():
            self.fix_pub.publish(fix)
            self.battery_pub.publish(battery)

        async def scenario():
            async with harness.watching(server) as watcher:
                robot_id = (await watcher.robot(NAME))["robot_id"]
                feed = asyncio.ensure_future(harness.repeat(publish))
                try:
                    located = await watcher.telemetry(robot_id, lambda d: "pose" in d and "battery" in d)
                    # The receiver loses its fix: the pose must stop, the battery must not.
                    fix.status.status = NavSatStatus.STATUS_NO_FIX
                    lost = await watcher.telemetry(robot_id, lambda d: "pose" not in d)
                finally:
                    feed.cancel()
                return located, lost

        located, lost = asyncio.run(scenario())
        # Frame-relative pose (DESIGN.md D7): geographic, and no yaw since a fix has no heading.
        self.assertEqual(
            located["pose"], {"frame": "geographic", "lat": 38.6488, "lon": -90.3108, "alt_m": 160.5}
        )
        # BatteryState.percentage is a float32 fraction; the wire carries percent.
        self.assertAlmostEqual(located["battery"]["pct"], 87.0, places=3)
        self.assertAlmostEqual(located["battery"]["voltage"], 12.5, places=3)
        self.assertIn("battery", lost)

    def test_stays_online_and_reconnects_as_the_same_robot(self, server):
        async def online():
            async with harness.watching(server) as watcher:
                return await watcher.robot(NAME, lambda r: r.get("manifest") == MANIFEST)

        async def outlast_heartbeats():
            async with harness.watching(server) as watcher:
                before = await watcher.robot(NAME)
                # Without heartbeats the server marks a robot offline after ~2.5 intervals.
                await asyncio.sleep(8 * server.heartbeat_interval_ms / 1000)
                after = await watcher.robot(NAME, timeout=1.0)
                return before, after

        before, after = asyncio.run(outlast_heartbeats())
        self.assertEqual(before["robot_id"], after["robot_id"])

        # The server forgets live connections and manifests when it restarts; the
        # node must come back by itself, with its stored token and its manifest.
        server.down()
        server.up()
        again = asyncio.run(online())
        self.assertEqual(again["robot_id"], before["robot_id"])
