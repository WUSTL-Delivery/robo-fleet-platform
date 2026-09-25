"""Shared pytest fixtures: the real fleet-server binary on an ephemeral port.

Every SDK test runs against the built server (docs/TESTING.md, layer 4), never a
mock. ``fleet_server`` builds ``server/cmd/fleet-server`` once per session with
``go build``, starts it on a free loopback port with a temp sqlite db, a declared
fleet + enrollment key, an admin token, and a short heartbeat interval, and waits
for ``/healthz`` before handing tests a :class:`FleetServer`.
"""

from __future__ import annotations

import os
import shutil
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Iterator

import pytest

REPO_ROOT = Path(__file__).resolve().parents[3]
SERVER_DIR = REPO_ROOT / "server"


@dataclass(frozen=True)
class FleetServer:
    """What a test needs to talk to the running fleet-server."""

    ws_url: str
    http_url: str
    fleet: str
    enroll_key: str
    admin_token: str
    heartbeat_interval_ms: int

    def log(self) -> str:
        """Everything the server printed so far (useful in assertion messages)."""
        return _LOG_PATH.read_text() if _LOG_PATH is not None and _LOG_PATH.exists() else ""


_LOG_PATH: Path | None = None


def _free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _wait_healthy(http_url: str, proc: subprocess.Popen[bytes], log_path: Path) -> None:
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise RuntimeError(f"fleet-server exited {proc.returncode}:\n{log_path.read_text()}")
        try:
            with urllib.request.urlopen(f"{http_url}/healthz", timeout=1) as res:
                if res.status == 200:
                    return
        except (urllib.error.URLError, ConnectionError, OSError):
            pass
        time.sleep(0.05)
    raise RuntimeError(f"fleet-server not healthy in 15s:\n{log_path.read_text()}")


@pytest.fixture(scope="session")
def fleet_server() -> Iterator[FleetServer]:
    """A real fleet-server for the whole test session."""
    global _LOG_PATH
    if shutil.which("go") is None:
        pytest.skip("go toolchain not found; fleet-server cannot be built")

    tmp = Path(tempfile.mkdtemp(prefix="fleet-sdk-python-"))
    binary = tmp / "fleet-server"
    subprocess.run(
        ["go", "build", "-o", str(binary), "./cmd/fleet-server"],
        cwd=SERVER_DIR,
        check=True,
    )

    port = _free_port()
    server = FleetServer(
        ws_url=f"ws://127.0.0.1:{port}/ws",
        http_url=f"http://127.0.0.1:{port}",
        fleet="sdk-python",
        enroll_key="sdk-python-enroll-key-0123456789",
        admin_token="sdk-python-admin-token-0123456789",
        # Short so a test can outlast several intervals and prove heartbeats keep it alive.
        heartbeat_interval_ms=200,
    )
    env = {
        **os.environ,
        "FLEET_LISTEN": f"127.0.0.1:{port}",
        "FLEET_DB": str(tmp / "fleet.db"),
        "FLEET_HEARTBEAT_INTERVAL_MS": str(server.heartbeat_interval_ms),
        "FLEET_SWEEP_MS": "50",
        "FLEET_BOOTSTRAP_FLEET": server.fleet,
        "FLEET_BOOTSTRAP_ENROLL_KEY": server.enroll_key,
        "FLEET_ADMIN_TOKEN": server.admin_token,
    }
    log_path = tmp / "server.log"
    _LOG_PATH = log_path
    with open(log_path, "wb") as log:
        proc = subprocess.Popen([str(binary)], cwd=tmp, env=env, stdout=log, stderr=subprocess.STDOUT)
    try:
        _wait_healthy(server.http_url, proc, log_path)
        yield server
    finally:
        if proc.poll() is None:
            proc.terminate()
            try:
                proc.wait(timeout=3)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
        shutil.rmtree(tmp, ignore_errors=True)
        _LOG_PATH = None
