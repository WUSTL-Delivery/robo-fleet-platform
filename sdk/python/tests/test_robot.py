"""Robot role against the real fleet-server, driven by a fake operator that
speaks raw JSON over its own websocket (no SDK on the operator side)."""

from __future__ import annotations

import asyncio
import json
import time
import urllib.request
from typing import Any

import pytest
from websockets.asyncio.client import connect as ws_connect

from fleet import Backoff, ConnectionState, FleetClient, FleetClientError
from fleet.client import parse_envelope
from fleet.robot import DEADMAN_MS, LeaseChange, Robot, TwistCommand, geo_pose, local_pose

FAST = Backoff(initial=0.02, max=0.2)
DRIVE_MANIFEST = {"drive": {"type": "twist", "max_v_mps": 1.0, "max_w_radps": 1.5}, "battery": {}}


def _env(type_: str, payload: dict[str, Any]) -> str:
    return json.dumps({"v": 0, "type": type_, "ts_ms": int(time.time() * 1000), "payload": payload})


class RawOperator:
    """An operator speaking raw JSON: enroll with an invite, hello, heartbeat, claim, twist."""

    def __init__(self, server: Any) -> None:
        self.server = server
        self.ws: Any = None
        self.inbox: asyncio.Queue[dict[str, Any]] = asyncio.Queue()
        self._tasks: list[asyncio.Task[Any]] = []

    async def start(self, name: str) -> None:
        key = await asyncio.to_thread(self._mint_invite)
        async with ws_connect(self.server.ws_url) as enroll:
            await enroll.send(_env("enroll.request", {"enrollment_key": key, "kind": "operator", "name": name}))
            resp = json.loads(await enroll.recv())
        assert resp["type"] == "enroll.response", resp
        self.ws = await ws_connect(self.server.ws_url)
        await self.ws.send(_env("hello", {"token": resp["payload"]["token"]}))
        welcome = json.loads(await self.ws.recv())
        assert welcome["type"] == "welcome", welcome
        interval = welcome["payload"]["heartbeat_interval_ms"] / 1000
        self._tasks.append(asyncio.create_task(self._heartbeat(interval)))
        self._tasks.append(asyncio.create_task(self._read()))

    def _mint_invite(self) -> str:
        req = urllib.request.Request(
            f"{self.server.http_url}/api/admin/fleets/{self.server.fleet}/operator-invites",
            method="POST",
            headers={"Authorization": f"Bearer {self.server.admin_token}"},
        )
        with urllib.request.urlopen(req) as res:
            return json.load(res)["key"]

    async def _heartbeat(self, interval: float) -> None:
        while True:
            await asyncio.sleep(interval)
            await self.ws.send(_env("heartbeat", {}))

    async def _read(self) -> None:
        async for raw in self.ws:
            env = parse_envelope(raw)
            if env is not None:
                await self.inbox.put(env)

    async def expect(self, type_: str, timeout: float = 3.0) -> dict[str, Any]:
        async def loop() -> dict[str, Any]:
            while True:
                env = await self.inbox.get()
                if env["type"] == type_:
                    return env["payload"]

        return await asyncio.wait_for(loop(), timeout)

    async def claim(self, robot_id: str, steal: bool = False) -> str:
        payload: dict[str, Any] = {"robot_id": robot_id}
        if steal:
            payload["steal"] = True
        await self.ws.send(_env("lease.claim", payload))
        return (await self.expect("lease.granted"))["lease_id"]

    async def twist(self, lease_id: str, x: float, wz: float = 0.0) -> None:
        await self.ws.send(
            _env("twist", {"lease_id": lease_id, "linear": {"x_mps": x}, "angular": {"z_radps": wz}})
        )

    async def close(self) -> None:
        for t in self._tasks:
            t.cancel()
        if self.ws is not None:
            await self.ws.close()


class TwistLog:
    """Records every twist command with the loop time it arrived."""

    def __init__(self) -> None:
        self.items: list[tuple[float, TwistCommand]] = []
        self._cond = asyncio.Condition()

    def __call__(self, cmd: TwistCommand) -> None:
        self.items.append((asyncio.get_running_loop().time(), cmd))
        asyncio.ensure_future(self._notify())

    async def _notify(self) -> None:
        async with self._cond:
            self._cond.notify_all()

    async def wait_for(self, pred: Any, timeout: float = 3.0) -> tuple[float, TwistCommand]:
        async def loop() -> tuple[float, TwistCommand]:
            async with self._cond:
                while True:
                    for item in self.items:
                        if pred(item[1]):
                            return item
                    await self._cond.wait()

        return await asyncio.wait_for(loop(), timeout)

    def sources(self) -> list[str]:
        return [c.source for _, c in self.items]


@pytest.fixture
async def teardown():
    things: list[Any] = []
    yield things
    for t in things:
        await t.close()


async def start_robot(server: Any, teardown: list[Any], **kw: Any) -> tuple[Robot, TwistLog, list[LeaseChange]]:
    kw.setdefault("manifest", DRIVE_MANIFEST)
    robot = Robot(server.ws_url, name="robot-role-bot", enrollment_key=server.enroll_key, reconnect=FAST, **kw)
    teardown.append(robot)
    twists = TwistLog()
    leases: list[LeaseChange] = []
    robot.on_twist(twists)
    robot.on_lease(leases.append)
    await robot.connect()
    return robot, twists, leases


async def start_operator(server: Any, teardown: list[Any], name: str = "op") -> RawOperator:
    op = RawOperator(server)
    teardown.append(op)
    await op.start(name)
    return op


async def wait_until(pred: Any, timeout: float = 3.0) -> None:
    deadline = time.monotonic() + timeout
    while not pred():
        if time.monotonic() > deadline:
            raise AssertionError("condition not met in time")
        await asyncio.sleep(0.01)


async def test_twist_under_lease_reaches_handler_and_stale_lease_does_not(fleet_server, teardown):
    robot, twists, leases = await start_robot(fleet_server, teardown)
    alice = await start_operator(fleet_server, teardown, "alice")
    lease1 = await alice.claim(robot.robot_id)
    await wait_until(lambda: robot.lease_id == lease1)
    assert leases[-1].granted and leases[-1].lease_id == lease1 and robot.mode == "teleop"

    await alice.twist(lease1, 0.5, 0.2)
    _, cmd = await twists.wait_for(lambda c: c.source == "operator")
    assert (cmd.linear_x, cmd.linear_y, cmd.angular_z, cmd.lease_id) == (0.5, 0.0, 0.2, lease1)

    # Bob steals the robot; alice's lease1 is now stale.
    bob = await start_operator(fleet_server, teardown, "bob")
    lease2 = await bob.claim(robot.robot_id, steal=True)
    await wait_until(lambda: robot.lease_id == lease2)
    assert lease2 != lease1
    # The steal stops the robot at once: alice's setpoint must not carry over.
    assert twists.items[-1][1].is_stop and twists.items[-1][1].source == "revoked"

    before = len(twists.items)
    # End to end: the server refuses a twist on a dead lease, so it never arrives.
    await alice.twist(lease1, 0.9)
    assert (await alice.expect("error"))["code"] == "not_authorized"
    # Robot side (defense in depth): a stale-lease twist frame that does reach
    # the robot's socket is still ignored. Delivered as the raw JSON it would be.
    stale = parse_envelope(_env("twist", {"lease_id": lease1, "linear": {"x_mps": 0.9}, "angular": {"z_radps": 0}}))
    robot.client._dispatch(stale)  # noqa: SLF001
    await asyncio.sleep(0.1)
    assert len(twists.items) == before, twists.items[before:]

    await bob.twist(lease2, -0.3)
    _, cmd = await twists.wait_for(lambda c: c.lease_id == lease2 and c.source == "operator")
    assert cmd.linear_x == -0.3


async def test_deadman_zeroes_300ms_after_operator_stops(fleet_server, teardown):
    robot, twists, _ = await start_robot(fleet_server, teardown)
    op = await start_operator(fleet_server, teardown)
    lease = await op.claim(robot.robot_id)
    await wait_until(lambda: robot.lease_id == lease)

    loop = asyncio.get_running_loop()
    last_sent = 0.0
    for _ in range(10):  # 20 Hz for half a second: the deadman must not fire mid-stream
        last_sent = loop.time()
        await op.twist(lease, 0.4)
        await asyncio.sleep(0.05)
    t_zero, zero = await twists.wait_for(lambda c: c.source == "deadman")
    assert zero.is_stop and zero.lease_id == lease
    assert twists.sources().count("deadman") == 1
    assert twists.sources().count("operator") == 10
    elapsed_ms = (t_zero - last_sent) * 1000
    print(f"deadman fired {elapsed_ms:.1f} ms after the last twist was sent")
    assert DEADMAN_MS - 50 <= elapsed_ms <= DEADMAN_MS + 50, elapsed_ms
    # The lease is still held; the robot is just stopped until the next twist.
    assert robot.lease_id == lease


async def test_zero_twist_on_socket_loss(fleet_server, teardown):
    robot, twists, _ = await start_robot(fleet_server, teardown)
    op = await start_operator(fleet_server, teardown)
    lease = await op.claim(robot.robot_id)
    await wait_until(lambda: robot.lease_id == lease)
    await op.twist(lease, 0.6)
    await twists.wait_for(lambda c: c.source == "operator")

    loop = asyncio.get_running_loop()
    lost_at = loop.time()
    robot.client._ws.transport.abort()  # noqa: SLF001  (yank the cable)
    t_zero, zero = await twists.wait_for(lambda c: c.source == "disconnected")
    assert zero.is_stop
    assert (t_zero - lost_at) * 1000 < 100, "stop must be immediate, not the deadman"
    assert "deadman" not in twists.sources()

    # It reconnects, declares its manifest again, and still holds the lease.
    await wait_until(lambda: robot.state is ConnectionState.OPEN)
    assert robot.lease_id == lease
    await op.twist(lease, 0.2)
    await twists.wait_for(lambda c: c.source == "operator" and c.linear_x == 0.2)


async def test_revoke_stops_immediately_and_reports_the_lease(fleet_server, teardown):
    robot, twists, leases = await start_robot(fleet_server, teardown)
    op = await start_operator(fleet_server, teardown)
    lease = await op.claim(robot.robot_id)
    await wait_until(lambda: robot.lease_id == lease)
    await op.twist(lease, 0.5)
    t_last, _ = await twists.wait_for(lambda c: c.source == "operator")

    await op.ws.send(_env("lease.release", {"lease_id": lease, "resolution": "resolved"}))
    t_zero, zero = await twists.wait_for(lambda c: c.source == "revoked")
    assert zero.is_stop and (t_zero - t_last) * 1000 < DEADMAN_MS
    assert robot.lease_id is None and robot.mode == "autonomous"
    assert leases[-1] == LeaseChange(granted=False, lease_id=lease, reason="released")
    # No deadman after the revoke: the stop already happened.
    await asyncio.sleep(DEADMAN_MS / 1000 + 0.1)
    assert "deadman" not in twists.sources()


async def test_manifest_on_every_connect_telemetry_and_help(fleet_server, teardown):
    robot, _, _ = await start_robot(fleet_server, teardown, manifest={"battery": {}, "cameras": [{"id": "front"}]})
    watcher = FleetClient(fleet_server.ws_url, kind="service", enrollment_key=fleet_server.enroll_key, reconnect=FAST)
    teardown.append(watcher)
    events: asyncio.Queue[dict[str, Any]] = asyncio.Queue()
    snapshots: asyncio.Queue[dict[str, Any]] = asyncio.Queue()
    watcher.on("event", lambda env: events.put_nowait(env["payload"]))
    watcher.on("snapshot", lambda env: snapshots.put_nowait(env["payload"]))
    await watcher.connect()

    async def manifest_seen() -> dict[str, Any] | None:
        await watcher.send("subscribe", {"topics": ["telemetry", "events"]})
        snap = await asyncio.wait_for(snapshots.get(), 3)
        mine = [r for r in snap["robots"] if r["robot_id"] == robot.robot_id]
        return mine[0].get("manifest") if mine else None

    await wait_until_async(lambda: manifest_seen(), lambda m: m == {"battery": {}, "cameras": [{"id": "front"}]})

    # After a reconnect the server has forgotten the manifest; the robot re-declares it.
    robot.client._ws.transport.abort()  # noqa: SLF001
    await wait_until(lambda: robot.state is not ConnectionState.OPEN)
    await wait_until(lambda: robot.state is ConnectionState.OPEN)
    await wait_until_async(lambda: manifest_seen(), lambda m: m == {"battery": {}, "cameras": [{"id": "front"}]})

    assert await robot.telemetry(pose=local_pose(1.5, -2.0, yaw_rad=0.25, frame_id="map"), battery=77)
    ev = await next_event(events, "robot.telemetry")
    assert ev["data"] == {
        "pose": {"frame": "local", "x_m": 1.5, "y_m": -2.0, "frame_id": "map", "yaw_rad": 0.25},
        "battery": {"pct": 77.0},
    }
    assert await robot.telemetry(pose=geo_pose(38.648, -90.305, yaw_rad=1.0))
    ev = await next_event(events, "robot.telemetry")
    assert ev["data"]["pose"] == {"frame": "geographic", "lat": 38.648, "lon": -90.305, "yaw_rad": 1.0}
    with pytest.raises(ValueError):
        await robot.telemetry(pose={"lat": 1.0, "lon": 2.0})  # bare lat/lon has no frame (D7)

    await robot.request_help("low_confidence", {"leg": 3})
    ev = await next_event(events, "robot.help_requested")
    assert ev["robot_id"] == robot.robot_id and ev["data"]["reason"] == "low_confidence"
    assert robot.mode == "help"


async def test_no_drive_declared_means_twist_is_not_obeyed(fleet_server, teardown):
    robot, twists, _ = await start_robot(fleet_server, teardown, manifest={"battery": {}})
    op = await start_operator(fleet_server, teardown)
    lease = await op.claim(robot.robot_id)
    await wait_until(lambda: robot.lease_id == lease)
    await op.twist(lease, 0.5)
    await asyncio.sleep(0.15)
    assert "operator" not in twists.sources()


async def test_telemetry_is_dropped_and_help_raises_while_disconnected(fleet_server, teardown):
    robot = Robot(fleet_server.ws_url, manifest=DRIVE_MANIFEST, enrollment_key=fleet_server.enroll_key)
    teardown.append(robot)
    assert await robot.telemetry(battery=50) is False
    with pytest.raises(FleetClientError):
        await robot.request_help("stuck")


async def next_event(q: asyncio.Queue[dict[str, Any]], name: str) -> dict[str, Any]:
    async def loop() -> dict[str, Any]:
        while True:
            ev = await q.get()
            if ev["event"] == name:
                return ev

    return await asyncio.wait_for(loop(), 3)


async def wait_until_async(get: Any, pred: Any, timeout: float = 3.0) -> None:
    deadline = time.monotonic() + timeout
    while True:
        value = await get()
        if pred(value):
            return
        if time.monotonic() > deadline:
            raise AssertionError(f"last value: {value!r}")
        await asyncio.sleep(0.05)


async def test_channel_delegates_to_the_client(fleet_server, teardown):
    manifest = {**DRIVE_MANIFEST, "channels": ["robot-role-jobs"]}
    robot, _, _ = await start_robot(fleet_server, teardown, manifest=manifest)
    got: asyncio.Queue[tuple[str, Any]] = asyncio.Queue()
    robot.channel("robot-role-jobs").on_message(lambda sender, data: got.put_nowait((sender, data)))
    service = FleetClient(fleet_server.ws_url, kind="service", enrollment_key=fleet_server.enroll_key, reconnect=FAST)
    teardown.append(service)
    await service.connect()
    await service.channel("robot-role-jobs").publish({"job": 7}, to=robot.robot_id)
    sender, data = await asyncio.wait_for(got.get(), 3)
    assert sender == service.client_id and data == {"job": 7}
