"""Twist over a WebRTC data channel (protocol/README.md, "Teleop data plane").

The robot under test is the SDK's ``Robot`` with the ``webrtc`` extra. The
operator is scripted: a raw-JSON websocket for the control plane (the
``RawOperator`` of test_robot.py) and its own aiortc peer connection as the
offerer. Offer and answer travel through the real fleet-server's signal relay.

Skipped as a whole when aiortc is not installed.
"""

from __future__ import annotations

import asyncio
import json
import uuid
from pathlib import Path
from typing import Any

import pytest

pytest.importorskip("aiortc", reason="needs the webrtc extra: pip install -e '.[webrtc]'")

from aiortc import RTCConfiguration, RTCPeerConnection, RTCSessionDescription  # noqa: E402

from fleet import Backoff, ConnectionState  # noqa: E402
from fleet.robot import DEADMAN_MS, Robot  # noqa: E402
from fleet.webrtc import (  # noqa: E402
    MAX_MESSAGE_BYTES,
    ChannelPing,
    ChannelTwist,
    TwistAnswerer,
    parse_channel_message,
)
from test_lease_reconnect import LinkGate  # noqa: E402
from test_robot import RawOperator, TwistLog, _env, start_operator, start_robot, wait_until  # noqa: E402

FIXTURES = Path(__file__).resolve().parents[3] / "protocol" / "fixtures" / "datachannel"
DEADMAN_S = DEADMAN_MS / 1000


@pytest.fixture
async def teardown():
    things: list[Any] = []
    yield things
    for t in reversed(things):
        await t.close()


class Offerer:
    """The operator's end of the data plane, scripted: one peer connection, one channel."""

    def __init__(self, op: RawOperator, robot_id: str, lease_id: str) -> None:
        self.op, self.robot_id, self.lease_id = op, robot_id, lease_id
        self.session = uuid.uuid4().hex
        # No ICE servers: host candidates only, as the contract requires by default.
        self.pc = RTCPeerConnection(RTCConfiguration(iceServers=[]))
        self.dc = self.pc.createDataChannel("twist", ordered=False, maxRetransmits=0)
        self.received: asyncio.Queue[Any] = asyncio.Queue()
        self.opened = asyncio.Event()
        self.closed = asyncio.Event()
        self.seq = 0
        self.dc.on("open", self.opened.set)
        self.dc.on("close", self.closed.set)
        self.dc.on("message", lambda data: self.received.put_nowait(json.loads(data)))

    async def signal(self, kind: str, data: dict[str, Any]) -> None:
        await self.op.ws.send(_env("signal", {"to": self.robot_id, "kind": kind, "data": data}))

    async def offer(self, *, lease_id: str | None = None, trickle: bool = False) -> None:
        await self.pc.setLocalDescription(await self.pc.createOffer())
        sdp = self.pc.localDescription.sdp
        candidates: list[str] = []
        if trickle:
            # aiortc puts every candidate in the SDP. Take them out and send them
            # as `ice` signals, the way a browser trickles them.
            lines = sdp.split("\r\n")
            candidates = [ln[2:] for ln in lines if ln.startswith("a=candidate:")]
            assert candidates, sdp
            sdp = "\r\n".join(ln for ln in lines if not ln.startswith(("a=candidate:", "a=end-of-candidates")))
        await self.signal(
            "offer",
            {"session": self.session, "lease_id": lease_id or self.lease_id, "type": "offer", "sdp": sdp},
        )
        for i, c in enumerate(candidates):
            # One with sdpMid, the rest with both null: either form must work.
            init = {"candidate": c, "sdpMid": "0", "sdpMLineIndex": 0} if i == 0 else {"candidate": c, "sdpMid": None, "sdpMLineIndex": None}
            await self.signal("ice", {"session": self.session, "candidate": init})
        if trickle:
            await self.signal("ice", {"session": self.session, "candidate": {"candidate": ""}})  # ignored

    async def answer(self, timeout: float = 5.0) -> dict[str, Any]:
        """The robot's answer for this session, as the server relayed it."""

        async def loop() -> dict[str, Any]:
            while True:
                sig = await self.op.expect("signal", timeout)
                if sig["kind"] == "answer" and sig["data"].get("session") == self.session:
                    return sig

        return await asyncio.wait_for(loop(), timeout)

    async def connect(self, *, trickle: bool = False) -> "Offerer":
        await self.offer(trickle=trickle)
        sig = await self.answer()
        assert sig["from"] == self.robot_id, sig
        assert sig["data"]["type"] == "answer" and "to" not in sig
        await self.pc.setRemoteDescription(RTCSessionDescription(sdp=sig["data"]["sdp"], type="answer"))
        await asyncio.wait_for(self.opened.wait(), 10)
        return self

    def twist(self, x: float, wz: float = 0.0, *, lease_id: str | None = None, seq: int | None = None) -> None:
        if seq is None:
            self.seq += 1
            seq = self.seq
        self.raw({"lease_id": lease_id or self.lease_id, "seq": seq, "linear": {"x_mps": x}, "angular": {"z_radps": wz}})

    def raw(self, msg: Any) -> None:
        self.dc.send(msg if isinstance(msg, str) else json.dumps(msg))

    async def ping(self, value: int = 1) -> Any:
        self.raw({"ping": value})
        return await asyncio.wait_for(self.received.get(), 3)

    async def close(self) -> None:
        await self.pc.close()


async def leased(server: Any, teardown: list[Any], **robot_kw: Any) -> tuple[Robot, TwistLog, RawOperator, str]:
    robot, twists, _ = await start_robot(server, teardown, **robot_kw)
    assert robot.data_channel_enabled
    op = await start_operator(server, teardown)
    lease = await op.claim(robot.robot_id)
    await wait_until(lambda: robot.lease_id == lease)
    return robot, twists, op, lease


async def connected(server: Any, teardown: list[Any], **robot_kw: Any) -> tuple[Robot, TwistLog, RawOperator, str, Offerer]:
    robot, twists, op, lease = await leased(server, teardown, **robot_kw)
    link = Offerer(op, robot.robot_id, lease)
    teardown.append(link)
    await link.connect()
    await wait_until(lambda: robot.data_channel_open)
    return robot, twists, op, lease, link


def operator_twists(twists: TwistLog) -> list[Any]:
    return [c for _, c in twists.items if c.source == "operator"]


# ---------------------------------------------------------------------- the message shapes


def test_fixtures_parse_as_the_three_shapes():
    twist = json.loads((FIXTURES / "twist.json").read_text())
    parsed = parse_channel_message((FIXTURES / "twist.json").read_text())
    assert parsed == ChannelTwist(
        seq=twist["seq"],
        payload={"lease_id": twist["lease_id"], "linear": twist["linear"], "angular": twist["angular"]},
    )
    ping = json.loads((FIXTURES / "ping.json").read_text())["ping"]
    assert parse_channel_message((FIXTURES / "ping.json").read_bytes()) == ChannelPing(ping)
    # A pong is the robot's own message: it means nothing when received.
    assert parse_channel_message((FIXTURES / "pong.json").read_text()) is None


@pytest.mark.parametrize(
    "msg",
    [
        "not json",
        "[1, 2]",
        "null",
        "42",
        "{}",
        '{"ping": "1"}',
        '{"ping": NaN}',
        '{"ping": true}',
        '{"lease_id": "", "seq": 1, "linear": {"x_mps": 0}, "angular": {"z_radps": 0}}',
        '{"lease_id": "ls_1", "linear": {"x_mps": 0}, "angular": {"z_radps": 0}}',
        '{"lease_id": "ls_1", "seq": 0, "linear": {"x_mps": 0}, "angular": {"z_radps": 0}}',
        '{"lease_id": "ls_1", "seq": -3, "linear": {"x_mps": 0}, "angular": {"z_radps": 0}}',
        '{"lease_id": "ls_1", "seq": 1.5, "linear": {"x_mps": 0}, "angular": {"z_radps": 0}}',
        '{"lease_id": "ls_1", "seq": true, "linear": {"x_mps": 0}, "angular": {"z_radps": 0}}',
        '{"lease_id": "ls_1", "seq": 9007199254740992, "linear": {"x_mps": 0}, "angular": {"z_radps": 0}}',
        '{"lease_id": "ls_1", "seq": 1, "linear": {"x_mps": "fast"}, "angular": {"z_radps": 0}}',
        '{"lease_id": "ls_1", "seq": 1, "linear": {"x_mps": Infinity}, "angular": {"z_radps": 0}}',
        '{"lease_id": "ls_1", "seq": 1, "linear": {"x_mps": 0, "y_mps": "x"}, "angular": {"z_radps": 0}}',
        '{"lease_id": "ls_1", "seq": 1, "linear": {"x_mps": 0}, "angular": {}}',
        '{"lease_id": "ls_1", "seq": 1, "linear": [0], "angular": {"z_radps": 0}}',
        b"\xff\xfe",
    ],
)
def test_anything_else_is_ignored(msg):
    assert parse_channel_message(msg) is None


def test_message_size_limit_and_optional_fields():
    ok = {"lease_id": "ls_1", "seq": 7, "linear": {"x_mps": 1, "y_mps": -0.5}, "angular": {"z_radps": 0.25}}
    parsed = parse_channel_message(json.dumps(ok))
    assert isinstance(parsed, ChannelTwist) and parsed.payload["linear"] == {"x_mps": 1, "y_mps": -0.5}
    padded = json.dumps({**ok, "pad": "x" * MAX_MESSAGE_BYTES})
    assert parse_channel_message(padded) is None


def test_ice_servers_are_config_with_no_default():
    client = object()
    assert TwistAnswerer(client, on_twist=lambda p: True)._ice == []  # noqa: SLF001
    ice = TwistAnswerer(
        client,
        on_twist=lambda p: True,
        ice_servers=[{"urls": "stun:stun.example.org:3478"}, {"urls": ["turn:t.example.org"], "username": "u", "credential": "c"}],
    )._ice  # noqa: SLF001
    assert [s.urls for s in ice] == ["stun:stun.example.org:3478", ["turn:t.example.org"]]
    assert (ice[1].username, ice[1].credential) == ("u", "c")
    with pytest.raises(ValueError):
        TwistAnswerer(client, on_twist=lambda p: True, ice_servers=[{"url": "stun:x"}])


# ---------------------------------------------------------------------- through the real server


async def test_drives_over_the_data_channel_through_the_real_signal_relay(fleet_server, teardown):
    robot, twists, op, lease, link = await connected(fleet_server, teardown)

    # The answer carried the robot's candidates; with no ICE servers they are host candidates only.
    sdp = link.pc.remoteDescription.sdp
    assert "a=candidate:" in sdp and "typ srflx" not in sdp and "typ relay" not in sdp

    # Ping is echoed with the same number (the golden pair), and it is not a twist.
    ping = json.loads((FIXTURES / "ping.json").read_text())
    assert await link.ping(ping["ping"]) == json.loads((FIXTURES / "pong.json").read_text())
    assert twists.items == []

    link.twist(0.5, -0.3)
    _, cmd = await twists.wait_for(lambda c: c.source == "operator")
    assert (cmd.linear_x, cmd.linear_y, cmd.angular_z, cmd.lease_id, cmd.via) == (0.5, 0.0, -0.3, lease, "p2p")

    # The same handler, the same deadman: 20 Hz holds it off, silence fires it once.
    loop = asyncio.get_running_loop()
    last_sent = 0.0
    for _ in range(10):
        last_sent = loop.time()
        link.twist(0.4)
        await link.ping()  # pings in between must not hold the deadman off later
        await asyncio.sleep(0.05)
    assert "deadman" not in twists.sources()
    for _ in range(4):  # pings keep flowing while the twists stop
        await asyncio.sleep(0.05)
        await link.ping()
    t_zero, zero = await twists.wait_for(lambda c: c.source == "deadman")
    assert zero.is_stop and zero.lease_id == lease and zero.via is None
    elapsed_ms = (t_zero - last_sent) * 1000
    print(f"deadman fired {elapsed_ms:.1f} ms after the last channel twist was sent")
    assert DEADMAN_MS - 50 <= elapsed_ms <= DEADMAN_MS + 60, elapsed_ms
    assert twists.sources().count("deadman") == 1
    assert len(operator_twists(twists)) == 11 and all(c.via == "p2p" for c in operator_twists(twists))
    assert robot.lease_id == lease and robot.data_channel_open


async def test_wrong_lease_old_seq_and_junk_are_ignored_and_do_not_move_the_seq_mark(fleet_server, teardown):
    robot, twists, op, lease, link = await connected(fleet_server, teardown)
    link.twist(0.2)  # seq 1
    link.twist(0.3)  # seq 2
    await wait_until(lambda: len(operator_twists(twists)) == 2)

    # A lease this robot does not hold, with a seq far ahead of anything sent.
    for i in range(5):
        link.twist(0.9, lease_id="ls_not_this_one", seq=1_000_000 + i)
    # The right lease but a seq already passed: a late, reordered packet.
    link.twist(0.9, seq=1)
    link.twist(0.9, seq=2)
    # Not a twist at all; none of it closes the channel.
    link.raw("not json")
    link.raw("[]")
    link.raw({"lease_id": lease, "seq": 2_000_000, "linear": {"x_mps": "fast"}, "angular": {"z_radps": 0}})
    link.raw({"lease_id": lease, "seq": 2_000_001, "linear": {"x_mps": 1}, "angular": {"z_radps": 0}, "pad": "x" * 1024})
    link.dc.send(b"\x00\x01\x02")
    assert await link.ping(7) == {"pong": 7}  # in order on this loopback link: everything above was seen
    await asyncio.sleep(0.05)
    assert [c.linear_x for c in operator_twists(twists)] == [0.2, 0.3]

    # The refused twists did not move the mark: the real driver's next seq is still heard.
    link.twist(0.4)  # seq 3
    await wait_until(lambda: len(operator_twists(twists)) == 3)
    assert operator_twists(twists)[-1].linear_x == 0.4
    assert robot.data_channel_open and link.dc.readyState == "open"


async def test_closing_the_peer_connection_stops_the_robot_within_the_deadman_window(fleet_server, teardown):
    robot, twists, op, lease, link = await connected(fleet_server, teardown)
    loop = asyncio.get_running_loop()
    last_sent = 0.0
    for _ in range(6):
        last_sent = loop.time()
        link.twist(0.6)
        await asyncio.sleep(0.05)
    assert twists.items[-1][1].linear_x == 0.6

    closed_at = loop.time()
    await link.pc.close()
    t_zero, zero = await twists.wait_for(lambda c: c.is_stop)
    since_close_ms = (t_zero - closed_at) * 1000
    print(f"zero velocity {since_close_ms:.1f} ms after the peer connection was closed")
    assert zero.source == "deadman" and zero.lease_id == lease
    assert since_close_ms <= DEADMAN_MS
    assert (t_zero - last_sent) * 1000 <= DEADMAN_MS + 60

    # Closing a peer never ends a lease: the robot drops the session and waits.
    await wait_until(lambda: not robot.data_channel_open)
    assert robot.lease_id == lease and robot.mode == "teleop"
    assert "revoked" not in twists.sources() and "disconnected" not in twists.sources()

    # Bus twist drives it again (the operator's fallback) ...
    await asyncio.sleep(DEADMAN_S)
    await op.twist(lease, 0.25)
    _, cmd = await twists.wait_for(lambda c: c.source == "operator" and c.via == "bus")
    assert cmd.linear_x == 0.25
    # ... and a new offer under the same lease is answered.
    again = Offerer(op, robot.robot_id, lease)
    teardown.append(again)
    await again.connect()
    await wait_until(lambda: robot.data_channel_open)
    again.twist(0.35)  # seq 1 again: the mark starts at 0 for each new channel
    await twists.wait_for(lambda c: c.via == "p2p" and c.linear_x == 0.35)


async def test_a_bus_twist_is_ignored_within_300ms_of_an_obeyed_channel_twist(fleet_server, teardown):
    robot, twists, op, lease, link = await connected(fleet_server, teardown)

    # Before any channel twist the bus drives as always.
    await op.twist(lease, 0.1)
    await twists.wait_for(lambda c: c.via == "bus" and c.linear_x == 0.1)

    # While channel twists are fresh, a straggler on the bus asking for reverse changes nothing.
    link.twist(0.7)
    await twists.wait_for(lambda c: c.via == "p2p")  # the robot is on the channel now
    for _ in range(6):
        link.twist(0.7)
        await op.twist(lease, -1.0)
        await asyncio.sleep(0.05)
    await link.ping()
    assert all(c.linear_x != -1.0 for c in operator_twists(twists))
    assert robot.last_twist.linear_x == 0.7

    # A channel twist refused for its lease does not shadow the bus.
    await twists.wait_for(lambda c: c.source == "deadman")
    link.twist(0.9, lease_id="ls_not_this_one", seq=5_000_000)
    await link.ping()
    await op.twist(lease, 0.15)
    await twists.wait_for(lambda c: c.via == "bus" and c.linear_x == 0.15)

    # The fallback direction: 300 ms after the last obeyed channel twist, the bus drives.
    link.twist(0.5)
    await twists.wait_for(lambda c: c.via == "p2p" and c.linear_x == 0.5)
    await asyncio.sleep(DEADMAN_S + 0.05)
    await op.twist(lease, 0.2)
    await twists.wait_for(lambda c: c.via == "bus" and c.linear_x == 0.2)


async def test_trickled_candidates_are_held_until_the_offer_is_applied(fleet_server, teardown):
    robot, twists, op, lease = await leased(fleet_server, teardown)
    link = Offerer(op, robot.robot_id, lease)
    teardown.append(link)
    await link.connect(trickle=True)  # the offer's SDP has no candidates; they follow as `ice`
    await wait_until(lambda: robot.data_channel_open)
    link.twist(0.3)
    await twists.wait_for(lambda c: c.via == "p2p")


async def test_only_the_lease_holders_offer_for_the_current_lease_is_answered(fleet_server, teardown):
    robot, twists, _ = await start_robot(fleet_server, teardown)
    alice = await start_operator(fleet_server, teardown, "alice")
    bob = await start_operator(fleet_server, teardown, "bob")

    async def unanswered(link: Offerer, **kw: Any) -> None:
        teardown.append(link)
        await link.offer(**kw)
        with pytest.raises(asyncio.TimeoutError):
            await link.answer(timeout=0.7)
        assert not robot.data_channel_open

    # No lease at all.
    await unanswered(Offerer(alice, robot.robot_id, "ls_none"))

    lease = await alice.claim(robot.robot_id)
    await wait_until(lambda: robot.lease_id == lease)
    # Another operator, even quoting the right lease id.
    await unanswered(Offerer(bob, robot.robot_id, lease))
    # The right operator, the wrong lease id.
    await unanswered(Offerer(alice, robot.robot_id, lease), lease_id="ls_not_this_one")
    # Not SDP: refused quietly, and the robot is still there for a real offer.
    await alice.ws.send(_env("signal", {"to": robot.robot_id, "kind": "offer", "data": {"session": "junk", "lease_id": lease, "type": "offer", "sdp": "v=nonsense"}}))

    good = Offerer(alice, robot.robot_id, lease)
    teardown.append(good)
    await good.connect()
    good.twist(0.3)
    await twists.wait_for(lambda c: c.via == "p2p")

    # A steal: the old peer is closed, and only the new operator's offer is answered.
    lease2 = await bob.claim(robot.robot_id, steal=True)
    await wait_until(lambda: robot.lease_id == lease2)
    assert not robot.data_channel_open
    await asyncio.wait_for(good.closed.wait(), 5)
    await unanswered(Offerer(alice, robot.robot_id, lease))
    theirs = Offerer(bob, robot.robot_id, lease2)
    teardown.append(theirs)
    await theirs.connect()
    theirs.twist(-0.2)
    _, cmd = await twists.wait_for(lambda c: c.via == "p2p" and c.lease_id == lease2)
    assert cmd.linear_x == -0.2


async def test_a_new_offer_replaces_the_session_and_other_labels_are_closed(fleet_server, teardown):
    robot, twists, op, lease, first = await connected(fleet_server, teardown)

    second = Offerer(op, robot.robot_id, lease)
    teardown.append(second)
    other = second.pc.createDataChannel("not-twist")
    other_closed = asyncio.Event()
    other.on("close", other_closed.set)
    await second.connect()
    # One peer per lease, the newest: the first session's channel is closed by the robot.
    await asyncio.wait_for(first.closed.wait(), 5)
    await asyncio.wait_for(other_closed.wait(), 5)
    await wait_until(lambda: robot.data_channel_open)
    second.twist(0.3)
    await twists.wait_for(lambda c: c.via == "p2p")
    assert second.dc.readyState == "open"


async def test_lease_release_closes_the_peer_and_a_late_channel_twist_drives_nothing(fleet_server, teardown):
    robot, twists, op, lease, link = await connected(fleet_server, teardown)
    link.twist(0.5)
    await twists.wait_for(lambda c: c.via == "p2p")

    await op.ws.send(_env("lease.release", {"lease_id": lease, "resolution": "resolved"}))
    await twists.wait_for(lambda c: c.source == "revoked")
    assert robot.lease_id is None and not robot.data_channel_open
    before = len(twists.items)
    try:
        link.twist(0.9)
    except Exception:  # noqa: BLE001 - already closed under us: equally nothing to obey
        pass
    # The robot closed its end because the lease ended, not the other way round.
    await asyncio.wait_for(link.closed.wait(), 5)
    await asyncio.sleep(0.1)
    assert len(twists.items) == before


async def test_channel_twist_is_not_obeyed_while_the_control_connection_is_down(fleet_server, teardown):
    # A slow reconnect, so there is a window with the peer up and the control link down.
    robot, twists, op, lease, link = await connected(fleet_server, teardown, reconnect=Backoff(initial=1.6, max=1.6))
    link.twist(0.5)
    await twists.wait_for(lambda c: c.via == "p2p")

    robot.client._ws.transport.abort()  # noqa: SLF001  (yank the control cable; the peer stays)
    await twists.wait_for(lambda c: c.source == "disconnected")
    assert robot.state is not ConnectionState.OPEN
    before = len(twists.items)
    for _ in range(4):
        link.twist(0.9)
        await asyncio.sleep(0.05)
    assert await link.ping(3) == {"pong": 3}  # the peer is alive and answering
    assert robot.state is not ConnectionState.OPEN
    assert len(twists.items) == before, twists.items[before:]

    # It resumes when the control connection is back, and the refused twists did not move the mark.
    await wait_until(lambda: robot.state is ConnectionState.OPEN, timeout=5)
    assert robot.lease_id == lease and robot.data_channel_open
    link.twist(0.3)
    _, cmd = await twists.wait_for(lambda c: c.via == "p2p" and c.linear_x == 0.3)
    assert cmd.lease_id == lease


async def test_channel_twist_on_a_lease_that_expired_while_the_control_connection_was_down_drives_nothing(
    short_lease_server, teardown, monkeypatch
):
    # The hole this closes: the peer outlives a control-link blip, so an operator
    # end that keeps sending could drive a robot whose lease the server revoked
    # while the robot could not be told.
    robot, twists, op, lease, link = await connected(short_lease_server, teardown)
    link.twist(0.5)
    await twists.wait_for(lambda c: c.via == "p2p")

    gate = LinkGate(monkeypatch)
    gate.cut(robot)
    await twists.wait_for(lambda c: c.source == "disconnected")
    revoked = await op.expect("lease.revoked", timeout=short_lease_server.lease_ttl_ms / 1000 + 3)
    assert revoked["lease_id"] == lease and revoked["reason"] == "expired"
    assert await link.ping(7) == {"pong": 7}  # the peer is still up: only the lease is gone
    assert robot.state is not ConnectionState.OPEN

    gate.restore()
    await wait_until(lambda: robot.state is ConnectionState.OPEN, timeout=5)
    assert robot.client.welcome["lease"] is None
    assert robot.lease_id is None and robot.mode == "help"
    assert not robot.data_channel_open
    before = len(twists.items)
    for _ in range(4):
        try:
            link.twist(0.9)
        except Exception:  # noqa: BLE001 - the robot closed its end: equally nothing to obey
            break
        await asyncio.sleep(0.05)
    await asyncio.wait_for(link.closed.wait(), 5)
    assert len(twists.items) == before, twists.items[before:]


async def test_data_channel_off_means_no_answer_and_bus_twist_still_drives(fleet_server, teardown):
    robot, twists, _ = await start_robot(fleet_server, teardown, data_channel=False)
    assert not robot.data_channel_enabled
    op = await start_operator(fleet_server, teardown)
    lease = await op.claim(robot.robot_id)
    await wait_until(lambda: robot.lease_id == lease)
    link = Offerer(op, robot.robot_id, lease)
    teardown.append(link)
    await link.offer()
    with pytest.raises(asyncio.TimeoutError):
        await link.answer(timeout=0.7)
    await op.twist(lease, 0.4)
    _, cmd = await twists.wait_for(lambda c: c.source == "operator")
    assert cmd.via == "bus" and cmd.linear_x == 0.4
