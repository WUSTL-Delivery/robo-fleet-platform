"""The deadman when a thread of the node itself fails, not the network.

fleet_agent has two threads and two deadman timers (node.py). These tests run
the node inside the test process, against a real fleet-server and a real
operator, and take one thread away while the robot is being driven:

- the SDK's asyncio loop is blocked: the ROS watchdog must stop the robot;
- the link thread dies outright: the ROS watchdog must stop the robot;
- the ROS executor stops spinning: the SDK's deadman must stop the robot.

Also here, because it needs the same rig: a help request made while the link
thread is stuck must not hold up the ROS executor (and with it the watchdog).

The operator keeps sending twist throughout, so nothing but the surviving timer
can be what publishes zero.

The last test runs the node as its own process and sends it SIGTERM mid-drive:
it must publish a final zero and exit cleanly.
"""

import asyncio
import gc
import os
import signal
import subprocess
import sys
import time

import pytest
import rclpy
from rclpy.parameter import Parameter

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import fleet_harness as harness  # noqa: E402
from fleet_harness import is_moving, is_zero  # noqa: E402

from fleet_agent.node import FleetAgent  # noqa: E402

DEADMAN_MS = 300


@pytest.fixture(scope="module")
def server():
    server = harness.FleetServer.start()
    yield server
    server.close()


@pytest.fixture(scope="module")
def ros():
    rclpy.init()
    yield
    rclpy.shutdown()


class Rig:
    """A FleetAgent in this process, plus a Recorder on its cmd_vel topic."""

    def __init__(self, server, name, *, spin_agent=True):
        self.server = server
        self.name = name
        topic = f"/test/watchdog/{name}/cmd_vel"
        parameters = harness.agent_parameters(
            server,
            name,
            {
                "drive.type": "twist",
                "drive.max_v_mps": 1.0,
                "drive.max_w_radps": 1.5,
                "cmd_vel.topic": topic,
                "cmd_vel.deadman_ms": DEADMAN_MS,
            },
        )
        self.agent = FleetAgent(parameter_overrides=[Parameter(k, value=v) for k, v in parameters.items()])
        # Not spinning the agent is the stalled ROS thread: its watchdog timer never runs.
        self.rec = harness.Recorder(f"test_{name}", topic, nodes=(self.agent,) if spin_agent else ())
        self.agent.start()

    def close(self):
        self.agent.stop()
        self.rec.close()
        self.agent.destroy_node()
        if self.agent.crashed is not None:
            # The loop was killed with its tasks pending. Collect them now, quietly, or
            # each one prints "Event loop is closed" at interpreter exit.
            self.agent = None
            hook, sys.unraisablehook = sys.unraisablehook, lambda unraisable: None
            try:
                gc.collect()
            finally:
                sys.unraisablehook = hook

    def drive_then(self, fail):
        """Drives the robot, calls ``fail()`` mid-stream, and checks the stop. Returns the gap in ms."""

        async def scenario():
            async with harness.watching(self.server) as watcher, harness.operating(self.server) as op:
                robot = await watcher.robot(self.name, lambda r: "drive" in (r.get("manifest") or {}))
                start = time.monotonic()
                lease = await op.claim(robot["robot_id"])
                driving = asyncio.ensure_future(op.drive(lease, 0.5))
                try:
                    await self.rec.cmd(is_moving, since=start)
                    await asyncio.sleep(0.5)
                    failed = time.monotonic()
                    fail()
                    await asyncio.sleep(1.0)
                    return failed
                finally:
                    driving.cancel()

        failed = asyncio.run(scenario())
        # Stopped, and still stopped a second later with the operator sending. Bounds
        # are extended by the time the host froze this process, if it did.
        last_moving, stopped = self.rec.final_stop(failed - 0.3)
        gap_ms, stall_ms = (stopped - last_moving) * 1000, self.rec.host_stall_ms(failed - 0.3)
        print(f"{self.name}: zero {gap_ms:.0f} ms after the last non-zero cmd_vel, host stall {stall_ms:.0f} ms")
        assert gap_ms <= DEADMAN_MS + stall_ms, self.rec.timeline(failed - 0.3)
        assert (stopped - failed) * 1000 <= DEADMAN_MS + stall_ms, self.rec.timeline(failed - 0.3)
        return gap_ms


@pytest.fixture
def rig(server, ros, request):
    rigs = []

    def make(name, **kwargs):
        rigs.append(Rig(server, name, **kwargs))
        return rigs[-1]

    yield make
    for r in rigs:
        r.close()


def test_blocked_sdk_loop_is_stopped_by_the_ros_watchdog(rig):
    r = rig("sdk_stalled")
    # Block the SDK's event loop: it reads no twist and its own deadman cannot fire.
    r.drive_then(lambda: r.agent._loop.call_soon_threadsafe(time.sleep, 2.0))
    assert r.agent.gate.watchdog_stops >= 1


def test_dead_link_thread_is_stopped_by_the_ros_watchdog(rig):
    r = rig("sdk_dead")
    # Stopping the loop under run_until_complete raises in the link thread: it dies
    # without the SDK getting to close anything or issue a stop.
    r.drive_then(lambda: r.agent._loop.call_soon_threadsafe(r.agent._loop.stop))
    assert r.agent.gate.watchdog_stops >= 1
    r.agent._thread.join(5)
    assert not r.agent._thread.is_alive() and r.agent.finished
    assert r.agent.crashed is not None


def test_help_request_does_not_block_the_executor_on_a_stuck_link(rig):
    r = rig("help_stalled")
    # Stands for the cmd_vel watchdog: a timer in the node's default callback group.
    ticks = []
    r.agent.create_timer(0.02, lambda: ticks.append(time.monotonic()))

    async def scenario():
        async with harness.watching(r.server) as watcher:
            await watcher.robot(r.name)
            await r.rec.lease(lambda m: m.connected)
            # The link thread stops taking work (it would send the request).
            r.agent._loop.call_soon_threadsafe(time.sleep, 4.0)
            await asyncio.sleep(0.2)
            began = time.monotonic()
            answer = await harness.request_help(r.rec.node, "stuck")
            return began, answer, time.monotonic()

    began, answer, ended = asyncio.run(scenario())
    stall_s = r.rec.host_stall_ms(began) / 1000
    # The request is given up after HELP_TIMEOUT_S (2 s), and says so.
    assert (answer.accepted, answer.code) == (False, "timeout"), answer
    assert 1.5 < ended - began < 3.5 + stall_s, ended - began
    # All that time the node's other callbacks kept running: the service callback
    # was waiting, not sitting on the ROS thread.
    during = [t for t in ticks if began <= t <= ended]
    gaps = [b - a for a, b in zip([began, *during], [*during, ended])]
    assert max(gaps) < 0.2 + stall_s, (max(gaps), len(during))


def test_stalled_ros_thread_is_stopped_by_the_sdk_deadman(rig):
    r = rig("ros_stalled", spin_agent=False)
    # The agent node is never spun, so the watchdog timer never runs. Twist stops
    # arriving when the operator goes away; only the SDK's timer is left to notice.
    stop_driving = []

    async def scenario():
        async with harness.watching(r.server) as watcher, harness.operating(r.server) as op:
            robot = await watcher.robot(r.name, lambda s: "drive" in (s.get("manifest") or {}))
            start = time.monotonic()
            lease = await op.claim(robot["robot_id"])
            for _ in range(10):
                await op.twist(lease, 0.5)
                await asyncio.sleep(0.05)
            stop_driving.append(start)
            await r.rec.cmd(is_zero, since=start)

    asyncio.run(scenario())
    start = stop_driving[0]
    gap_ms, stall_ms = r.rec.stop_gap_ms(start), r.rec.host_stall_ms(start)
    print(f"{r.name}: zero {gap_ms:.0f} ms after the last non-zero cmd_vel, host stall {stall_ms:.0f} ms")
    assert gap_ms <= DEADMAN_MS + stall_ms, r.rec.timeline(start)
    assert r.agent.gate.watchdog_stops == 0


def test_sigterm_publishes_a_final_stop_and_exits_cleanly(server, ros):
    name, topic = "sigterm", "/test/watchdog/sigterm/cmd_vel"
    parameters = harness.agent_parameters(
        server,
        name,
        {"drive.type": "twist", "drive.max_v_mps": 1.0, "drive.max_w_radps": 1.5, "cmd_vel.topic": topic},
    )
    args = [arg for k, v in parameters.items() for arg in ("-p", f"{k}:={v}")]
    rec = harness.Recorder("test_sigterm", topic)
    node = subprocess.Popen([sys.executable, "-m", "fleet_agent.node", "--ros-args", *args])

    async def scenario():
        async with harness.watching(server) as watcher, harness.operating(server) as op:
            robot = await watcher.robot(name, lambda s: "drive" in (s.get("manifest") or {}))
            start = time.monotonic()
            lease = await op.claim(robot["robot_id"])
            driving = asyncio.ensure_future(op.drive(lease, 0.5))
            try:
                await rec.cmd(is_moving, since=start)
                await asyncio.sleep(0.3)
                signalled = time.monotonic()
                node.send_signal(signal.SIGTERM)
                # The operator is still sending twist while the node shuts down.
                code = await asyncio.to_thread(node.wait, 10)
                await asyncio.sleep(0.3)
                return signalled, code
            finally:
                driving.cancel()

    try:
        signalled, code = asyncio.run(scenario())
        _, stopped = rec.final_stop(signalled - 0.3)
        stall_ms = rec.host_stall_ms(signalled - 0.3)
    finally:
        if node.poll() is None:
            node.kill()
        rec.close()
    print(f"sigterm: zero {(stopped - signalled) * 1000:.0f} ms after the signal, host stall {stall_ms:.0f} ms")
    assert stopped - signalled < 0.2 + stall_ms / 1000, rec.timeline(signalled - 0.3)
    assert code == 0
