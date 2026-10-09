"""Channels over the real fleet-server: a raw-JSON service talks to an SDK robot."""

from __future__ import annotations

import asyncio
import json
import os
import sys
import time
from pathlib import Path
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

from fleet.channel import ACKED_MEMORY_SECONDS, MAX_SEQ, _ack_seq, _acked_seq

FAST = Backoff(initial=0.02, max=0.2)

_REPO_ROOT = Path(__file__).resolve().parents[3]
_FIXTURES = _REPO_ROOT / "protocol" / "fixtures" / "valid"
#: Golden wire examples of the acked-send convention (protocol/README.md).
ACKED_PUBLISH = json.loads((_FIXTURES / "channel-publish-acked.json").read_text())
ACK_MESSAGE = json.loads((_FIXTURES / "channel-message-ack.json").read_text())


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


# -- acked receive (protocol/README.md, "Acked send (convention on channel data)", Receiver)


class ChanAckedRobot:
    """An SDK robot with an acked handler on one channel, and a service to send to it."""

    def __init__(self, robot: ChanRecordingClient, svc: ChanRawService, channel: str) -> None:
        self.robot, self.svc, self.channel = robot, svc, channel
        self.handled: list[tuple[str, Any]] = []

    async def send(self, seq: Any, data: Any = None, *, svc: ChanRawService | None = None, channel: str | None = None) -> None:
        payload = {"channel": channel or self.channel, "to": self.robot.client_id, "data": {"seq": seq, "data": data}}
        await (svc or self.svc).send("channel.publish", payload, f"acked-{seq}")

    async def ack(self, timeout: float = 5.0, svc: ChanRawService | None = None) -> dict[str, Any]:
        """The next channel.message payload the service gets."""
        return (await (svc or self.svc).next("channel.message", timeout))["payload"]

    async def no_message(self, wait: float = 0.3, svc: ChanRawService | None = None) -> None:
        with pytest.raises(asyncio.TimeoutError):
            await (svc or self.svc).next("channel.message", wait)

    def acks_sent(self) -> list[dict[str, Any]]:
        return [e["payload"] for e in self.robot.sent if e["type"] == "channel.publish"]


async def _chan_acked(server, cleanup: list[Any], name: str, handler=None, channel: str = "commands") -> ChanAckedRobot:
    robot = _chan_robot(server, cleanup, name)
    await robot.connect()
    svc = await _chan_service(server, cleanup)
    pair = ChanAckedRobot(robot, svc, channel)

    def record(sender: str, data: Any) -> None:
        pair.handled.append((sender, data))

    robot.channel(channel).on_acked(handler or record)
    return pair


def test_channel_acked_fixtures_have_the_reserved_shapes():
    sent = ACKED_PUBLISH["payload"]
    assert ACKED_PUBLISH["type"] == "channel.publish" and "to" in sent and "broadcast" not in sent
    seq = _acked_seq(sent["data"])
    assert seq == 1755100000001 and seq > 2**32
    assert _ack_seq(sent["data"]) is None
    back = ACK_MESSAGE["payload"]
    assert ACK_MESSAGE["type"] == "channel.message"
    assert _ack_seq(back["data"]) == seq and _acked_seq(back["data"]) is None
    assert back["channel"] == sent["channel"] and back["from"] == sent["to"]


def test_channel_acked_shapes_are_exact():
    assert _acked_seq({"seq": 1, "data": None}) == 1  # null is a legal inner data
    assert _acked_seq({"seq": MAX_SEQ, "data": [1]}) == MAX_SEQ == 9007199254740991
    for plain in [
        {"seq": 1},  # missing data
        {"seq": 1, "data": {}, "extra": 0},
        {"seq": 0, "data": {}},
        {"seq": -1, "data": {}},
        {"seq": MAX_SEQ + 1, "data": {}},
        {"seq": "1", "data": {}},
        {"seq": 1.5, "data": {}},
        {"seq": True, "data": {}},
        {"seq": None, "data": {}},
        {"ack": 1},
        [1, 2],
        "seq",
        None,
    ]:
        assert _acked_seq(plain) is None, plain
    assert _ack_seq({"ack": 7}) == 7
    for plain in [{"ack": 7, "seq": 7}, {"ack": 0}, {"ack": "7"}, {"ack": 7.5}, {"ack": False}, {"ack": MAX_SEQ + 1}, {}, 7]:
        assert _ack_seq(plain) is None, plain


async def test_channel_acked_fixture_roundtrip_first_delivery_and_resend(fleet_server, chan_cleanup):
    pair = await _chan_acked(fleet_server, chan_cleanup, "chan-robot-ack")
    robot, svc = pair.robot, pair.svc
    seq = ACKED_PUBLISH["payload"]["data"]["seq"]
    inner = ACKED_PUBLISH["payload"]["data"]["data"]
    # The fixture as the sender puts it on the wire; only the target is this run's robot.
    publish = {**ACKED_PUBLISH["payload"], "to": robot.client_id}
    expected_ack = {**ACK_MESSAGE["payload"], "from": robot.client_id}

    # First delivery: the handler gets the inner data (no seq), and the sender gets the ack fixture.
    await svc.send("channel.publish", publish, ACKED_PUBLISH["id"])
    assert await pair.ack() == expected_ack
    assert pair.handled == [(svc.client_id, inner)]

    # Re-sends (same seq, same data): acked every time, never handled again.
    for copies in (2, 3):
        await svc.send("channel.publish", publish, ACKED_PUBLISH["id"])
        assert await pair.ack() == expected_ack
        assert len(pair.acks_sent()) == copies
    assert pair.handled == [(svc.client_id, inner)]

    # What the robot put on the wire: one directed ack per copy received, nothing else.
    assert pair.acks_sent() == [{"channel": "commands", "to": svc.client_id, "data": {"ack": seq}}] * 3
    # on_acked needs no subscription: acked messages are directed.
    assert not [e for e in robot.sent if e["type"] == "subscribe"]
    await pair.no_message()  # acks are not re-sent on a timer


async def test_channel_acked_handler_failure_sends_no_ack_and_resend_is_new(fleet_server, chan_cleanup):
    calls: list[Any] = []

    def flaky(sender: str, data: Any) -> None:
        calls.append(data)
        if len(calls) == 1:
            raise RuntimeError("not ready")

    pair = await _chan_acked(fleet_server, chan_cleanup, "chan-robot-ackfail", flaky)
    await pair.send(41, {"op": "blink"})
    await pair.no_message()  # failed: forgotten, no ack
    assert calls == [{"op": "blink"}] and pair.acks_sent() == []

    await pair.send(41, {"op": "blink"})  # the sender's next re-send is treated as new
    assert (await pair.ack())["data"] == {"ack": 41}
    assert calls == [{"op": "blink"}] * 2

    await pair.send(41, {"op": "blink"})  # now done: acked, not handled
    assert (await pair.ack())["data"] == {"ack": 41}
    assert len(calls) == 2

    # Same for a coroutine handler that raises.
    async def boom(sender: str, data: Any) -> None:
        calls.append("async")
        raise RuntimeError("no")

    pair.robot.channel("other").on_acked(boom)
    await pair.send(41, None, channel="other")
    await pair.no_message()
    assert calls[-1] == "async" and len(pair.acks_sent()) == 2


async def test_channel_acked_repeat_while_in_progress_is_dropped(fleet_server, chan_cleanup):
    release = asyncio.Event()
    started: list[Any] = []

    async def slow(sender: str, data: Any) -> None:
        started.append(data)
        await release.wait()

    pair = await _chan_acked(fleet_server, chan_cleanup, "chan-robot-ackslow", slow)
    await pair.send(7, "job")
    await _chan_until(lambda: started == ["job"])
    await pair.send(7, "job")
    await pair.send(7, "job")
    await pair.no_message()  # in progress: repeats dropped, no ack yet
    assert started == ["job"] and pair.acks_sent() == []

    release.set()  # accepted: exactly one ack, for the first copy
    assert (await pair.ack())["data"] == {"ack": 7}
    await pair.no_message()
    assert len(pair.acks_sent()) == 1

    await pair.send(7, "job")  # done: ack again
    assert (await pair.ack())["data"] == {"ack": 7}
    assert started == ["job"]


async def test_channel_acked_dedup_key_is_channel_sender_seq(fleet_server, chan_cleanup):
    pair = await _chan_acked(fleet_server, chan_cleanup, "chan-robot-ackkey")
    other_svc = await _chan_service(fleet_server, chan_cleanup)
    elsewhere: list[Any] = []
    pair.robot.channel("elsewhere").on_acked(lambda sender, data: elsewhere.append((sender, data)))
    with pytest.raises(ValueError):
        pair.robot.channel("elsewhere").on_acked(lambda sender, data: None)  # one per channel

    await pair.send(5, "a")
    assert (await pair.ack())["data"] == {"ack": 5}
    await pair.send(5, "b", svc=other_svc)  # same seq, another sender: new
    assert await pair.ack(svc=other_svc) == {"channel": "commands", "from": pair.robot.client_id, "data": {"ack": 5}}
    await pair.send(5, "c", channel="elsewhere")  # same seq and sender, another channel: new
    assert await pair.ack() == {"channel": "elsewhere", "from": pair.robot.client_id, "data": {"ack": 5}}
    await pair.send(6, "d")  # another seq; gaps and order mean nothing
    assert (await pair.ack())["data"] == {"ack": 6}

    assert pair.handled == [(pair.svc.client_id, "a"), (other_svc.client_id, "b"), (pair.svc.client_id, "d")]
    assert elsewhere == [(pair.svc.client_id, "c")]
    await pair.no_message(0.2, svc=other_svc)  # acks go to the sender of that copy only


async def test_channel_acked_and_ordinary_data_share_a_channel(fleet_server, chan_cleanup):
    pair = await _chan_acked(fleet_server, chan_cleanup, "chan-robot-ackmix")
    robot, svc = pair.robot, pair.svc
    plain: list[Any] = []
    robot.channel("commands").on_message(lambda sender, data: plain.append(data))

    ordinary = [
        {"seq": 1, "data": {}, "extra": True},
        {"seq": 1},
        {"seq": "1", "data": {}},
        {"seq": 0, "data": {}},
        {"seq": MAX_SEQ + 1, "data": {}},
        {"seq": 1.5, "data": {}},
        {"ack": 1},
        "text",
    ]
    for data in ordinary:
        await svc.send("channel.publish", {"channel": "commands", "to": robot.client_id, "data": data})
    await pair.send(1, None)  # null inner data is legal
    await pair.send(MAX_SEQ, {"k": "v"})
    assert (await pair.ack())["data"] == {"ack": 1}
    assert (await pair.ack())["data"] == {"ack": MAX_SEQ}

    assert plain == ordinary  # handed over untouched, in order; acked messages not shown
    assert pair.handled == [(svc.client_id, None), (svc.client_id, {"k": "v"})]
    assert len(pair.acks_sent()) == 2

    # Without an acked handler the reserved shape is plain data and nothing is acked.
    seen: list[Any] = []
    robot.channel("plain").on_message(lambda sender, data: seen.append(data))
    await pair.send(9, "x", channel="plain")
    await _chan_until(lambda: seen == [{"seq": 9, "data": "x"}])
    await pair.no_message(0.2)


async def test_channel_acked_done_key_is_remembered_for_ten_minutes(fleet_server, chan_cleanup):
    assert ACKED_MEMORY_SECONDS == 600
    pair = await _chan_acked(fleet_server, chan_cleanup, "chan-robot-ackmem")
    hub = pair.robot._fleet_channel_hub
    clock = [1000.0]
    hub._now = lambda: clock[0]

    await pair.send(1, "first")
    assert (await pair.ack())["data"] == {"ack": 1}
    clock[0] += 300
    await pair.send(2, "second")
    assert (await pair.ack())["data"] == {"ack": 2}

    clock[0] = 1000.0 + ACKED_MEMORY_SECONDS  # ten minutes after seq 1 was first received
    await pair.send(1, "first")
    assert (await pair.ack())["data"] == {"ack": 1}
    assert len(pair.handled) == 2 and len(hub._seen) == 2  # re-acking did not extend or drop it

    clock[0] += 1  # past the memory: seq 1 forgotten (handled again), seq 2 still done
    await pair.send(2, "second")
    assert (await pair.ack())["data"] == {"ack": 2}
    assert list(hub._seen) == [("commands", pair.svc.client_id, 2)]
    await pair.send(1, "first")
    assert (await pair.ack())["data"] == {"ack": 1}
    assert [d for _, d in pair.handled] == ["first", "second", "first"]


async def test_channel_acked_ack_to_a_sender_that_left_is_dropped(fleet_server, chan_cleanup):
    release = asyncio.Event()
    started: list[Any] = []

    async def slow(sender: str, data: Any) -> None:
        started.append(data)
        await release.wait()

    pair = await _chan_acked(fleet_server, chan_cleanup, "chan-robot-ackgone", slow)
    errors: list[dict[str, Any]] = []
    pair.robot.on("error", lambda env: errors.append(env["payload"]))
    await pair.send(3, "job")
    await _chan_until(lambda: started == ["job"])
    await pair.svc.close()
    await asyncio.sleep(0.1)  # let the server notice the sender is gone

    release.set()
    await _chan_until(lambda: len(pair.acks_sent()) == 1)
    await _chan_until(lambda: [e["code"] for e in errors] == ["not_found"])
    await asyncio.sleep(0.2)
    assert len(pair.acks_sent()) == 1  # dropped, not retried
    assert pair.robot.state is ConnectionState.OPEN


async def test_channel_fake_robot_example_acks_through_the_helper(fleet_server, chan_cleanup, tmp_path):
    """examples/fake_robot.py, as a process: a job sent as an acked message comes back {ack: seq}."""
    proc = await asyncio.create_subprocess_exec(
        sys.executable, "-u", str(_REPO_ROOT / "sdk" / "python" / "examples" / "fake_robot.py"),
        "--url", fleet_server.ws_url, "--name", "chan-fake-robot", "--token-file", str(tmp_path / "token.json"), "--no-wander",
        env={**os.environ, "FLEET_ENROLL_KEY": fleet_server.enroll_key},
        stdin=asyncio.subprocess.DEVNULL, stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.STDOUT,
    )
    try:
        robot_id = ""
        while not robot_id:
            line = (await asyncio.wait_for(proc.stdout.readline(), 15)).decode()
            assert line, "fake_robot.py exited before coming online"
            if " online as " in line:
                robot_id = line.split(" online as ")[1].split()[0]
        svc = await _chan_service(fleet_server, chan_cleanup)
        seq = ACKED_PUBLISH["payload"]["data"]["seq"]
        publish = {**ACKED_PUBLISH["payload"], "channel": "jobs", "to": robot_id}
        expected = {**ACK_MESSAGE["payload"], "channel": "jobs", "from": robot_id}
        for _ in range(2):  # the re-send is acked too
            await svc.send("channel.publish", publish, ACKED_PUBLISH["id"])
            assert (await svc.next("channel.message", channel="jobs"))["payload"] == expected
        assert seq == expected["data"]["ack"]
        printed = b""
        while b"job from" not in printed:
            printed = await asyncio.wait_for(proc.stdout.readline(), 5)
        assert str(ACKED_PUBLISH["payload"]["data"]["data"]) in printed.decode()  # the inner data, once
    finally:
        proc.terminate()
        await proc.wait()
