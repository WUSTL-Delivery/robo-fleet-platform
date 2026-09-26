"""FleetClient: the connection core every Python client is built on.

It owns exactly the identity + liveness part of the wire protocol
(docs/INTEGRATION.md sections 2.2 and 2.3):

1. Enroll ONCE: if the token store is empty, open a one-shot socket, send
   ``enroll.request {enrollment_key, kind, name}`` and persist the
   ``enroll.response`` credentials.
2. Hello on EVERY connect: ``hello {token, agent}`` -> ``welcome``.
3. Heartbeat every ``welcome.heartbeat_interval_ms`` while open.
4. Reconnect with exponential backoff + jitter using the SAME token. It never
   re-enrolls on reconnect (that would mint a new identity).

Everything else (manifest, telemetry, leases, twist, channels, subscribe) is
layered on top through a small surface::

    await client.send(type, payload, id=None)  one outbound envelope; raises unless open
    client.on(type, handler)                   inbound envelopes of one type
    client.on_message(handler)                 every inbound envelope
    client.on_state(handler)                   connection-state changes; OPEN carries the
                                               welcome and fires on EVERY (re)connect, which
                                               is where a layer re-sends its manifest or
                                               re-subscribes
    client.state / client.welcome / client.client_id

Each ``on*`` call returns a function that unregisters the handler. Handlers may
be plain functions or coroutine functions; a coroutine is scheduled as its own
task, so a handler can ``await client.send(...)``. A handler that raises is
logged and never breaks the connection loop or the other handlers.

Reconnect policy: only transient failures retry (network drop, server restart,
handshake timeout, heartbeat lapse = ``rate_limited``). Any other server
refusal is terminal and closes the client: ``auth_failed`` (bad or revoked
token, including one revoked while connected; bad enrollment key),
``conflict`` (another connection took over this identity, so reconnecting
would just kick it back and ping-pong; also a reused invite key), and the
rest. The client never clears the token store by itself.

From synchronous code, run it with ``asyncio.run(client.run_forever())``.
"""

from __future__ import annotations

import asyncio
import enum
import inspect
import json
import logging
import random
import time
from collections.abc import Awaitable, Callable, Mapping
from dataclasses import dataclass
from typing import TYPE_CHECKING, Any, Literal, Union

from websockets.asyncio.client import ClientConnection, connect as ws_connect
from websockets.exceptions import ConnectionClosed, InvalidURI, WebSocketException
from websockets.protocol import State as WsState

from .token_store import Credentials, MemoryTokenStore, TokenStore

if TYPE_CHECKING:
    from .channel import Channel

__all__ = [
    "PROTOCOL_VERSION",
    "Backoff",
    "ClientKind",
    "ConnectionState",
    "Envelope",
    "FleetClient",
    "FleetClientError",
    "StateChange",
]

log = logging.getLogger("fleet.client")

PROTOCOL_VERSION = 0

ClientKind = Literal["robot", "service", "operator"]

#: One wire envelope as a dict: ``{"v": 0, "type": ..., "id"?: ..., "ts_ms"?: ..., "payload": {...}}``.
Envelope = dict[str, Any]

Handler = Callable[[Any], Union[None, Awaitable[None]]]

#: Failure codes worth retrying; everything else closes the client.
RETRYABLE = frozenset({"network", "timeout", "rate_limited"})


class ConnectionState(str, enum.Enum):
    """Where the client is in its lifecycle.

    - IDLE: constructed, connect() not called yet
    - ENROLLING: exchanging the enrollment key for a token (first run only)
    - CONNECTING: socket opening / hello sent, waiting for welcome
    - OPEN: welcome received, heartbeating; send() works
    - RECONNECTING: connection lost, waiting out the backoff delay
    - CLOSED: terminal; close() was called or the server refused us
    """

    IDLE = "idle"
    ENROLLING = "enrolling"
    CONNECTING = "connecting"
    OPEN = "open"
    RECONNECTING = "reconnecting"
    CLOSED = "closed"


class FleetClientError(Exception):
    """An error the client surfaces. ``code`` is the server's error code when
    there is one (``auth_failed``, ``conflict``, ``rate_limited``, ...), else one
    of ``network``, ``timeout``, ``closed``, ``protocol``."""

    def __init__(self, code: str, message: str) -> None:
        super().__init__(f"{code}: {message}")
        self.code = code
        self.message = message


@dataclass(frozen=True)
class StateChange:
    """One connection-state transition, as handed to ``on_state`` handlers."""

    state: ConnectionState
    #: Set when state is OPEN: the welcome payload for this connection.
    welcome: dict[str, Any] | None = None
    #: Set when state is RECONNECTING: delay before the next attempt (seconds) and its 1-based number.
    retry_in: float | None = None
    attempt: int | None = None
    #: Why the previous connection ended or why the client closed, when known.
    error: FleetClientError | None = None


@dataclass(frozen=True)
class Backoff:
    """Reconnect delays: ``initial * factor**(n-1)`` capped at ``max``, jittered into [d/2, d]."""

    initial: float = 0.25
    max: float = 10.0
    factor: float = 2.0

    def delay(self, attempt: int) -> float:
        base = min(self.max, self.initial * self.factor ** (attempt - 1))
        return base / 2 + random.random() * (base / 2)


class FleetClient:
    """An asyncio connection to fleet-server for one identity."""

    def __init__(
        self,
        url: str,
        *,
        kind: ClientKind,
        name: str | None = None,
        enrollment_key: str | None = None,
        token_store: TokenStore | None = None,
        agent: Mapping[str, Any] | None = None,
        reconnect: Backoff | None = Backoff(),
        handshake_timeout: float = 10.0,
    ) -> None:
        """
        Args:
            url: the server's WebSocket endpoint, e.g. ``ws://localhost:8080/ws``.
            kind: client kind to enroll as (only used when enrolling).
            name: display name sent with ``enroll.request``.
            enrollment_key: fleet enrollment key (robot/service) or single-use invite
                key (operator). Needed only when the store holds no token.
            token_store: where credentials persist. Default: in memory.
            agent: ``{"name": ..., "version": ...}`` sent with enroll and every hello.
            reconnect: backoff tuning, or None to never reconnect after a drop.
            handshake_timeout: seconds to wait for the socket, ``welcome`` or
                ``enroll.response``.
        """
        self.url = url
        self.kind = kind
        self.name = name
        self._enrollment_key = enrollment_key
        self._store: TokenStore = token_store if token_store is not None else MemoryTokenStore()
        self._agent = dict(agent) if agent is not None else None
        self._backoff = reconnect
        self._handshake_timeout = handshake_timeout

        self._state = ConnectionState.IDLE
        self._welcome: dict[str, Any] | None = None
        self._credentials: Credentials | None = None
        self._ws: ClientConnection | None = None
        self._run_task: asyncio.Task[None] | None = None
        self._heartbeat_task: asyncio.Task[None] | None = None
        self._first_welcome: asyncio.Future[dict[str, Any]] | None = None
        self._closed_event: asyncio.Event | None = None
        self._close_error: FleetClientError | None = None
        self._attempt = 0

        self._typed: dict[str, list[Handler]] = {}
        self._any: list[Handler] = []
        self._state_handlers: list[Handler] = []
        self._handler_tasks: set[asyncio.Task[Any]] = set()

    # ------------------------------------------------------------------ properties

    @property
    def state(self) -> ConnectionState:
        return self._state

    @property
    def welcome(self) -> dict[str, Any] | None:
        """The welcome payload of the current (or last) connection."""
        return self._welcome

    @property
    def client_id(self) -> str | None:
        """This client's id: from the latest welcome, else from the store."""
        if self._welcome is not None:
            return self._welcome.get("client_id")
        return self._credentials.client_id if self._credentials else None

    @property
    def close_error(self) -> FleetClientError | None:
        """Why the client closed, once it has."""
        return self._close_error

    # ------------------------------------------------------------------ lifecycle

    async def connect(self) -> dict[str, Any]:
        """Enrolls if needed, then connects; returns the first welcome payload.

        Raises FleetClientError on a terminal failure (auth_failed, conflict, ...)
        or if close() is called before the first welcome. Network failures before
        the first welcome are retried with backoff. After that, reconnects happen
        in the background; watch on_state. Calling it again awaits the same
        first welcome.
        """
        if self._first_welcome is None:
            loop = asyncio.get_running_loop()
            self._first_welcome = loop.create_future()
            if self._closed_event is None:
                self._closed_event = asyncio.Event()
            if self._state is ConnectionState.CLOSED:
                self._first_welcome.set_exception(FleetClientError("closed", "client is closed"))
            else:
                self._run_task = asyncio.create_task(self._run(), name=f"fleet-client:{self.name or self.kind}")
        return await asyncio.shield(self._first_welcome)

    async def close(self) -> None:
        """Closes the connection and stops reconnecting. Terminal and idempotent."""
        await self._terminate(FleetClientError("closed", "client closed"))

    async def wait_closed(self) -> FleetClientError | None:
        """Waits until the client is closed; returns why."""
        if self._closed_event is None:
            self._closed_event = asyncio.Event()
            if self._state is ConnectionState.CLOSED:
                self._closed_event.set()
        await self._closed_event.wait()
        return self._close_error

    async def run_forever(self) -> None:
        """Connects, then stays connected until close() or a terminal failure,
        which is raised. For sync code: ``asyncio.run(client.run_forever())``."""
        await self.connect()
        err = await self.wait_closed()
        if err is not None and err.code != "closed":
            raise err

    # ------------------------------------------------------------------ messaging

    async def send(self, type: str, payload: Mapping[str, Any], *, id: str | None = None) -> Envelope:
        """Sends one envelope and returns it (with ``ts_ms`` filled in).

        Raises FleetClientError("closed") unless the state is OPEN.
        """
        ws = self._ws
        if self._state is not ConnectionState.OPEN or ws is None:
            raise FleetClientError("closed", f"cannot send {type}: connection is {self._state.value}")
        env = envelope(type, payload, id)
        try:
            await self._send_frame(ws, env)
        except ConnectionClosed as e:
            raise FleetClientError("closed", f"cannot send {type}: socket closed") from e
        return env

    def on(self, type: str, handler: Handler) -> Callable[[], None]:
        """Calls ``handler(envelope)`` for every inbound envelope of ``type``."""
        return _add(self._typed.setdefault(type, []), handler)

    def on_message(self, handler: Handler) -> Callable[[], None]:
        """Calls ``handler(envelope)`` for every inbound envelope."""
        return _add(self._any, handler)

    def on_state(self, handler: Handler) -> Callable[[], None]:
        """Calls ``handler(StateChange)`` on every connection-state change."""
        return _add(self._state_handlers, handler)

    def channel(self, name: str) -> Channel:
        """A view of domain channel ``name``: ``publish`` / ``on_message``. See fleet/channel.py."""
        from .channel import Channel

        return Channel(self, name)

    # ------------------------------------------------------------------ internals

    async def _send_frame(self, ws: ClientConnection, env: Envelope) -> None:
        """The single write path for every frame (tests hook it to record traffic)."""
        await ws.send(json.dumps(env, separators=(",", ":")))

    async def _run(self) -> None:
        try:
            while self._state is not ConnectionState.CLOSED:
                err = await self._attempt_once()
                if self._state is ConnectionState.CLOSED:
                    return
                if err.code not in RETRYABLE or self._backoff is None:
                    await self._terminate(err)
                    return
                self._attempt += 1
                delay = self._backoff.delay(self._attempt)
                self._set_state(
                    StateChange(ConnectionState.RECONNECTING, retry_in=delay, attempt=self._attempt, error=err)
                )
                await asyncio.sleep(delay)
        except asyncio.CancelledError:
            raise
        except Exception as e:  # a bug, not a network condition: do not spin
            log.exception("fleet client loop crashed")
            await self._terminate(FleetClientError("protocol", f"client loop crashed: {e!r}"))

    async def _attempt_once(self) -> FleetClientError:
        """One enroll-if-needed + session. Returns why it ended."""
        try:
            creds = self._credentials or self._store.load()
            if creds is None:
                self._set_state(StateChange(ConnectionState.ENROLLING))
                creds = await self._enroll()
                if self._state is ConnectionState.CLOSED:
                    return FleetClientError("closed", "client closed")
                self._store.save(creds)
            self._credentials = creds
            return await self._session(creds.token)
        except FleetClientError as e:
            return e
        except asyncio.TimeoutError:
            return FleetClientError("timeout", "timed out opening the socket")
        except InvalidURI as e:
            return FleetClientError("protocol", f"bad url: {e}")  # retrying cannot fix it
        except (OSError, WebSocketException) as e:
            return FleetClientError("network", f"{type(e).__name__}: {e}")

    async def _enroll(self) -> Credentials:
        """One-shot enroll socket: enroll.request -> enroll.response; the server then closes it."""
        key = self._enrollment_key
        if not key:
            raise FleetClientError("auth_failed", "no stored token and no enrollment_key to enroll with")
        payload: dict[str, Any] = {"enrollment_key": key, "kind": self.kind}
        if self.name is not None:
            payload["name"] = self.name
        if self._agent is not None:
            payload["agent"] = self._agent
        async with ws_connect(self.url, open_timeout=self._handshake_timeout) as ws:
            await self._send_frame(ws, envelope("enroll.request", payload))
            try:
                raw = await asyncio.wait_for(ws.recv(), self._handshake_timeout)
            except asyncio.TimeoutError:
                raise FleetClientError("timeout", "enroll: no response") from None
            except ConnectionClosed:
                raise FleetClientError("network", "enroll: socket closed before response") from None
        env = parse_envelope(raw)
        if env is None:
            raise FleetClientError("protocol", "enroll: unparseable response")
        p = env["payload"]
        if env["type"] == "enroll.response":
            return Credentials(token=p["token"], client_id=p["client_id"], fleet_id=p["fleet_id"])
        if env["type"] == "error":
            raise FleetClientError(str(p.get("code")), f"enroll: {p.get('message', '')}")
        raise FleetClientError("protocol", f"enroll: unexpected {env['type']}")

    async def _session(self, token: str) -> FleetClientError:
        """hello -> welcome -> heartbeat, until the socket drops. Returns why it ended."""
        self._set_state(StateChange(ConnectionState.CONNECTING))
        ws = await ws_connect(self.url, open_timeout=self._handshake_timeout)
        if self._state is ConnectionState.CLOSED:
            await ws.close()
            return FleetClientError("closed", "client closed")
        self._ws = ws
        last_error: FleetClientError | None = None
        try:
            hello: dict[str, Any] = {"token": token}
            if self._agent is not None:
                hello["agent"] = self._agent
            await self._send_frame(ws, envelope("hello", hello))

            loop = asyncio.get_running_loop()
            deadline = loop.time() + self._handshake_timeout
            welcomed = False
            while not welcomed:
                try:
                    raw = await asyncio.wait_for(ws.recv(), max(0.0, deadline - loop.time()))
                except asyncio.TimeoutError:
                    return FleetClientError("timeout", "no welcome after hello")
                env = parse_envelope(raw)
                if env is None:
                    continue
                if env["type"] == "welcome":
                    welcomed = True
                    self._on_welcome(ws, env["payload"])
                elif env["type"] == "error":
                    # Before welcome, any error is the handshake's answer.
                    last_error = _error_from(env)
                self._dispatch(env)

            async for raw in ws:
                env = parse_envelope(raw)
                if env is None:
                    continue
                if env["type"] == "error":
                    # After welcome, errors are replies to our sends and do not
                    # close the socket, except these three, sent right before
                    # close (auth_failed: this client's token was revoked).
                    code = env["payload"].get("code")
                    if code in ("conflict", "rate_limited", "auth_failed"):
                        last_error = _error_from(env)
                self._dispatch(env)
        except ConnectionClosed:
            pass
        finally:
            self._stop_heartbeat()
            if self._ws is ws:
                self._ws = None
            if ws.state is not WsState.CLOSED:
                if self._state is ConnectionState.CLOSED:
                    ws.transport.abort()  # close() is already tearing down; do not block it
                else:
                    await ws.close()
        if last_error is not None:
            return last_error
        reason = f": {ws.close_reason}" if ws.close_reason else ""
        return FleetClientError("network", f"socket closed ({ws.close_code}{reason})")

    def _on_welcome(self, ws: ClientConnection, welcome: dict[str, Any]) -> None:
        self._welcome = welcome
        self._attempt = 0
        self._stop_heartbeat()
        interval = float(welcome.get("heartbeat_interval_ms", 10_000)) / 1000
        self._heartbeat_task = asyncio.create_task(self._heartbeat(ws, interval), name="fleet-client:heartbeat")
        self._set_state(StateChange(ConnectionState.OPEN, welcome=welcome))
        first = self._first_welcome
        if first is not None and not first.done():
            first.set_result(welcome)

    async def _heartbeat(self, ws: ClientConnection, interval: float) -> None:
        try:
            while True:
                await asyncio.sleep(interval)
                await self._send_frame(ws, envelope("heartbeat", {}))
        except ConnectionClosed:
            return

    def _stop_heartbeat(self) -> None:
        task = self._heartbeat_task
        self._heartbeat_task = None
        if task is not None and task is not asyncio.current_task():
            task.cancel()

    async def _terminate(self, err: FleetClientError) -> None:
        if self._state is ConnectionState.CLOSED:
            return
        self._state = ConnectionState.CLOSED  # handlers are told below, after cleanup
        self._close_error = err
        self._stop_heartbeat()
        ws = self._ws
        self._ws = None
        if ws is not None:
            try:
                await ws.close(1000, "client closed")
            except Exception:  # already gone
                pass
        task = self._run_task
        if task is not None and task is not asyncio.current_task() and not task.done():
            task.cancel()
            try:
                await task
            except asyncio.CancelledError:
                pass
        self._set_state(StateChange(ConnectionState.CLOSED, error=err))
        first = self._first_welcome
        if first is not None and not first.done():
            first.set_exception(err)
            first.exception()  # mark retrieved: a caller that only uses on_state gets no warning
        if self._closed_event is not None:
            self._closed_event.set()

    def _set_state(self, change: StateChange) -> None:
        self._state = change.state
        for h in list(self._state_handlers):
            self._call(h, change)

    def _dispatch(self, env: Envelope) -> None:
        for h in list(self._typed.get(env["type"], ())):
            self._call(h, env)
        for h in list(self._any):
            self._call(h, env)

    def _call(self, handler: Handler, arg: Any) -> None:
        """A raising handler must not break the others or the connection loop."""
        try:
            result = handler(arg)
        except Exception:
            log.exception("fleet client handler %r raised", handler)
            return
        if inspect.isawaitable(result):
            task = asyncio.ensure_future(result)
            self._handler_tasks.add(task)
            task.add_done_callback(self._handler_done)

    def _handler_done(self, task: asyncio.Future[Any]) -> None:
        self._handler_tasks.discard(task)  # type: ignore[arg-type]
        if not task.cancelled() and task.exception() is not None:
            log.error("fleet client handler raised", exc_info=task.exception())


# ---------------------------------------------------------------------- helpers


def envelope(type: str, payload: Mapping[str, Any], id: str | None = None) -> Envelope:
    """Builds one v0 envelope."""
    env: Envelope = {"v": PROTOCOL_VERSION, "type": type, "ts_ms": int(time.time() * 1000), "payload": dict(payload)}
    if id is not None:
        env["id"] = id
    return env


def parse_envelope(raw: str | bytes) -> Envelope | None:
    """Parses one inbound frame; None unless it is a v0 envelope with an object payload."""
    try:
        v = json.loads(raw)
    except (ValueError, UnicodeDecodeError):
        return None
    if not isinstance(v, dict) or v.get("v") != PROTOCOL_VERSION or not isinstance(v.get("type"), str):
        return None
    if not isinstance(v.get("payload"), dict):
        return None
    return v


def _error_from(env: Envelope) -> FleetClientError:
    p = env["payload"]
    return FleetClientError(str(p.get("code", "protocol")), str(p.get("message", "")))


def _add(handlers: list[Handler], handler: Handler) -> Callable[[], None]:
    handlers.append(handler)

    def remove() -> None:
        try:
            handlers.remove(handler)
        except ValueError:
            pass

    return remove
