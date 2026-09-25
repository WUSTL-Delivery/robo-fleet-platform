"""FleetClient against the real fleet-server binary (the ``fleet_server`` fixture)."""

from __future__ import annotations

import asyncio
import json
import stat
import urllib.request
from typing import Any

import pytest

from fleet import (
    Backoff,
    ConnectionState,
    Credentials,
    FileTokenStore,
    FleetClient,
    FleetClientError,
    MemoryTokenStore,
    StateChange,
)

FAST = Backoff(initial=0.02, max=0.2)


class RecordingClient(FleetClient):
    """Records every frame the client writes and every session socket it opens."""

    def __init__(self, *args: Any, **kwargs: Any) -> None:
        super().__init__(*args, **kwargs)
        self.sent: list[dict[str, Any]] = []
        self.sockets: list[Any] = []

    async def _send_frame(self, ws: Any, env: dict[str, Any]) -> None:
        self.sent.append(env)
        if env["type"] == "hello":
            self.sockets.append(ws)
        await super()._send_frame(ws, env)

    def sent_types(self, type_: str) -> list[dict[str, Any]]:
        return [e for e in self.sent if e["type"] == type_]


@pytest.fixture
async def clients():
    made: list[FleetClient] = []
    yield made
    for c in made:
        await c.close()


def make(clients: list[FleetClient], server, cls=FleetClient, **kw: Any) -> Any:
    kw.setdefault("reconnect", FAST)
    c = cls(server.ws_url, **kw)
    clients.append(c)
    return c


async def wait_for_state(client: FleetClient, want: ConnectionState, timeout: float = 5.0) -> StateChange:
    fut: asyncio.Future[StateChange] = asyncio.get_running_loop().create_future()

    def handler(change: StateChange) -> None:
        if change.state is want and not fut.done():
            fut.set_result(change)

    off = client.on_state(handler)
    try:
        return await asyncio.wait_for(fut, timeout)
    finally:
        off()


async def test_robot_enrolls_once_heartbeats_and_reconnects_with_same_client_id(fleet_server, clients, tmp_path):
    store = FileTokenStore(tmp_path / "robot.json")
    robot: RecordingClient = make(
        clients,
        fleet_server,
        cls=RecordingClient,
        kind="robot",
        name="py-robot-01",
        enrollment_key=fleet_server.enroll_key,
        token_store=store,
        agent={"name": "sdk-python-test", "version": "0.0.1"},
    )
    states: list[ConnectionState] = []
    robot.on_state(lambda c: states.append(c.state))

    welcome = await robot.connect()
    assert welcome["kind"] == "robot"
    assert welcome["client_id"].startswith("r_")
    assert welcome["heartbeat_interval_ms"] == fleet_server.heartbeat_interval_ms
    saved = store.load()
    assert saved is not None and saved.client_id == welcome["client_id"]
    assert states == [ConnectionState.ENROLLING, ConnectionState.CONNECTING, ConnectionState.OPEN]

    # Outlast ~2.5 missed intervals several times over: only heartbeats keep us online.
    await asyncio.sleep(fleet_server.heartbeat_interval_ms * 5 / 1000)
    assert robot.state is ConnectionState.OPEN
    assert states == [ConnectionState.ENROLLING, ConnectionState.CONNECTING, ConnectionState.OPEN]
    assert len(robot.sent_types("heartbeat")) >= 3

    # Kill the live session socket out from under the client (no close handshake).
    reopened = asyncio.ensure_future(wait_for_state(robot, ConnectionState.OPEN))
    robot.sockets[-1].transport.abort()
    again = await reopened

    assert again.welcome is not None and again.welcome["client_id"] == welcome["client_id"]
    assert robot.client_id == welcome["client_id"]
    assert ConnectionState.RECONNECTING in states
    # Exactly one enroll ever; one hello per connection, all with the stored token.
    assert len(robot.sent_types("enroll.request")) == 1
    hellos = robot.sent_types("hello")
    assert len(hellos) == 2
    assert {h["payload"]["token"] for h in hellos} == {saved.token}
    assert all(h["payload"]["agent"] == {"name": "sdk-python-test", "version": "0.0.1"} for h in hellos)

    # And it is usable again: a robot-only message goes through without an error reply.
    errors: list[dict[str, Any]] = []
    robot.on("error", errors.append)
    await robot.send("telemetry", {"battery": {"pct": 90.0}})
    await asyncio.sleep(0.2)
    assert errors == []


async def test_reuses_stored_credentials_without_an_enrollment_key(fleet_server, clients, tmp_path):
    path = tmp_path / "creds" / "robot.json"
    first = make(clients, fleet_server, kind="robot", enrollment_key=fleet_server.enroll_key, token_store=FileTokenStore(path))
    w1 = await first.connect()
    await first.close()
    assert first.state is ConnectionState.CLOSED
    assert stat.S_IMODE(path.stat().st_mode) == 0o600

    # A new process: same file, no key.
    second: RecordingClient = make(clients, fleet_server, cls=RecordingClient, kind="robot", token_store=FileTokenStore(path))
    w2 = await second.connect()
    assert w2["client_id"] == w1["client_id"]
    assert second.sent_types("enroll.request") == []


async def test_delivers_inbound_messages_and_sends_envelopes(fleet_server, clients):
    client = make(clients, fleet_server, kind="service", enrollment_key=fleet_server.enroll_key)
    await client.connect()
    loop = asyncio.get_running_loop()
    snapshot: asyncio.Future[dict[str, Any]] = loop.create_future()
    client.on("snapshot", lambda env: snapshot.done() or snapshot.set_result(env["payload"]))
    seen: list[str] = []
    client.on_message(lambda env: seen.append(env["type"]))

    async def async_handler(env: dict[str, Any]) -> None:  # coroutine handlers are scheduled as tasks
        seen.append("async:" + env["type"])

    client.on("snapshot", async_handler)

    sent = await client.send("subscribe", {"topics": ["presence"]}, id="sub-1")
    assert sent["v"] == 0 and sent["type"] == "subscribe" and sent["id"] == "sub-1"
    assert "robots" in await asyncio.wait_for(snapshot, 5)
    await asyncio.sleep(0)
    assert "snapshot" in seen and "async:snapshot" in seen


async def test_error_reply_carries_ref_and_keeps_the_connection(fleet_server, clients):
    client = make(clients, fleet_server, kind="service", enrollment_key=fleet_server.enroll_key)
    await client.connect()
    err: asyncio.Future[dict[str, Any]] = asyncio.get_running_loop().create_future()
    client.on("error", lambda env: err.done() or err.set_result(env["payload"]))
    await client.send("telemetry", {}, id="t-1")  # robot-only message from a service
    payload = await asyncio.wait_for(err, 5)
    assert payload["code"] == "not_authorized" and payload["ref"] == "t-1"
    await asyncio.sleep(0.1)
    assert client.state is ConnectionState.OPEN


async def test_bad_token_is_terminal(fleet_server, clients):
    store = MemoryTokenStore(Credentials(token="fp-tk-not-a-real-token", client_id="s_x", fleet_id="f_x"))
    client: RecordingClient = make(clients, fleet_server, cls=RecordingClient, kind="service", token_store=store)
    with pytest.raises(FleetClientError) as info:
        await client.connect()
    assert info.value.code == "auth_failed"
    assert client.state is ConnectionState.CLOSED
    assert len(client.sent_types("hello")) == 1  # not retried
    assert store.load() is not None  # the client never clears the store itself


async def test_bad_enrollment_key_is_terminal(fleet_server, clients):
    client = make(clients, fleet_server, kind="robot", enrollment_key="not-the-key")
    with pytest.raises(FleetClientError) as info:
        await client.connect()
    assert info.value.code == "auth_failed"
    assert client.state is ConnectionState.CLOSED


async def test_operator_invite_is_single_use(fleet_server, clients):
    def mint_invite() -> str:
        req = urllib.request.Request(
            f"{fleet_server.http_url}/api/admin/fleets/{fleet_server.fleet}/operator-invites",
            method="POST",
            headers={"Authorization": f"Bearer {fleet_server.admin_token}"},
        )
        with urllib.request.urlopen(req) as res:
            return json.load(res)["key"]

    key = await asyncio.to_thread(mint_invite)
    op = make(clients, fleet_server, kind="operator", name="alice", enrollment_key=key)
    welcome = await op.connect()
    assert welcome["kind"] == "operator" and welcome["client_id"].startswith("o_")

    again = make(clients, fleet_server, kind="operator", enrollment_key=key)
    with pytest.raises(FleetClientError):
        await again.connect()
    assert again.state is ConnectionState.CLOSED


async def test_takeover_by_same_identity_is_terminal_not_ping_pong(fleet_server, clients):
    store = MemoryTokenStore()
    first = make(clients, fleet_server, kind="robot", enrollment_key=fleet_server.enroll_key, token_store=store)
    w1 = await first.connect()
    closed = asyncio.ensure_future(wait_for_state(first, ConnectionState.CLOSED))
    second = make(clients, fleet_server, kind="robot", token_store=store)
    w2 = await second.connect()
    assert w2["client_id"] == w1["client_id"]
    change = await closed
    assert change.error is not None and change.error.code == "conflict"
    await asyncio.sleep(0.3)
    assert second.state is ConnectionState.OPEN


async def test_send_refused_while_not_open_and_close_is_idempotent(fleet_server, clients):
    client = make(clients, fleet_server, kind="service", enrollment_key=fleet_server.enroll_key)
    with pytest.raises(FleetClientError) as info:
        await client.send("heartbeat", {})
    assert info.value.code == "closed"
    await client.connect()
    await client.close()
    await client.close()
    assert client.state is ConnectionState.CLOSED
    assert (await client.wait_closed()).code == "closed"
    with pytest.raises(FleetClientError):
        await client.send("heartbeat", {})


async def test_close_during_backoff_stops_reconnecting(clients):
    # Nothing listens on this port: every attempt fails with a network error and retries.
    client = FleetClient("ws://127.0.0.1:9/ws", kind="robot", enrollment_key="k", reconnect=Backoff(initial=0.05, max=0.05))
    clients.append(client)
    connecting = asyncio.ensure_future(client.connect())
    await wait_for_state(client, ConnectionState.RECONNECTING)
    await client.close()
    with pytest.raises(FleetClientError) as info:
        await connecting
    assert info.value.code == "closed"
    assert client.state is ConnectionState.CLOSED
