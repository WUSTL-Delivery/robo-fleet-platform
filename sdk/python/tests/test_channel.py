"""Channels over the real fleet-server: a raw-JSON service talks to an SDK robot."""

from __future__ import annotations

import asyncio
import json
import time
from typing import Any

import pytest
from websockets.asyncio.client import connect as ws_connect

from fleet import (
    Backoff,
    Channel,
    ChannelTargetNotFound,
    ConnectionState,
    FleetClient,
    FleetClientError,
    StateChange,
)

FAST = Backoff(initial=0.02, max=0.2)


def _chan_env(type_: str, payload: dict[str, Any], id_: str | None = None) -> str:
    env: dict[str, Any] = {"v": 0, "type": type_, "ts_ms": int(time.time() * 1000), "payload": payload}
    if id_ is not None:
        env["id"] = id_
    return json.dumps(env)


class ChanRawService:
    """A service speaking raw JSON (no SDK): enroll, hello, heartbeat, and an inbox."""

    def __init__(self, server) -> None:
        self.server = server
        self.inbox: asyncio.Queue[dict[str, Any]] = asyncio.Queue()
        self.client_id = ""
        self._ws: Any = None
        self._tasks: list[asyncio.Task[Any]] = []

    async def start(self) -> ChanRawService:
        async with ws_connect(self.server.ws_url) as ws:
            await ws.send(_chan_env("enroll.request", {"enrollment_key": self.server.enroll_key, "kind": "service", "name": "chan-svc"}))
            res = json.loads(await ws.recv())
        assert res["type"] == "enroll.response", res
        self._ws = await ws_connect(self.server.ws_url)
        await self._ws.send(_chan_env("hello", {"token": res["payload"]["token"]}))
        welcome = json.loads(await asyncio.wait_for(self._ws.recv(), 5))
        assert welcome["type"] == "welcome", welcome
        self.client_id = welcome["payload"]["client_id"]
        interval = welcome["payload"]["heartbeat_interval_ms"] / 1000
        self._tasks = [asyncio.create_task(self._read()), asyncio.create_task(self._beat(interval))]
        return self

    async def _read(self) -> None:
        async for raw in self._ws:
            await self.inbox.put(json.loads(raw))

    async def _beat(self, interval: float) -> None:
        while True:
            await asyncio.sleep(interval)
            await self._ws.send(_chan_env("heartbeat", {}))

    async def send(self, type_: str, payload: dict[str, Any], id_: str | None = None) -> None:
        await self._ws.send(_chan_env(type_, payload, id_))

    async def next(self, type_: str, timeout: float = 5.0, **match: Any) -> dict[str, Any]:
        """The next inbound envelope of ``type_`` whose payload has ``match``; skips the rest."""
        loop = asyncio.get_running_loop()
        deadline = loop.time() + timeout
        while True:
            env = await asyncio.wait_for(self.inbox.get(), max(0.01, deadline - loop.time()))
            if env["type"] == type_ and all(env["payload"].get(k) == v for k, v in match.items()):
                return env

    async def close(self) -> None:
        for t in self._tasks:
            t.cancel()
        if self._ws is not None:
            await self._ws.close()


class ChanRecordingClient(FleetClient):
    """Records outbound frames and session sockets, so a test can kill the live socket."""

    def __init__(self, *args: Any, **kwargs: Any) -> None:
        super().__init__(*args, **kwargs)
        self.sent: list[dict[str, Any]] = []
        self.sockets: list[Any] = []

    async def _send_frame(self, ws: Any, env: dict[str, Any]) -> None:
        self.sent.append(env)
        if env["type"] == "hello":
            self.sockets.append(ws)
        await super()._send_frame(ws, env)


@pytest.fixture
async def chan_cleanup():
    items: list[Any] = []
    yield items
    for c in items:
        await c.close()


def _chan_robot(server, cleanup: list[Any], name: str) -> ChanRecordingClient:
    robot = ChanRecordingClient(server.ws_url, kind="robot", name=name, enrollment_key=server.enroll_key, reconnect=FAST)
    cleanup.append(robot)
    return robot


async def _chan_service(server, cleanup: list[Any]) -> ChanRawService:
    svc = await ChanRawService(server).start()
    cleanup.append(svc)
    return svc


async def _chan_wait_open(client: FleetClient, timeout: float = 5.0) -> StateChange:
    fut: asyncio.Future[StateChange] = asyncio.get_running_loop().create_future()
    off = client.on_state(lambda c: c.state is ConnectionState.OPEN and not fut.done() and fut.set_result(c))
    try:
        return await asyncio.wait_for(fut, timeout)
    finally:
        off()


async def _chan_next_snapshot(client: FleetClient, timeout: float = 5.0) -> dict[str, Any]:
    fut: asyncio.Future[dict[str, Any]] = asyncio.get_running_loop().create_future()
    off = client.on("snapshot", lambda env: fut.done() or fut.set_result(env["payload"]))
    try:
        return await asyncio.wait_for(fut, timeout)
    finally:
        off()


async def test_channel_directed_roundtrip_survives_reconnect(fleet_server, chan_cleanup):
    robot = _chan_robot(fleet_server, chan_cleanup, "chan-robot-rt")
    received: list[tuple[str, Any]] = []
    assignment = robot.channel("assignment")

    async def on_assignment(sender: str, data: Any) -> None:
        received.append((sender, data))
        await assignment.publish({"ack": data["seq"]}, to=sender)

    assignment.on_message(on_assignment)
    await robot.connect()
    svc = await _chan_service(fleet_server, chan_cleanup)

    # Service -> robot, directed; the robot's handler replies to the stamped sender.
    await svc.send("channel.publish", {"channel": "assignment", "to": robot.client_id, "data": {"seq": 1, "order_id": 17}})
    reply = await svc.next("channel.message", channel="assignment")
    assert reply["payload"] == {"channel": "assignment", "from": robot.client_id, "data": {"ack": 1}}
    assert received == [(svc.client_id, {"seq": 1, "order_id": 17})]

    # Force a reconnect: kill the live socket with no close handshake.
    first_subscribes = [e for e in robot.sent if e["type"] == "subscribe"]
    assert [e["payload"]["topics"] for e in first_subscribes] == [["channel:assignment"]]
    reopened = asyncio.ensure_future(_chan_wait_open(robot))
    resubscribed = asyncio.ensure_future(_chan_next_snapshot(robot))
    robot.sockets[-1].transport.abort()
    await reopened
    await resubscribed  # the re-subscribe was answered, so it is in place server-side
    assert len(robot.sockets) == 2
    subscribes = [e for e in robot.sent if e["type"] == "subscribe"]
    assert [e["payload"]["topics"] for e in subscribes] == [["channel:assignment"]] * 2

    # Directed again: same handler, same client id after reconnect.
    await svc.send("channel.publish", {"channel": "assignment", "to": robot.client_id, "data": {"seq": 2}}, "pub-2")
    reply = await svc.next("channel.message", channel="assignment")
    assert reply["payload"]["data"] == {"ack": 2}
    assert received[-1] == (svc.client_id, {"seq": 2})

    # Broadcast reaches the robot only through its (re-)subscription: it sent no manifest.
    await svc.send("channel.publish", {"channel": "assignment", "broadcast": True, "data": {"seq": 3}})
    reply = await svc.next("channel.message", channel="assignment")
    assert reply["payload"]["data"] == {"ack": 3}
    assert received[-1] == (svc.client_id, {"seq": 3})
    assert len(received) == 3


async def test_channel_publish_to_offline_client_raises_not_found(fleet_server, chan_cleanup):
    robot = _chan_robot(fleet_server, chan_cleanup, "chan-robot-nf")
    await robot.connect()
    svc = await _chan_service(fleet_server, chan_cleanup)
    gone = svc.client_id
    await svc.close()
    await asyncio.sleep(0.1)  # let the server notice the socket is gone

    ch = robot.channel("edge_report")
    started = time.monotonic()
    with pytest.raises(ChannelTargetNotFound) as info:
        await ch.publish({"edge_id": "e12"}, to=gone, id="nf-1")
    assert time.monotonic() - started < 0.4  # the error ends the wait early, not the window
    err = info.value
    assert isinstance(err, FleetClientError) and err.code == "not_found"
    assert err.channel == "edge_report" and err.to == gone

    with pytest.raises(ChannelTargetNotFound):
        await ch.publish({"edge_id": "e12"}, to="r_never_existed")  # generated id

    # Fire-and-forget does not wait, so it cannot see the error; the socket stays up.
    env = await ch.publish({"edge_id": "e12"}, to=gone, wait=0)
    assert env["type"] == "channel.publish" and env["payload"]["to"] == gone
    await asyncio.sleep(0.1)
    assert robot.state is ConnectionState.OPEN


async def test_channel_robot_broadcast_reaches_subscribed_service(fleet_server, chan_cleanup):
    robot = _chan_robot(fleet_server, chan_cleanup, "chan-robot-bc")
    await robot.connect()
    svc = await _chan_service(fleet_server, chan_cleanup)
    await svc.send("subscribe", {"topics": ["channel:edge_report"]})
    await svc.next("snapshot")

    # A broadcast does not wait (nothing can be missing), and reaches the subscriber.
    env = await robot.channel("edge_report").publish({"edge_id": "e7", "seconds": 41.5})
    assert env["payload"] == {"channel": "edge_report", "broadcast": True, "data": {"edge_id": "e7", "seconds": 41.5}}
    msg = await svc.next("channel.message", channel="edge_report")
    assert msg["payload"] == {"channel": "edge_report", "from": robot.client_id, "data": {"edge_id": "e7", "seconds": 41.5}}

    ok = await robot.channel("edge_report").publish({"x": 1}, to=svc.client_id, wait=0.1)
    assert ok["payload"]["to"] == svc.client_id
    assert (await svc.next("channel.message", channel="edge_report"))["payload"]["data"] == {"x": 1}


async def test_channel_handlers_share_state_and_unregister(fleet_server, chan_cleanup):
    robot = _chan_robot(fleet_server, chan_cleanup, "chan-robot-off")
    await robot.connect()
    svc = await _chan_service(fleet_server, chan_cleanup)

    got_a: list[Any] = []
    got_b: list[Any] = []
    off_a = robot.channel("status").on_message(lambda s, d: got_a.append(d))
    robot.channel("status").on_message(lambda s, d: got_b.append(d))  # another view, same channel
    await asyncio.sleep(0.05)
    # Registered while open: subscribed once, not once per handler.
    assert [e["payload"]["topics"] for e in robot.sent if e["type"] == "subscribe"] == [["channel:status"]]

    await svc.send("channel.publish", {"channel": "status", "to": robot.client_id, "data": 1})
    await svc.send("channel.publish", {"channel": "other", "to": robot.client_id, "data": "ignored"})
    await _chan_until(lambda: got_a == [1] and got_b == [1])
    off_a()
    await svc.send("channel.publish", {"channel": "status", "to": robot.client_id, "data": 2})
    await _chan_until(lambda: got_b == [1, 2])
    assert got_a == [1]

    snap = await robot.channel("status").subscribe()
    assert "robots" in snap


async def test_channel_validates_names_and_targets(fleet_server, chan_cleanup):
    robot = _chan_robot(fleet_server, chan_cleanup, "chan-robot-val")
    for bad in ["", "Upper", "-lead", "has space", "a" * 65, "chan:x"]:
        with pytest.raises(ValueError):
            robot.channel(bad)
    assert Channel(robot, "a.b_c-9").topic == "channel:a.b_c-9"
    assert robot.channel("a" * 64).name == "a" * 64
    with pytest.raises(FleetClientError) as info:
        await robot.channel("ok").publish({}, to="r_x")  # not connected yet
    assert info.value.code == "closed"
    with pytest.raises(ValueError):
        await robot.channel("ok").publish({}, to="")


async def _chan_until(cond, timeout: float = 5.0) -> None:
    deadline = time.monotonic() + timeout
    while not cond():
        if time.monotonic() > deadline:
            raise AssertionError("condition not met in time")
        await asyncio.sleep(0.01)
