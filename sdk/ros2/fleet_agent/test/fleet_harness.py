"""Shared pieces for fleet_agent launch tests: a real fleet-server, the node, a watcher.

Every launch test runs the node against the real ``fleet-server`` binary, never a
mock (docs/TESTING.md). A test module typically does::

    import fleet_harness as harness

    @pytest.mark.launch_test
    def generate_test_description():
        server = harness.FleetServer.start()
        agent = harness.agent_node(server, "bot-1", {"drive.type": "twist", ...})
        return LaunchDescription([agent, ReadyToTest()]), {"server": server}

and its test methods take ``server`` as an argument, then observe the robot the
way the console does, through ``harness.watching(server)``.

For teleop tests there is also an operator (``operating(server)``: claim, twist,
release), a ``TcpProxy`` to put between the node and the server so the network
can be blackholed or cut, and a ``Recorder`` that timestamps what the node
publishes on cmd_vel and fleet/lease.

The server binary comes from ``$FLEET_SERVER_BIN`` (scripts/in_container.sh sets
it) or, failing that, is built with ``go build`` from this checkout's server/.
"""

from __future__ import annotations

import asyncio
import atexit
import contextlib
import os
import json
import shutil
import socket
import struct
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any, AsyncIterator, Callable, Optional

import rclpy
from geometry_msgs.msg import Twist
from launch_ros.actions import Node
from rclpy.executors import SingleThreadedExecutor
from rclpy.qos import DurabilityPolicy, QoSProfile, ReliabilityPolicy

from fleet import Backoff, FleetClient, FleetClientError
from fleet_agent_msgs.msg import Lease

#: Generous: CI runners and emulated containers are slow to discover DDS peers.
TIMEOUT_S = 30.0


class FleetServer:
    """A fleet-server process on a free loopback port, with one fleet and its enrollment key."""

    fleet = "ros2-test"
    enroll_key = "ros2-test-enroll-key-0123456789abcdef"
    admin_token = "ros2-test-admin-token-0123456789abcdef"
    #: Short, so a test can outlast several intervals and prove heartbeats keep the robot online.
    heartbeat_interval_ms = 200

    def __init__(self, binary: Path, workdir: Path, port: int) -> None:
        self.binary = binary
        self.workdir = workdir
        self.port = port
        self.ws_url = f"ws://127.0.0.1:{port}/ws"
        self.http_url = f"http://127.0.0.1:{port}"
        self.log_path = workdir / "server.log"
        self._proc: Optional[subprocess.Popen[bytes]] = None

    @classmethod
    def start(cls) -> FleetServer:
        """Starts a server and waits until it is healthy. Stopped at interpreter exit."""
        workdir = Path(tempfile.mkdtemp(prefix="fleet-agent-test-"))
        server = cls(_server_binary(workdir), workdir, _free_port())
        server.up()
        atexit.register(server.close)
        return server

    def up(self) -> None:
        """Starts the process (same port, same database) and waits for /healthz."""
        env = {
            **os.environ,
            "FLEET_LISTEN": f"127.0.0.1:{self.port}",
            "FLEET_DB": str(self.workdir / "fleet.db"),
            "FLEET_HEARTBEAT_INTERVAL_MS": str(self.heartbeat_interval_ms),
            "FLEET_SWEEP_MS": "50",
            "FLEET_BOOTSTRAP_FLEET": self.fleet,
            "FLEET_BOOTSTRAP_ENROLL_KEY": self.enroll_key,
            "FLEET_ADMIN_TOKEN": self.admin_token,
        }
        with open(self.log_path, "ab") as log:
            self._proc = subprocess.Popen(
                [str(self.binary)], cwd=self.workdir, env=env, stdout=log, stderr=subprocess.STDOUT
            )
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            if self._proc.poll() is not None:
                raise RuntimeError(f"fleet-server exited {self._proc.returncode}:\n{self.log()}")
            try:
                with urllib.request.urlopen(f"{self.http_url}/healthz", timeout=1) as res:
                    if res.status == 200:
                        return
            except (urllib.error.URLError, ConnectionError, OSError):
                pass
            time.sleep(0.05)
        raise RuntimeError(f"fleet-server not healthy in 15 s:\n{self.log()}")

    def down(self) -> None:
        """Stops the process; the database stays, so ``up()`` resumes the same fleet."""
        proc, self._proc = self._proc, None
        if proc is None or proc.poll() is not None:
            return
        proc.terminate()
        try:
            proc.wait(timeout=3)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()

    def kill(self) -> None:
        """SIGKILL: the server dies mid-sentence, with no goodbye to its clients."""
        proc, self._proc = self._proc, None
        if proc is not None and proc.poll() is None:
            proc.kill()
            proc.wait()

    def close(self) -> None:
        self.down()
        shutil.rmtree(self.workdir, ignore_errors=True)

    def mint_operator_invite(self) -> str:
        """A single-use operator invite key, from the admin API (docs/FLEETCTL.md)."""
        req = urllib.request.Request(
            f"{self.http_url}/api/admin/fleets/{self.fleet}/operator-invites",
            method="POST",
            headers={"Authorization": f"Bearer {self.admin_token}"},
        )
        with urllib.request.urlopen(req, timeout=5) as res:
            return json.load(res)["key"]

    def log(self) -> str:
        """Everything the server printed so far (for assertion messages)."""
        return self.log_path.read_text() if self.log_path.exists() else ""

    def token_file(self, name: str) -> Path:
        """Where ``agent_node(server, name)`` keeps that robot's token."""
        return self.workdir / f"{name}.token.json"


def agent_parameters(
    server: FleetServer, name: str, parameters: Optional[dict[str, Any]] = None, *, url: Optional[str] = None
) -> dict[str, Any]:
    """Parameters for a fleet_agent that enrolls on ``server`` as ``name``.

    ``url`` replaces the server's own endpoint, to route the node through a TcpProxy.
    """
    return {
        "url": url or server.ws_url,
        "name": name,
        "token_file": str(server.token_file(name)),
        "enroll_key": server.enroll_key,
        **(parameters or {}),
    }


def agent_node(
    server: FleetServer, name: str, parameters: Optional[dict[str, Any]] = None, *, url: Optional[str] = None
) -> Node:
    """The fleet_agent node pointed at ``server``, enrolling as ``name``."""
    return Node(
        package="fleet_agent",
        executable="fleet_agent",
        output="screen",
        parameters=[agent_parameters(server, name, parameters, url=url)],
    )


class TcpProxy:
    """A TCP relay to put between the node and the server, to break the network on demand.

    ``blackhole()`` is the cable pulled or the Wi-Fi gone: bytes vanish in both
    directions and nobody is told. ``cut()`` resets every connection, which both
    ends notice at once. After a blackhole, ``cut()`` then ``restore()`` brings the
    path back (the swallowed bytes are gone, so the old connections cannot continue).
    """

    def __init__(self, upstream_port: int) -> None:
        self._upstream_port = upstream_port
        self._listener = socket.create_server(("127.0.0.1", 0))
        self.port = self._listener.getsockname()[1]
        self.ws_url = f"ws://127.0.0.1:{self.port}/ws"
        self._blackholed = threading.Event()
        self._lock = threading.Lock()
        self._socks: list[socket.socket] = []
        threading.Thread(target=self._accept, name="tcp-proxy", daemon=True).start()
        atexit.register(self._listener.close)

    def blackhole(self) -> None:
        self._blackholed.set()

    def restore(self) -> None:
        self._blackholed.clear()

    def cut(self) -> None:
        with self._lock:
            socks, self._socks = self._socks, []
        for sock in socks:
            try:
                # Linger 0: close sends a reset, not an orderly shutdown.
                sock.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
                sock.close()
            except OSError:
                pass

    def _accept(self) -> None:
        while True:
            try:
                client, _ = self._listener.accept()
            except OSError:
                return
            try:
                upstream = socket.create_connection(("127.0.0.1", self._upstream_port), timeout=2)
            except OSError:
                client.close()  # server down: the node sees a refused/closed connection
                continue
            upstream.settimeout(None)
            with self._lock:
                self._socks += [client, upstream]
            for src, dst in ((client, upstream), (upstream, client)):
                threading.Thread(target=self._pump, args=(src, dst), daemon=True).start()

    def _pump(self, src: socket.socket, dst: socket.socket) -> None:
        try:
            while True:
                data = src.recv(65536)
                if not data:
                    break
                if not self._blackholed.is_set():
                    dst.sendall(data)
        except OSError:
            pass
        if self._blackholed.is_set():
            return  # one end gave up; the other must not find out until cut()
        for sock in (src, dst):
            try:
                sock.close()
            except OSError:
                pass


Velocity = tuple  # (linear x, linear y, angular z)


def is_zero(v: Velocity) -> bool:
    return v == (0.0, 0.0, 0.0)


def is_moving(v: Velocity) -> bool:
    return not is_zero(v)


class Recorder:
    """A ROS node on its own spin thread that timestamps what fleet_agent publishes.

    ``cmd_vel`` and ``leases`` are lists of ``(time.monotonic(), value)`` stamped
    on arrival, so a test's own scheduling does not blur the timings. ``nodes``
    are spun on the same thread (for a FleetAgent created inside the test).

    A deadline measured in user space is only as good as the host's scheduler: a
    shared CI runner or a laptop VM can freeze every process for 100 ms or more.
    The recorder therefore runs a metronome and ``host_stall_ms()`` reports how
    long the host kept it from running; timing assertions add that, and only
    that, to their bound. On a quiet host it is zero.
    """

    _BEAT_S = 0.005
    #: A beat this late is the host stalling, not ordinary jitter.
    _STALL_S = 0.02

    def __init__(
        self, name: str, cmd_vel_topic: str, lease_topic: str = "/fleet/lease", nodes: tuple = ()
    ) -> None:
        self.node = rclpy.create_node(name)
        self.cmd_vel: list[tuple[float, Velocity]] = []
        self.leases: list[tuple[float, Lease]] = []
        self._lease_topic = lease_topic
        self.node.create_subscription(Twist, cmd_vel_topic, self._on_cmd_vel, 50)
        self.node.create_subscription(Lease, lease_topic, self._on_lease, _LATCHED)
        self._executor = SingleThreadedExecutor()
        for node in (self.node, *nodes):
            self._executor.add_node(node)
        self._stopping = threading.Event()
        self._thread = threading.Thread(target=self._spin, name="recorder", daemon=True)
        self._thread.start()
        self._stalls: list[tuple[float, float]] = []
        threading.Thread(target=self._metronome, name="recorder-metronome", daemon=True).start()

    def _metronome(self) -> None:
        while not self._stopping.is_set():
            before = time.monotonic()
            time.sleep(self._BEAT_S)
            late = time.monotonic() - before - self._BEAT_S
            if late > self._STALL_S:
                self._stalls.append((before, late))

    def host_stall_ms(self, since: float) -> float:
        """Milliseconds the host kept this process from running since ``since``."""
        return sum(late for before, late in list(self._stalls) if before + late >= since) * 1000

    def _spin(self) -> None:
        while not self._stopping.is_set():
            self._executor.spin_once(timeout_sec=0.05)

    def _on_cmd_vel(self, msg: Twist) -> None:
        self.cmd_vel.append((time.monotonic(), (msg.linear.x, msg.linear.y, msg.angular.z)))

    def _on_lease(self, msg: Lease) -> None:
        self.leases.append((time.monotonic(), msg))

    def close(self) -> None:
        self._stopping.set()
        self._thread.join(timeout=5)
        self._executor.shutdown()
        self.node.destroy_node()

    async def cmd(
        self, match: Callable[[Velocity], bool], since: float = 0.0, timeout: float = TIMEOUT_S
    ) -> tuple[float, Velocity]:
        """Waits for a cmd_vel message that arrived after ``since`` and satisfies ``match``."""
        return await _first(self.cmd_vel, match, since, timeout, "cmd_vel")

    async def lease(
        self, match: Callable[[Lease], bool], since: float = 0.0, timeout: float = TIMEOUT_S
    ) -> Lease:
        """Waits for a fleet/lease message that arrived after ``since`` and satisfies ``match``."""
        return (await _first(self.leases, match, since, timeout, "fleet/lease"))[1]

    async def latched_lease(self, timeout: float = TIMEOUT_S) -> Lease:
        """What a node that subscribes only now is told: the latched lease state."""
        got: list[tuple[float, Lease]] = []
        sub = self.node.create_subscription(
            Lease, self._lease_topic, lambda msg: got.append((time.monotonic(), msg)), _LATCHED
        )
        try:
            return (await _first(got, lambda msg: True, 0.0, timeout, "latched fleet/lease"))[1]
        finally:
            self.node.destroy_subscription(sub)

    def stop_gap_ms(self, since: float) -> float:
        """Milliseconds from the last non-zero cmd_vel to the zero that followed it.

        Looks at the first run of non-zero setpoints after ``since``; that run has
        no zero inside it by construction, so a premature stop shows up as a short run.
        """
        last_moving: Optional[float] = None
        for t, v in list(self.cmd_vel):
            if t < since:
                continue
            if is_moving(v):
                last_moving = t
            elif last_moving is not None:
                return (t - last_moving) * 1000
        raise AssertionError(f"no stop after a non-zero cmd_vel since {since}: {self.cmd_vel[-10:]}")

    def final_stop(self, since: float) -> tuple[float, float]:
        """(time of the last non-zero cmd_vel, time of the zero after it), looking from ``since``.

        For "it stopped and stayed stopped": call it after watching for a while. It
        fails if the newest setpoint is not a stop. Unlike "the first zero after
        the event", it is not fooled by a repeat of an earlier stop. With no
        non-zero setpoint at all since ``since``, the first time is ``since``.
        """
        last_moving, stopped = since, None
        for t, v in list(self.cmd_vel):
            if t < since:
                continue
            if is_moving(v):
                last_moving, stopped = t, None
            elif stopped is None:
                stopped = t
        if stopped is None:
            raise AssertionError(f"cmd_vel is not stopped: {self.timeline(since)}")
        return last_moving, stopped

    def timeline(self, since: float) -> str:
        """cmd_vel since ``since`` as "ms: x/wz" entries, for assertion messages."""
        return ", ".join(
            f"{(t - since) * 1000:.0f}: {v[0]:g}/{v[2]:g}" for t, v in list(self.cmd_vel) if t >= since
        )

    def moving_count(self, since: float) -> int:
        return sum(1 for t, v in list(self.cmd_vel) if t >= since and is_moving(v))


_LATCHED = QoSProfile(
    depth=1, reliability=ReliabilityPolicy.RELIABLE, durability=DurabilityPolicy.TRANSIENT_LOCAL
)


async def _first(items: list, match: Callable[[Any], bool], since: float, timeout: float, what: str) -> Any:
    deadline = time.monotonic() + timeout
    while True:
        for t, value in list(items):
            if t >= since and match(value):
                return t, value
        if time.monotonic() > deadline:
            raise AssertionError(f"no matching {what} within {timeout} s; last saw {items[-5:]}")
        await asyncio.sleep(0.005)


class Operator:
    """An operator client: claims a robot, sends twist under the lease, hands back."""

    def __init__(self, client: FleetClient) -> None:
        self._client = client
        self._granted: asyncio.Queue[dict[str, Any]] = asyncio.Queue()
        self.errors: list[dict[str, Any]] = []
        client.on("lease.granted", lambda env: self._granted.put_nowait(env["payload"]))
        client.on("error", lambda env: self.errors.append(env["payload"]))

    async def claim(self, robot_id: str) -> str:
        """Takes the wheel of ``robot_id``; returns the lease id."""
        await self._client.send("lease.claim", {"robot_id": robot_id})
        try:
            while True:
                granted = await asyncio.wait_for(self._granted.get(), 5)
                if granted.get("robot_id") == robot_id:
                    return granted["lease_id"]
        except asyncio.TimeoutError:
            raise AssertionError(f"lease on {robot_id} not granted; server said {self.errors}") from None

    async def twist(self, lease_id: str, x_mps: float, w_radps: float = 0.0) -> None:
        await self._client.send(
            "twist", {"lease_id": lease_id, "linear": {"x_mps": x_mps}, "angular": {"z_radps": w_radps}}
        )

    async def drive(self, lease_id: str, x_mps: float, w_radps: float = 0.0, hz: float = 20.0) -> None:
        """Sends the same twist at ``hz`` until cancelled, or until this operator's link is gone."""
        with contextlib.suppress(FleetClientError):
            while True:
                await self.twist(lease_id, x_mps, w_radps)
                await asyncio.sleep(1 / hz)

    async def release(self, lease_id: str) -> None:
        """Hands the robot back (the lease is revoked with reason ``released``)."""
        await self._client.send("lease.release", {"lease_id": lease_id, "resolution": "resolved"})


@contextlib.asynccontextmanager
async def operating(server: FleetServer, name: str = "ros2-test-operator") -> AsyncIterator[Operator]:
    """Enrolls a throwaway operator on ``server`` (with a fresh invite) for the duration of the block."""
    invite = await asyncio.to_thread(server.mint_operator_invite)
    client = FleetClient(
        server.ws_url, kind="operator", name=name, enrollment_key=invite, reconnect=Backoff(initial=0.05, max=0.5)
    )
    await asyncio.wait_for(client.connect(), TIMEOUT_S)
    try:
        yield Operator(client)
    finally:
        await client.close()


class Watcher:
    """A service client that sees the fleet the way the console does (snapshot + events)."""

    def __init__(self, client: FleetClient) -> None:
        self._client = client
        self._snapshots: asyncio.Queue[dict[str, Any]] = asyncio.Queue()
        self._telemetry: asyncio.Queue[dict[str, Any]] = asyncio.Queue()
        client.on("snapshot", lambda env: self._snapshots.put_nowait(env["payload"]))
        client.on("event", self._on_event)

    def _on_event(self, env: dict[str, Any]) -> None:
        if env["payload"].get("event") == "robot.telemetry":
            self._telemetry.put_nowait(env["payload"])

    async def robot(
        self,
        name: str,
        match: Callable[[dict[str, Any]], bool] = lambda summary: True,
        timeout: float = TIMEOUT_S,
    ) -> dict[str, Any]:
        """Waits for robot ``name`` to be online (and ``match(summary)``); returns its summary.

        The summary is ``defs.schema.json#/$defs/robotSummary``: robot_id, name,
        presence, state, manifest, lease.
        """
        seen: Any = None
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            # Each subscribe is answered with a fresh snapshot of the fleet.
            await self._client.send("subscribe", {"topics": ["presence", "events", "telemetry"]})
            snapshot = await asyncio.wait_for(self._snapshots.get(), 5)
            seen = [r for r in snapshot["robots"] if r.get("name") == name]
            for summary in seen:
                if summary["presence"] == "online" and match(summary):
                    return summary
            await asyncio.sleep(0.1)
        raise AssertionError(f"robot {name!r} not online as expected within {timeout} s; last saw {seen}")

    async def telemetry(
        self,
        robot_id: str,
        match: Callable[[dict[str, Any]], bool] = lambda data: True,
        timeout: float = TIMEOUT_S,
    ) -> dict[str, Any]:
        """Waits for a telemetry sample from ``robot_id`` that satisfies ``match``; returns its data.

        Call ``robot()`` first: it is what subscribes this watcher to telemetry.
        """
        seen: Any = None
        deadline = time.monotonic() + timeout
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise AssertionError(f"no matching telemetry from {robot_id} within {timeout} s; last saw {seen}")
            try:
                event = await asyncio.wait_for(self._telemetry.get(), remaining)
            except asyncio.TimeoutError:
                continue
            if event.get("robot_id") != robot_id:
                continue
            seen = event["data"]
            if match(seen):
                return seen


@contextlib.asynccontextmanager
async def watching(server: FleetServer) -> AsyncIterator[Watcher]:
    """Connects a throwaway service client to ``server`` for the duration of the block."""
    client = FleetClient(
        server.ws_url,
        kind="service",
        name="ros2-test-watcher",
        enrollment_key=server.enroll_key,
        reconnect=Backoff(initial=0.05, max=0.5),
    )
    await asyncio.wait_for(client.connect(), TIMEOUT_S)
    try:
        yield Watcher(client)
    finally:
        await client.close()


async def repeat(action: Callable[[], None], period_s: float = 0.1) -> None:
    """Calls ``action`` until cancelled: keep publishing while a watcher waits."""
    while True:
        action()
        await asyncio.sleep(period_s)


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _server_binary(workdir: Path) -> Path:
    preset = os.environ.get("FLEET_SERVER_BIN")
    if preset:
        if not os.access(preset, os.X_OK):
            raise RuntimeError(f"FLEET_SERVER_BIN={preset} is not an executable file")
        return Path(preset)
    # No preset: build from this checkout. Resolve symlinks so it also works from a
    # colcon workspace whose src/ links to sdk/ros2.
    for parent in Path(__file__).resolve().parents:
        if (parent / "server" / "cmd" / "fleet-server").is_dir():
            if shutil.which("go") is None:
                break
            binary = workdir / "fleet-server"
            subprocess.run(["go", "build", "-o", str(binary), "./cmd/fleet-server"], cwd=parent / "server", check=True)
            return binary
    raise RuntimeError(
        "launch tests need a fleet-server binary: set FLEET_SERVER_BIN, or install Go and "
        "run from a checkout of the repo (sdk/ros2/README.md, Tests)"
    )
