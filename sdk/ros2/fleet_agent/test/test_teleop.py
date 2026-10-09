"""Launch test: operator twist to cmd_vel, the lease topic, and every way teleop ends.

The node runs against a real fleet-server, reached through a TcpProxy so the test
can break the network. A real operator client claims the robot and drives it. The
test watches cmd_vel and fleet/lease the way the robot's base and application
node would, and times each stop from the last non-zero cmd_vel it received. Each
bound is extended by the time the host froze the test process, if it did
(Recorder.host_stall_ms); the printed line shows both numbers.

Covered here: twist stops arriving (deadman), the lease is handed back, the
network goes silent with no close (cable pulled), the socket is reset, and the
server is killed. A stalled or dead SDK thread is covered in test_watchdog.py.
"""

import asyncio
import contextlib
import os
import sys
import time
import unittest

import launch
import launch_testing.actions
import pytest
import rclpy

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import fleet_harness as harness  # noqa: E402
from fleet_harness import is_moving, is_zero  # noqa: E402

NAME = "teleop-bot"
CMD_VEL = "/test/teleop/cmd_vel"
MAX_V, MAX_W = 1.0, 1.5
DEADMAN_MS = 300
#: A stop faster than this after the last twist cannot be the deadman: it is the
#: revoke or the lost link acting on its own.
IMMEDIATE_S = 0.2


@pytest.mark.launch_test
def generate_test_description():
    server = harness.FleetServer.start()
    proxy = harness.TcpProxy(server.port)
    agent = harness.agent_node(
        server,
        NAME,
        {
            "drive.type": "twist",
            "drive.max_v_mps": MAX_V,
            "drive.max_w_radps": MAX_W,
            "cmd_vel.topic": CMD_VEL,
            "cmd_vel.deadman_ms": DEADMAN_MS,
        },
        url=proxy.ws_url,
    )
    context = {"server": server, "proxy": proxy}
    return launch.LaunchDescription([agent, launch_testing.actions.ReadyToTest()]), context


class TestTeleop(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        rclpy.init()
        cls.rec = harness.Recorder("test_teleop", CMD_VEL)

    @classmethod
    def tearDownClass(cls):
        cls.rec.close()
        rclpy.shutdown()

    def _teleop(self, server, body):
        """Runs ``body(op, lease, start)`` with an operator holding the lease on the robot."""

        async def scenario():
            async with harness.watching(server) as watcher, harness.operating(server) as op:
                robot = await watcher.robot(NAME, lambda r: "drive" in (r.get("manifest") or {}))
                await self.rec.lease(lambda m: m.connected)
                start = time.monotonic()
                lease = await op.claim(robot["robot_id"])
                await self.rec.lease(lambda m: m.held and m.lease_id == lease, since=start)
                try:
                    return await body(op, lease, start)
                finally:
                    with contextlib.suppress(Exception):
                        await op.release(lease)
                        await self.rec.lease(lambda m: not m.held, since=start, timeout=2)

        return asyncio.run(scenario())

    def test_1_twist_reaches_cmd_vel_clamped_and_the_lease_is_latched(self, server, proxy):
        rec = self.rec
        self.assertFalse(asyncio.run(rec.latched_lease()).held)

        async def body(op, lease, start):
            held = await rec.latched_lease()
            self.assertTrue(held.held and held.connected)
            self.assertEqual((held.lease_id, held.mode, held.reason), (lease, "teleop", ""))
            self.assertTrue(held.operator_id.startswith("o_"), held.operator_id)

            await op.twist(lease, 0.4, 0.2)
            _, inside = await rec.cmd(is_moving, since=start)
            self.assertEqual(inside, pytest.approx((0.4, 0.0, 0.2)))

            mark = time.monotonic()
            await op.twist(lease, 5.0, -9.0)
            _, clamped = await rec.cmd(is_moving, since=mark)
            self.assertEqual(clamped, (MAX_V, 0.0, -MAX_W))

            # Handback while moving: zero at once, not 300 ms later.
            released = time.monotonic()
            await op.release(lease)
            stopped, _ = await rec.cmd(is_zero, since=released)
            self.assertLess(stopped - released, IMMEDIATE_S + rec.host_stall_ms(released) / 1000)
            ended = await rec.lease(lambda m: not m.held, since=released)
            self.assertEqual((ended.lease_id, ended.reason, ended.mode), (lease, "released", "autonomous"))
            self.assertFalse((await rec.latched_lease()).held)

            # The lease is over: its twist must not move the robot (the server refuses
            # it; the SDK and the gate would too).
            after = time.monotonic()
            await op.twist(lease, 0.5)
            await asyncio.sleep(0.5)
            self.assertEqual(rec.moving_count(after), 0)

        self._teleop(server, body)

    def test_2_deadman_zeroes_within_300ms_of_the_last_twist(self, server, proxy):
        rec = self.rec

        async def body(op, lease, start):
            sent = []
            for _ in range(20):  # 20 Hz for a second: the deadman must not fire mid-stream
                sent.append(f"{(time.monotonic() - start) * 1000:.0f}")
                await op.twist(lease, 0.5, 0.3)
                await asyncio.sleep(0.05)
            await rec.cmd(is_zero, since=start)
            seen = f"twist sent at {sent} ms; cmd_vel {rec.timeline(start)}"
            self.assertEqual(rec.moving_count(start), 20, seen)
            gap_ms, stall_ms = rec.stop_gap_ms(start), rec.host_stall_ms(start)
            print(f"deadman: zero {gap_ms:.0f} ms after the last non-zero cmd_vel, host stall {stall_ms:.0f} ms")
            self.assertLessEqual(gap_ms, DEADMAN_MS + stall_ms, seen)
            self.assertGreater(gap_ms, DEADMAN_MS - 100 - stall_ms, seen)

            # The lease is still held: the robot is only stopped until the next twist.
            self.assertTrue((await rec.latched_lease()).held)
            await asyncio.sleep(0.5)
            again = time.monotonic()
            await op.twist(lease, 0.3)
            _, v = await rec.cmd(is_moving, since=again)
            self.assertEqual(v, pytest.approx((0.3, 0.0, 0.0)))

        self._teleop(server, body)

    def test_3_network_blackhole_zeroes_within_300ms(self, server, proxy):
        rec = self.rec

        async def body(op, lease, start):
            driving = asyncio.ensure_future(op.drive(lease, 0.5))
            try:
                await rec.cmd(is_moving, since=start)
                await asyncio.sleep(0.5)
                # The cable is pulled: no close, no reset, the operator keeps sending.
                gone = time.monotonic()
                proxy.blackhole()
                await asyncio.sleep(1.0)
                # Stopped, and still stopped a second later with the operator sending.
                last_moving, stopped = rec.final_stop(gone - 0.3)
                gap_ms, stall_ms = (stopped - last_moving) * 1000, rec.host_stall_ms(gone - 0.3)
                print(f"blackhole: zero {gap_ms:.0f} ms after the last non-zero cmd_vel, host stall {stall_ms:.0f} ms")
                self.assertLessEqual(gap_ms, DEADMAN_MS + stall_ms, rec.timeline(gone - 0.3))
                self.assertLessEqual((stopped - gone) * 1000, DEADMAN_MS + stall_ms, rec.timeline(gone - 0.3))
            finally:
                driving.cancel()
                proxy.cut()
                proxy.restore()
            await rec.lease(lambda m: m.connected, since=time.monotonic())

        self._teleop(server, body)

    def test_4_socket_reset_zeroes_at_once_and_twist_resumes(self, server, proxy):
        rec = self.rec

        async def body(op, lease, start):
            driving = asyncio.ensure_future(op.drive(lease, 0.5))
            try:
                await rec.cmd(is_moving, since=start)
                await asyncio.sleep(0.3)
                cut = time.monotonic()
                proxy.cut()
                stopped, _ = await rec.cmd(is_zero, since=cut)
                stall_ms = rec.host_stall_ms(cut - 0.1)
                after_ms = (stopped - cut) * 1000
                print(f"socket reset: zero {after_ms:.0f} ms after the reset, host stall {stall_ms:.0f} ms")
                self.assertLess(stopped - cut, IMMEDIATE_S + stall_ms / 1000, rec.timeline(cut - 0.3))
                down = await rec.lease(lambda m: not m.connected, since=cut)
                self.assertTrue(down.held, "a link blip must not drop the lease")
                # The node reconnects by itself and the same lease drives again.
                await rec.lease(lambda m: m.connected and m.held, since=cut)
                await rec.cmd(is_moving, since=stopped)
            finally:
                driving.cancel()

        self._teleop(server, body)

    def test_5_server_killed_zeroes_within_300ms(self, server, proxy):
        rec = self.rec

        async def body(op, lease, start):
            driving = asyncio.ensure_future(op.drive(lease, 0.5))
            try:
                await rec.cmd(is_moving, since=start)
                await asyncio.sleep(0.3)
                killed = time.monotonic()
                server.kill()
                await rec.lease(lambda m: not m.connected, since=killed)
                await asyncio.sleep(1.0)
                # Stopped, and still stopped a second later.
                last_moving, stopped = rec.final_stop(killed - 0.3)
                gap_ms, stall_ms = (stopped - last_moving) * 1000, rec.host_stall_ms(killed - 0.3)
                after_ms = (stopped - killed) * 1000
                print(f"server killed: zero {after_ms:.0f} ms after the kill, host stall {stall_ms:.0f} ms")
                self.assertLessEqual(gap_ms, DEADMAN_MS + stall_ms, rec.timeline(killed - 0.3))
                self.assertLessEqual((stopped - killed) * 1000, DEADMAN_MS + stall_ms, rec.timeline(killed - 0.3))
            finally:
                driving.cancel()

        self._teleop(server, body)
