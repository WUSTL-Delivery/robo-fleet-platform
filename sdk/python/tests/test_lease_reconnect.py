"""What a robot believes about its lease across a reconnect
(protocol/README.md, "A robot's lease at connect").

The first half runs against the real fleet-server with a two-second lease, so
a lease can expire while the robot's link is down. The second half uses a
scripted stand-in for the one thing the real server cannot be made to do:
welcome a robot the way a server older than ``welcome.lease`` would, or name a
lease the robot has never heard of.
"""

from __future__ import annotations

import asyncio
import json
from typing import Any

import pytest
from websockets.asyncio.server import serve

import fleet.client as fleet_client
from fleet import ConnectionState
from fleet.robot import LeaseChange, Robot
from test_robot import DRIVE_MANIFEST, FAST, TwistLog, _env, start_operator, start_robot, wait_until


@pytest.fixture
async def teardown():
    things: list[Any] = []
    yield things
    for t in things:
        await t.close()


class LinkGate:
    """Holds the robot's reconnects back until the test lets them through.

    Install it after the robot's first connect: it replaces the session dial
    of the SDK's client, not the enrollment one.
    """

    def __init__(self, monkeypatch: Any) -> None:
        self._open = asyncio.Event()
        self._open.set()
        real = fleet_client.ws_connect

        async def gated(*args: Any, **kw: Any) -> Any:
            await self._open.wait()
            return await real(*args, **kw)

        monkeypatch.setattr(fleet_client, "ws_connect", gated)

    def cut(self, robot: Robot) -> None:
        """Yanks the robot's control link and keeps it down."""
        self._open.clear()
        robot.client._ws.transport.abort()  # noqa: SLF001

    def restore(self) -> None:
        self._open.set()


def bus_twist(robot: Robot, lease_id: str, x: float) -> None:
    """A bus twist handed to the robot as if a server had relayed it."""
    robot.client._dispatch(  # noqa: SLF001
        {"v": 0, "type": "twist", "payload": {"lease_id": lease_id, "linear": {"x_mps": x}, "angular": {"z_radps": 0.0}}}
    )


# ---------------------------------------------------------------------- the real server


async def test_a_lease_that_expired_while_the_robot_was_away_is_dropped_on_reconnect(
    short_lease_server, teardown, monkeypatch
):
    robot, twists, leases = await start_robot(short_lease_server, teardown)
    op = await start_operator(short_lease_server, teardown)
    lease = await op.claim(robot.robot_id)
    await wait_until(lambda: robot.lease_id == lease)
    await op.twist(lease, 0.5)
    await twists.wait_for(lambda c: c.source == "operator")

    gate = LinkGate(monkeypatch)
    gate.cut(robot)
    await twists.wait_for(lambda c: c.source == "disconnected")

    # Nobody renews. The server revokes the lease while the robot cannot be told.
    revoked = await op.expect("lease.revoked", timeout=short_lease_server.lease_ttl_ms / 1000 + 3)
    assert revoked["lease_id"] == lease and revoked["reason"] == "expired"
    assert robot.state is not ConnectionState.OPEN

    gate.restore()
    await wait_until(lambda: robot.state is ConnectionState.OPEN, timeout=5)
    # The welcome said "lease": null, and the robot took its word in that same turn.
    assert robot.client.welcome["lease"] is None
    assert robot.lease_id is None and robot.lease is None
    assert robot.mode == "help"
    assert leases[-1] == LeaseChange(granted=False, lease_id=lease, reason=None)
    _, last = twists.items[-1]
    assert last.source == "revoked" and last.is_stop and last.lease_id is None

    # The old lease id moves nothing: not when a twist bearing it reaches the
    # robot, and the server does not relay one in the first place.
    before = len(twists.items)
    bus_twist(robot, lease, 0.9)
    await op.twist(lease, 0.9)
    assert (await op.expect("error"))["code"] == "not_authorized"
    await asyncio.sleep(0.1)
    assert len(twists.items) == before, twists.items[before:]

    # It is back in the queue like any other robot: claimed again, driven again.
    lease2 = await op.claim(robot.robot_id)
    await wait_until(lambda: robot.lease_id == lease2)
    await op.twist(lease2, 0.3)
    _, cmd = await twists.wait_for(lambda c: c.source == "operator" and c.lease_id == lease2)
    assert cmd.linear_x == 0.3


async def test_a_lease_still_valid_after_the_blip_is_kept_without_a_second_grant(short_lease_server, teardown, monkeypatch):
    robot, twists, leases = await start_robot(short_lease_server, teardown)
    op = await start_operator(short_lease_server, teardown)
    lease = await op.claim(robot.robot_id)
    await wait_until(lambda: robot.lease_id == lease)

    gate = LinkGate(monkeypatch)
    gate.cut(robot)
    await wait_until(lambda: robot.state is not ConnectionState.OPEN)
    # While it is away it still remembers the lease, and obeys nothing under it.
    assert robot.lease_id == lease
    gate.restore()
    await wait_until(lambda: robot.state is ConnectionState.OPEN, timeout=5)

    assert robot.client.welcome["lease"]["lease_id"] == lease
    assert robot.lease_id == lease and robot.mode == "teleop"
    # One grant, no revoke: the welcome confirmed what the robot already knew.
    assert [c.granted for c in leases] == [True]
    await op.twist(lease, 0.4)
    _, cmd = await twists.wait_for(lambda c: c.source == "operator")
    assert cmd.lease_id == lease and cmd.linear_x == 0.4


# ---------------------------------------------------------------------- a scripted server

ABSENT = object()


class ScriptedServer:
    """Speaks just enough protocol to welcome one robot, with whatever ``lease`` the test queued."""

    ROBOT_ID = "r_scripted"

    def __init__(self) -> None:
        #: welcome.lease for each connection in turn; ABSENT leaves the member out, as an older server does.
        self.leases: list[Any] = []
        self.sessions: asyncio.Queue[Any] = asyncio.Queue()
        self._srv: Any = None
        self.url = ""

    async def start(self) -> "ScriptedServer":
        self._srv = await serve(self._handle, "127.0.0.1", 0)
        port = self._srv.sockets[0].getsockname()[1]
        self.url = f"ws://127.0.0.1:{port}/ws"
        return self

    async def _handle(self, ws: Any) -> None:
        first = json.loads(await ws.recv())
        if first["type"] == "enroll.request":
            await ws.send(_env("enroll.response", {"token": "tok", "client_id": self.ROBOT_ID, "fleet_id": "f_scripted"}))
            return
        assert first["type"] == "hello", first
        welcome: dict[str, Any] = {
            "client_id": self.ROBOT_ID,
            "fleet_id": "f_scripted",
            "kind": "robot",
            "server_time_ms": 1755100000000,
            "heartbeat_interval_ms": 60_000,
        }
        lease = self.leases.pop(0) if self.leases else ABSENT
        if lease is not ABSENT:
            welcome["lease"] = lease
        await ws.send(_env("welcome", welcome))
        await self.sessions.put(ws)
        async for _ in ws:  # manifest, heartbeats
            pass

    def lease(self, lease_id: str, operator_id: str = "o_scripted") -> dict[str, Any]:
        return {"lease_id": lease_id, "robot_id": self.ROBOT_ID, "operator_id": operator_id, "expires_at_ms": 1755100015000}

    async def close(self) -> None:
        self._srv.close()
        await self._srv.wait_closed()


async def twist_to(ws: Any, lease_id: str, x: float) -> None:
    await ws.send(_env("twist", {"lease_id": lease_id, "linear": {"x_mps": x}, "angular": {"z_radps": 0.0}}))


async def scripted_robot(teardown: list[Any], leases: list[Any]) -> tuple[ScriptedServer, Robot, TwistLog, list[LeaseChange], Any]:
    """A robot connected to a scripted server and holding lease ``ls_one``, driven once."""
    server = await ScriptedServer().start()
    teardown.append(server)
    server.leases = [ABSENT, *leases]
    robot = Robot(server.url, manifest=DRIVE_MANIFEST, name="bot", enrollment_key="k", reconnect=FAST, data_channel=False)
    teardown.insert(0, robot)  # closed before the server it talks to
    twists = TwistLog()
    changes: list[LeaseChange] = []
    robot.on_twist(twists)
    robot.on_lease(changes.append)
    await robot.connect()
    ws = await server.sessions.get()
    await ws.send(_env("lease.granted", server.lease("ls_one")))
    await wait_until(lambda: robot.lease_id == "ls_one")
    await twist_to(ws, "ls_one", 0.5)
    await twists.wait_for(lambda c: c.source == "operator")
    return server, robot, twists, changes, ws


async def test_a_welcome_that_does_not_state_a_lease_confirms_nothing(teardown):
    # An older server: it never sends welcome.lease, and it never told the robot
    # that the lease ended while the link was down.
    server, robot, twists, changes, ws = await scripted_robot(teardown, [ABSENT])
    await ws.close()
    ws = await asyncio.wait_for(server.sessions.get(), 5)
    await wait_until(lambda: robot.state is ConnectionState.OPEN)

    assert "lease" not in robot.client.welcome
    assert robot.lease_id is None and robot.mode == "help"
    assert changes[-1] == LeaseChange(granted=False, lease_id="ls_one", reason=None)
    before = len(twists.items)
    await twist_to(ws, "ls_one", 0.9)
    await asyncio.sleep(0.15)
    assert len(twists.items) == before, twists.items[before:]

    # A fresh grant is the only way back.
    await ws.send(_env("lease.granted", server.lease("ls_two")))
    await wait_until(lambda: robot.lease_id == "ls_two")
    await twist_to(ws, "ls_two", 0.2)
    await twists.wait_for(lambda c: c.source == "operator" and c.lease_id == "ls_two")


async def test_a_welcome_naming_another_lease_replaces_the_one_held(teardown):
    server, robot, twists, changes, ws = await scripted_robot(teardown, [])
    server.leases = [server.lease("ls_two", "o_other")]
    await ws.close()
    ws = await asyncio.wait_for(server.sessions.get(), 5)
    await wait_until(lambda: robot.state is ConnectionState.OPEN)

    assert robot.lease_id == "ls_two" and robot.mode == "teleop"
    assert changes[-1] == LeaseChange(granted=True, lease_id="ls_two", operator_id="o_other", expires_at_ms=1755100015000)
    before = len(twists.items)
    await twist_to(ws, "ls_one", 0.9)
    await twist_to(ws, "ls_two", 0.2)
    _, cmd = await twists.wait_for(lambda c: c.source == "operator" and c.lease_id == "ls_two")
    assert cmd.linear_x == 0.2
    assert [c.linear_x for _, c in twists.items[before:] if c.source == "operator"] == [0.2]


async def test_a_robot_that_held_nothing_takes_the_lease_its_welcome_names(teardown):
    # The robot process restarted while it was leased: the server remembers, the robot does not.
    server = await ScriptedServer().start()
    teardown.append(server)
    server.leases = [server.lease("ls_kept")]
    robot = Robot(server.url, manifest=DRIVE_MANIFEST, name="bot", enrollment_key="k", reconnect=FAST, data_channel=False)
    teardown.insert(0, robot)
    twists = TwistLog()
    changes: list[LeaseChange] = []
    robot.on_twist(twists)
    robot.on_lease(changes.append)
    await robot.connect()
    ws = await server.sessions.get()

    assert robot.lease_id == "ls_kept" and robot.mode == "teleop"
    assert changes == [LeaseChange(granted=True, lease_id="ls_kept", operator_id="o_scripted", expires_at_ms=1755100015000)]
    await twist_to(ws, "ls_kept", 0.4)
    await twists.wait_for(lambda c: c.source == "operator" and c.lease_id == "ls_kept")
