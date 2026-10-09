"""Shared pytest fixtures: the real fleet-server binary on an ephemeral port.

Every SDK test runs against the built server (docs/TESTING.md, layer 4), never a
mock. ``fleet_server`` builds ``server/cmd/fleet-server`` once per session with
``go build``, starts it on a free loopback port with a temp sqlite db, a declared
fleet + enrollment key, an admin token, and a short heartbeat interval, and waits
for ``/healthz`` before handing tests a :class:`FleetServer`.

``short_lease_server`` is a second server from the same binary whose leases
expire after two seconds, for the tests that need a lease to run out.
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
    #: How long a lease lives without a renewal (the server's lease_ttl_ms).
    lease_ttl_ms: int = 15_000
    log_path: Path | None = None

    def log(self) -> str:
        """Everything the server printed so far (useful in assertion messages)."""
        return self.log_path.read_text() if self.log_path is not None and self.log_path.exists() else ""


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
def fleet_server_binary() -> Iterator[Path]:
    """fleet-server, built once per test session."""
    if shutil.which("go") is None:
        pytest.skip("go toolchain not found; fleet-server cannot be built")
    tmp = Path(tempfile.mkdtemp(prefix="fleet-sdk-python-bin-"))
    binary = tmp / "fleet-server"
    try:
        subprocess.run(["go", "build", "-o", str(binary), "./cmd/fleet-server"], cwd=SERVER_DIR, check=True)
        yield binary
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


def _run_server(binary: Path, fleet: str, lease_ttl_ms: int) -> Iterator[FleetServer]:
    """Starts the binary on a free loopback port with its own temp database."""
    tmp = Path(tempfile.mkdtemp(prefix="fleet-sdk-python-"))
    port = _free_port()
    log_path = tmp / "server.log"
    server = FleetServer(
        ws_url=f"ws://127.0.0.1:{port}/ws",
        http_url=f"http://127.0.0.1:{port}",
        fleet=fleet,
        enroll_key="sdk-python-enroll-key-0123456789",
        admin_token="sdk-python-admin-token-0123456789",
        # Short so a test can outlast several intervals and prove heartbeats keep it alive.
        heartbeat_interval_ms=200,
        lease_ttl_ms=lease_ttl_ms,
        log_path=log_path,
    )
    env = {
        **os.environ,
        "FLEET_LISTEN": f"127.0.0.1:{port}",
        "FLEET_DB": str(tmp / "fleet.db"),
        "FLEET_HEARTBEAT_INTERVAL_MS": str(server.heartbeat_interval_ms),
        "FLEET_LEASE_TTL_MS": str(lease_ttl_ms),
        "FLEET_SWEEP_MS": "50",
        "FLEET_BOOTSTRAP_FLEET": server.fleet,
        "FLEET_BOOTSTRAP_ENROLL_KEY": server.enroll_key,
        "FLEET_ADMIN_TOKEN": server.admin_token,
    }
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


@pytest.fixture(scope="session")
def fleet_server(fleet_server_binary: Path) -> Iterator[FleetServer]:
    """A real fleet-server for the whole test session."""
    yield from _run_server(fleet_server_binary, "sdk-python", lease_ttl_ms=15_000)


@pytest.fixture(scope="session")
def short_lease_server(fleet_server_binary: Path) -> Iterator[FleetServer]:
    """A second real fleet-server whose leases expire two seconds after the last renewal."""
    yield from _run_server(fleet_server_binary, "sdk-python-short-lease", lease_ttl_ms=2_000)
