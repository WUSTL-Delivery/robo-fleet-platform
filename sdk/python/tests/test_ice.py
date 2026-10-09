"""ICE servers for the teleop peer connection (protocol/README.md, "ICE servers").

Three real fleet-servers: one with nothing configured, one configured with a
TURN server (the stub in stun_stub.py), and that same one made to
answer ``ice.request`` as a server from before the message existed does.

The first half is ``FleetClient.ice_config()`` / ``ice_servers()`` and needs
nothing extra. The second half is what the robot's peer connection is created
with, and is skipped when aiortc is not installed.
"""

from __future__ import annotations

import asyncio
import base64
import hashlib
import hmac
import time
from typing import Any

import pytest

from fleet import Backoff, FleetClient, FleetClientError, MemoryTokenStore
from fleet.robot import Robot
from test_robot import start_operator, start_robot, wait_until

try:
    import aiortc  # noqa: F401
except ImportError:
    HAVE_WEBRTC = False
else:
    HAVE_WEBRTC = True

needs_webrtc = pytest.mark.skipif(not HAVE_WEBRTC, reason="needs the webrtc extra: pip install -e '.[webrtc]'")


@pytest.fixture
async def teardown():
    things: list[Any] = []
    yield things
    for t in reversed(things):
        await t.close()


async def connect(server: Any, teardown: list[Any], kind: str = "robot") -> FleetClient:
    client = FleetClient(server.ws_url, kind=kind, name=f"ice-{kind}", enrollment_key=server.enroll_key, reconnect=None)  # type: ignore[arg-type]
    teardown.append(client)
    await client.connect()
    return client


def answer_like_an_older_server(client: FleetClient) -> list[str]:
    """Makes ``client``'s server one from before ``ice.request``.

    The request never reaches the real server; the client is handed what such
    a server answers to a message type it does not know, through the same
    path as any inbound frame. Returns the ids of the requests refused.
    """
    refused: list[str] = []
    real = client._send_frame  # noqa: SLF001

    async def send_frame(ws: Any, env: dict[str, Any]) -> None:
        if env["type"] != "ice.request":
            return await real(ws, env)
        refused.append(env["id"])
        reply = {
            "v": 0,
            "type": "error",
            "payload": {"code": "invalid_message", "message": 'unknown message type "ice.request"', "ref": env["id"]},
        }

        def deliver() -> None:
            client._settle_ice(reply)  # noqa: SLF001
            client._dispatch(reply)  # noqa: SLF001

        asyncio.get_running_loop().call_soon(deliver)

    client._send_frame = send_frame  # type: ignore[method-assign]  # noqa: SLF001
    return refused


def assert_configured(servers: list[Any], server: Any, client_id: str) -> None:
    """``servers`` is what ``server`` hands ``client_id``: its URLs and a TURN credential that is that client's own."""

    def get(s: Any, key: str) -> Any:
        return s.get(key) if isinstance(s, dict) else getattr(s, key)

    stun = [s for s in servers if get(s, "username") is None]
    turn = [s for s in servers if get(s, "username") is not None]
    assert [get(s, "urls") for s in stun] == ([server.stun_urls] if server.stun_urls else [])
    assert len(turn) == 1 and get(turn[0], "urls") == server.turn_urls
    # username is "<expiry in Unix seconds>:<client id>", the credential its HMAC under the shared secret.
    username = get(turn[0], "username")
    expiry, who = username.split(":")
    assert who == client_id
    assert int(expiry) > time.time()
    want = base64.b64encode(hmac.new(server.turn_secret.encode(), username.encode(), hashlib.sha1).digest()).decode()
    assert get(turn[0], "credential") == want


# ---------------------------------------------------------------------- the client


async def test_nothing_configured_is_an_empty_list_not_an_error(fleet_server, teardown):
    client = await connect(fleet_server, teardown)
    cfg = await client.ice_config()
    assert cfg["ice_servers"] == [] and "expires_at_ms" not in cfg
    assert cfg["ref"].startswith("ice-")
    assert await client.ice_servers() == []


async def test_configured_servers_come_with_a_credential_minted_for_the_asker(ice_server, teardown):
    client = await connect(ice_server, teardown)
    cfg = await client.ice_config()
    assert_configured(cfg["ice_servers"], ice_server, client.client_id)
    assert cfg["expires_at_ms"] > time.time() * 1000
    # Every answer is matched to its own request, also when several are out at once.
    a, b, c = await asyncio.gather(client.ice_config(), client.ice_config(), client.ice_servers())
    assert a["ref"] != b["ref"]
    assert_configured(c, ice_server, client.client_id)


async def test_a_refusal_raises_from_ice_config_and_is_an_empty_list_from_ice_servers(ice_server, teardown):
    service = await connect(ice_server, teardown, kind="service")
    errors: list[str] = []
    service.on("error", lambda env: errors.append(env["payload"]["code"]))
    with pytest.raises(FleetClientError) as e:
        await service.ice_config()
    assert e.value.code == "not_authorized"
    assert await service.ice_servers() == []
    assert service.state.value == "open"  # a refused request closes nothing
    assert errors == ["not_authorized", "not_authorized"]


async def test_an_older_server_answers_invalid_message_which_is_an_empty_list(ice_server, teardown):
    client = await connect(ice_server, teardown)
    refused = answer_like_an_older_server(client)
    with pytest.raises(FleetClientError) as e:
        await client.ice_config()
    assert e.value.code == "invalid_message"
    assert await client.ice_servers() == []
    assert len(refused) == 2


async def test_no_answer_in_time_and_not_connected(ice_server, teardown):
    client = await connect(ice_server, teardown)
    real = client._send_frame  # noqa: SLF001

    async def swallow(ws: Any, env: dict[str, Any]) -> None:
        if env["type"] != "ice.request":
            await real(ws, env)

    client._send_frame = swallow  # type: ignore[method-assign]  # noqa: SLF001
    t0 = time.monotonic()
    with pytest.raises(FleetClientError) as e:
        await client.ice_config(timeout=0.2)
    assert e.value.code == "timeout" and 0.15 < time.monotonic() - t0 < 1.0
    assert await client.ice_servers(timeout=0.1) == []
    assert client._ice_waiters == {}  # noqa: SLF001

    await client.close()
    with pytest.raises(FleetClientError) as e:
        await client.ice_config()
    assert e.value.code == "closed"
    assert await client.ice_servers() == []


async def test_a_request_out_when_the_connection_drops_fails_at_once(ice_server, teardown):
    client = FleetClient(ice_server.ws_url, kind="robot", name="ice-drop", enrollment_key=ice_server.enroll_key, reconnect=Backoff(initial=0.05, max=0.1))
    teardown.append(client)
    await client.connect()
    real = client._send_frame  # noqa: SLF001

    async def swallow_then_drop(ws: Any, env: dict[str, Any]) -> None:
        if env["type"] != "ice.request":
            return await real(ws, env)
        ws.transport.abort()

    client._send_frame = swallow_then_drop  # type: ignore[method-assign]  # noqa: SLF001
    t0 = time.monotonic()
    with pytest.raises(FleetClientError) as e:
        await client.ice_config(timeout=5)
    assert e.value.code == "network" and time.monotonic() - t0 < 2


# ---------------------------------------------------------------------- the robot's peer connection


async def leased(server: Any, teardown: list[Any], **robot_kw: Any) -> tuple[Robot, Any, str]:
    robot, _, _ = await start_robot(server, teardown, **robot_kw)
    op = await start_operator(server, teardown)
    lease = await op.claim(robot.robot_id)
    await wait_until(lambda: robot.lease_id == lease)
    return robot, op, lease


async def offer(op: Any, robot: Robot, lease: str, teardown: list[Any]) -> Any:
    """A scripted operator connects a twist channel to the robot; returns the offerer and the robot's answer."""
    from test_webrtc import Offerer

    link = Offerer(op, robot.robot_id, lease)
    teardown.append(link)
    await link.offer()
    sig = await link.answer()
    from aiortc import RTCSessionDescription

    await link.pc.setRemoteDescription(RTCSessionDescription(sdp=sig["data"]["sdp"], type="answer"))
    await asyncio.wait_for(link.opened.wait(), 10)
    await wait_until(lambda: robot.data_channel_open)
    return link, sig["data"]["sdp"]


@needs_webrtc
async def test_the_robot_gives_its_peer_the_installations_servers_and_reuses_them_under_the_lease(ice_server, teardown):
    robot, _, _ = await start_robot(ice_server, teardown)
    answerer = robot._peer  # noqa: SLF001
    assert answerer.ice_requests == 0 and robot.peer_ice_servers is None  # nothing is asked before there is a lease
    op = await start_operator(ice_server, teardown)
    lease = await op.claim(robot.robot_id)
    await wait_until(lambda: robot.lease_id == lease)
    assert answerer.ice_requests == 1  # asked on the grant, ahead of any offer

    seen = len(ice_server.stun_stub.usernames)
    link, _ = await offer(op, robot, lease, teardown)
    first = robot.peer_ice_servers
    assert_configured(first, ice_server, robot.robot_id)
    # Not only handed over: the peer connection went to that server with the robot's own credential.
    assert [u.split(":")[1] for u in ice_server.stun_stub.usernames[seen:]] == [robot.robot_id]
    assert await link.ping(7) == {"pong": 7}

    # A second session under the same lease: answered from what the robot was told on the grant.
    await offer(op, robot, lease, teardown)
    assert answerer.ice_requests == 1
    assert [(s.urls, s.username, s.credential) for s in robot.peer_ice_servers] == [(s.urls, s.username, s.credential) for s in first]


@needs_webrtc
async def test_nothing_configured_gives_the_peer_an_explicit_empty_list(fleet_server, teardown):
    robot, op, lease = await leased(fleet_server, teardown)
    _, sdp = await offer(op, robot, lease, teardown)
    assert robot.peer_ice_servers == []
    assert robot._peer.ice_requests == 1  # noqa: SLF001 - it asked; the answer was the empty list
    # With none configured the robot asked no STUN server of its own choosing:
    # every candidate in its answer is one of its own addresses.
    kinds = [line.split(" typ ")[1].split()[0] for line in sdp.splitlines() if line.startswith("a=candidate:")]
    assert kinds and set(kinds) == {"host"}


@needs_webrtc
async def test_an_older_server_means_an_empty_list_and_the_offer_is_still_answered(ice_server, teardown):
    robot, _, _ = await start_robot(ice_server, teardown)
    refused = answer_like_an_older_server(robot.client)
    op = await start_operator(ice_server, teardown)
    lease = await op.claim(robot.robot_id)
    await wait_until(lambda: robot.lease_id == lease)
    link, _ = await offer(op, robot, lease, teardown)
    assert robot.peer_ice_servers == []
    assert len(refused) >= 1
    assert await link.ping(3) == {"pong": 3}


@needs_webrtc
async def test_a_list_given_to_the_robot_is_used_as_given_and_the_server_is_never_asked(ice_server, teardown):
    # The empty list is a list: "none, whatever the server says".
    robot, op, lease = await leased(ice_server, teardown, ice_servers=[])
    await offer(op, robot, lease, teardown)
    assert robot.peer_ice_servers == []
    assert robot._peer.ice_requests == 0  # noqa: SLF001

    # A list of the robot's own: here a TURN server with a credential the fleet-server never signed.
    turn = {"urls": ice_server.turn_urls, "username": "9999999999:my-own", "credential": "not-the-servers"}
    other, op2, lease2 = await leased(ice_server, teardown, ice_servers=[turn])
    await offer(op2, other, lease2, teardown)
    assert [(s.urls, s.username, s.credential) for s in other.peer_ice_servers] == [
        (turn["urls"], turn["username"], turn["credential"])
    ]
    assert other._peer.ice_requests == 0  # noqa: SLF001
    assert "9999999999:my-own" in ice_server.stun_stub.usernames


@needs_webrtc
async def test_an_ice_server_that_never_replies_does_not_hold_the_answer_past_the_operators_timeout(fleet_server, teardown):
    from fleet.webrtc import GATHER_TIMEOUT_S

    # UDP port 9 on loopback: nothing answers, as with a STUN server that is down or unreachable.
    robot, op, lease = await leased(fleet_server, teardown, ice_servers=[{"urls": "stun:127.0.0.1:9"}])
    t0 = time.monotonic()
    link, _ = await offer(op, robot, lease, teardown)
    took = time.monotonic() - t0
    # The operator gives an offer five seconds; the channel is up well inside them.
    assert took < GATHER_TIMEOUT_S + 1.5, took
    assert await link.ping(9) == {"pong": 9}


@needs_webrtc
async def test_a_stored_answer_past_its_expiry_is_asked_for_again_when_the_offer_arrives(ice_server, teardown):
    robot, op, lease = await leased(ice_server, teardown)
    answerer = robot._peer  # noqa: SLF001
    await offer(op, robot, lease, teardown)
    assert answerer.ice_requests == 1
    expires_at_ms = (await robot.client.ice_config())["expires_at_ms"]

    # The robot's clock passes the credential's expiry.
    answerer._now_ms = lambda: expires_at_ms + 1  # noqa: SLF001
    link, _ = await offer(op, robot, lease, teardown)
    assert answerer.ice_requests == 2
    # This clock is ahead of the server's, so the new credential is itself
    # "expired" by it: the robot does not use one it believes is past its
    # time, and goes ahead with none rather than refuse the offer.
    assert robot.peer_ice_servers == []
    assert await link.ping(5) == {"pong": 5}

    # Back on the server's time, the next offer asks once more and uses the answer.
    answerer._now_ms = lambda: time.time() * 1000  # noqa: SLF001
    await offer(op, robot, lease, teardown)
    assert answerer.ice_requests == 2  # the answer of a moment ago is still good
    assert_configured(robot.peer_ice_servers, ice_server, robot.robot_id)


@needs_webrtc
async def test_a_lease_taken_from_the_welcome_asks_too(ice_server, teardown):
    """The robot process restarted while it was leased: only the welcome names the lease."""
    tokens = MemoryTokenStore()
    first, op, lease = await leased(ice_server, teardown, token_store=tokens)
    robot_id = first.robot_id
    await first.close()  # the server keeps the lease

    again, _, leases = await start_robot(ice_server, teardown, token_store=tokens)
    assert again.robot_id == robot_id and again.lease_id == lease
    assert leases and leases[-1].granted
    assert again._peer.ice_requests == 1  # noqa: SLF001
    await offer(op, again, lease, teardown)
    assert_configured(again.peer_ice_servers, ice_server, robot_id)
    assert again._peer.ice_requests == 1  # noqa: SLF001
