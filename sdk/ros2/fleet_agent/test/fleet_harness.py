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

The server binary comes from ``$FLEET_SERVER_BIN`` (scripts/in_container.sh sets
it) or, failing that, is built with ``go build`` from this checkout's server/.
"""

from __future__ import annotations

import asyncio
import atexit
import contextlib
import os
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any, AsyncIterator, Callable, Optional

from launch_ros.actions import Node

from fleet import Backoff, FleetClient

#: Generous: CI runners and emulated containers are slow to discover DDS peers.
TIMEOUT_S = 30.0


class FleetServer:
    """A fleet-server process on a free loopback port, with one fleet and its enrollment key."""

    fleet = "ros2-test"
    enroll_key = "ros2-test-enroll-key-0123456789abcdef"
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

    def close(self) -> None:
        self.down()
        shutil.rmtree(self.workdir, ignore_errors=True)

    def log(self) -> str:
        """Everything the server printed so far (for assertion messages)."""
        return self.log_path.read_text() if self.log_path.exists() else ""

    def token_file(self, name: str) -> Path:
        """Where ``agent_node(server, name)`` keeps that robot's token."""
        return self.workdir / f"{name}.token.json"


def agent_node(server: FleetServer, name: str, parameters: Optional[dict[str, Any]] = None) -> Node:
    """The fleet_agent node pointed at ``server``, enrolling as ``name``."""
    return Node(
        package="fleet_agent",
        executable="fleet_agent",
        output="screen",
        parameters=[
            {
                "url": server.ws_url,
                "name": name,
                "token_file": str(server.token_file(name)),
                "enroll_key": server.enroll_key,
                **(parameters or {}),
            }
        ],
    )


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
