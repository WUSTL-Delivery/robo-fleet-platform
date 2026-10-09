"""Channels: the opaque domain bus (docs/INTEGRATION.md section 2.6).

The platform never looks inside ``data``. Anything shaped like an order, a
waypoint list or an edge report rides on a channel. A :class:`Channel` is a
thin view over one channel name on a :class:`~fleet.client.FleetClient`, so any
kind of client (robot, service, operator) can use it::

    status = client.channel("status")

    def on_status(sender: str, data):              # sender is stamped by the server
        print(sender, data)

    status.on_message(on_status)                   # directed + broadcast messages
    await status.publish(report)                   # to=None: broadcast (sender excluded)
    await status.publish(note, to=robot_id)        # directed; raises ChannelTargetNotFound
                                                   # if that client is not connected
    client.channel("jobs").on_acked(on_job)        # acked receive, see below

Receiving. Directed messages reach a client whether or not it subscribed, and
so do broadcasts to a robot whose manifest lists the channel. Everyone else
needs ``subscribe {topics: ["channel:<name>"]}`` to receive broadcasts, so the
client subscribes to ``channel:<name>`` for every channel with at least one
handler: right away when the connection is open, and again on every
(re)connect, because the server forgets subscriptions with the connection.
Removing the last handler stops the re-subscribe; v0 has no unsubscribe
message, so the current connection keeps receiving until it drops (and the
messages are dropped client-side).

Publishing and the not_found window. Delivery is at-most-once (DESIGN.md D4)
and the server sends nothing back when a publish succeeds. It answers a
directed publish to a client that is not connected (or not in this fleet) with
``error {code: "not_found", ref: <publish id>}``. So a directed ``publish``
sends with an id and then waits up to ``wait`` seconds (default
:data:`DEFAULT_NOT_FOUND_WINDOW`, 0.5 s) for an error carrying that ref:

- ``not_found`` arrives: raises :class:`ChannelTargetNotFound`.
- any other error with that ref (e.g. ``invalid_message``, payload too large):
  raises :class:`~fleet.client.FleetClientError` with the server's code.
- nothing within the window: returns the sent envelope. That is NOT a delivery
  receipt; the target can still drop off right after. If you need one, the
  sender uses an acked send and the receiver ``on_acked`` (below).

The error normally arrives within one round trip, so the window only costs
time on success. Pass ``wait=0`` for fire-and-forget (high-rate publishes).
Broadcasts default to ``wait=0``: they have no target that can be missing.

Acked receive. The acked-send convention (protocol/README.md, "Acked send
(convention on channel data)", the single reference) lets a sender learn that
a directed message was accepted: it publishes ``{"seq": n, "data": ...}`` and
re-sends until the receiver answers ``{"ack": n}`` on the same channel. This
module implements the receiving half::

    def on_job(sender: str, data):       # data is the inner value; seq is not shown
        queue.put_nowait(data)           # returning without error accepts it

    jobs.on_acked(on_job)

For each ``(channel, sender, seq)``:

- first copy: the handler runs. When it returns (or its coroutine finishes)
  without raising, the SDK publishes ``{"ack": seq}`` to the sender. If it
  raises, nothing is sent and the key is forgotten, so the sender's next
  re-send runs the handler again;
- a repeat while the handler is still running: dropped, no ack yet;
- a repeat after it was accepted: the handler is not called again, and the ack
  is published again, once per copy received.

An ack means accepted, not finished: return well inside the sender's re-send
interval (1 s by default) and do long work afterwards, reporting its result as
ordinary channel data. An ack that cannot be sent (link down, or the sender
gone) is dropped; the sender's next re-send is answered.

Accepted keys are remembered for :data:`ACKED_MEMORY_SECONDS` (10 minutes)
after they were first received, in memory only. So a message is handled exactly
once within one process lifetime; a receiver that restarts forgets, and a
re-send arriving after that is handled again. Data whose effect must not happen
twice needs its own identity inside the inner ``data``.

Only data of exactly the acked-message shape (an object with just ``seq`` and
``data``, ``seq`` an integer in 1..:data:`MAX_SEQ`) goes to the acked handler,
and only on a channel that has one. Such a message is then not shown to
``on_message`` handlers. Everything else on the channel, including ``{"ack":
n}`` replies, is ordinary data for ``on_message``. Acked messages are directed,
and directed messages need no subscription, so ``on_acked`` does not subscribe.
The sending half is not in this SDK yet.
"""

from __future__ import annotations

import asyncio
import inspect
import logging
import re
import time
import uuid
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any, Union

from .client import ConnectionState, Envelope, FleetClientError, StateChange

if TYPE_CHECKING:
    from .client import FleetClient

__all__ = [
    "ACKED_MEMORY_SECONDS",
    "CHANNEL_NAME_PATTERN",
    "DEFAULT_NOT_FOUND_WINDOW",
    "Channel",
    "ChannelHandler",
    "ChannelMessage",
    "ChannelTargetNotFound",
    "MAX_SEQ",
    "channel",
]

log = logging.getLogger("fleet.channel")

#: Channel names as the protocol allows them (defs.schema.json#/$defs/channelName).
CHANNEL_NAME_PATTERN = re.compile(r"^[a-z0-9][a-z0-9_.-]{0,63}$")

#: Seconds a directed publish waits for a ``not_found`` reply before returning.
DEFAULT_NOT_FOUND_WINDOW = 0.5

#: Seconds an accepted acked message is remembered after it was first received
#: (protocol/README.md, Acked send, Receiver: at least 10 minutes).
ACKED_MEMORY_SECONDS = 600.0

#: Largest valid acked-send ``seq`` (2^53 - 1); the smallest is 1.
MAX_SEQ = 2**53 - 1

#: ``handler(sender, data)``; a coroutine function is scheduled as its own task.
ChannelHandler = Callable[[str, Any], Union[None, Awaitable[None]]]


class ChannelTargetNotFound(FleetClientError):
    """A directed publish named a client that is not connected (or not in this fleet)."""

    def __init__(self, channel: str, to: str, message: str = "channel target not connected") -> None:
        super().__init__("not_found", f"{message} (channel {channel!r}, to {to!r})")
        self.channel = channel
        self.to = to


@dataclass(frozen=True)
class ChannelMessage:
    """One inbound ``channel.message``."""

    channel: str
    #: Sender's client id, stamped by the server. Reply with ``publish(..., to=sender)``.
    sender: str
    data: Any
    ts_ms: int | None = None


@dataclass
class _Seen:
    """One seen-set entry: in progress until the handler accepts, then done."""

    first: float
    done: bool = field(default=False)


def _acked_seq(data: Any) -> int | None:
    """The seq if ``data`` is exactly an acked message ``{seq, data}``, else None."""
    if not isinstance(data, dict) or len(data) != 2 or "seq" not in data or "data" not in data:
        return None
    return _valid_seq(data["seq"])


def _ack_seq(data: Any) -> int | None:
    """The seq if ``data`` is exactly an ack ``{ack}``, else None."""
    if not isinstance(data, dict) or len(data) != 1 or "ack" not in data:
        return None
    return _valid_seq(data["ack"])


def _valid_seq(seq: Any) -> int | None:
    # bool is an int in Python but true/false are not JSON integers.
    if isinstance(seq, int) and not isinstance(seq, bool) and 1 <= seq <= MAX_SEQ:
        return seq
    return None


def channel(client: FleetClient, name: str) -> Channel:
    """Returns the :class:`Channel` view of ``name`` on ``client`` (same as ``client.channel(name)``)."""
    return Channel(client, name)


class Channel:
    """A view of one channel on one client. Cheap to create; all state lives on
    the client, so two ``Channel`` objects for the same name share handlers and
    the subscription."""

    def __init__(self, client: FleetClient, name: str) -> None:
        if not isinstance(name, str) or not CHANNEL_NAME_PATTERN.match(name):
            raise ValueError(f"invalid channel name {name!r}: must match {CHANNEL_NAME_PATTERN.pattern}")
        self._client = client
        self._hub = _hub(client)
        self.name = name

    def __repr__(self) -> str:
        return f"Channel({self.name!r})"

    @property
    def topic(self) -> str:
        """The subscribe topic for this channel's broadcasts."""
        return f"channel:{self.name}"

    def on_message(self, handler: ChannelHandler) -> Callable[[], None]:
        """Calls ``handler(sender, data)`` for every message on this channel,
        directed or broadcast, and keeps ``channel:<name>`` subscribed across
        reconnects while any handler is registered. Returns a function that
        removes the handler."""
        return self._hub.add_handler(self.name, handler)

    def on_acked(self, handler: ChannelHandler) -> Callable[[], None]:
        """Receives acked sends on this channel: calls ``handler(sender, data)``
        with the inner ``data`` once per ``(sender, seq)``, and publishes
        ``{"ack": seq}`` back to the sender when the handler returns (or its
        coroutine finishes) without raising, and again for every later repeat.
        A handler that raises is logged, sends no ack, and sees the sender's
        next re-send as new. See the module docs. Returns a function that
        removes the handler.

        Raises:
            ValueError: this channel already has an acked handler. There is one
                per channel because one handler's outcome decides the ack.
        """
        return self._hub.set_acked_handler(self.name, handler)

    async def publish(
        self,
        data: Any,
        to: str | None = None,
        *,
        id: str | None = None,
        wait: float | None = None,
    ) -> Envelope:
        """Sends ``data`` to client ``to``, or broadcasts it when ``to`` is None
        (the sender is excluded). Returns the sent envelope.

        Args:
            data: any JSON-serializable value; the platform never inspects it.
            to: a connected client id in this fleet, or None to broadcast.
            id: envelope id; error replies carry it as ``ref``. Generated when
                omitted and a wait is needed.
            wait: seconds to wait for an error reply (see the module docs).
                Default: :data:`DEFAULT_NOT_FOUND_WINDOW` for a directed send,
                0 for a broadcast.

        Raises:
            ChannelTargetNotFound: ``to`` is not connected.
            FleetClientError: not connected (``closed``), or the server
                rejected the publish (its error code).
        """
        payload: dict[str, Any] = {"channel": self.name, "data": data}
        if to is None:
            payload["broadcast"] = True
        elif isinstance(to, str) and to:
            payload["to"] = to
        else:
            raise ValueError(f"to must be a client id or None, got {to!r}")
        if wait is None:
            wait = DEFAULT_NOT_FOUND_WINDOW if to is not None else 0.0
        if wait <= 0:
            return await self._client.send("channel.publish", payload, id=id)

        ref = id if id is not None else f"ch-{uuid.uuid4().hex[:12]}"
        reply = self._hub.expect_error(ref)
        try:
            env = await self._client.send("channel.publish", payload, id=ref)
            try:
                err = await asyncio.wait_for(asyncio.shield(reply), wait)
            except asyncio.TimeoutError:
                return env
        finally:
            self._hub.forget_error(ref)
        code = str(err.get("code", "protocol"))
        message = str(err.get("message", ""))
        if code == "not_found" and to is not None:
            raise ChannelTargetNotFound(self.name, to, message or "channel target not connected")
        raise FleetClientError(code, f"channel.publish on {self.name!r}: {message}")

    async def subscribe(self, timeout: float = 10.0) -> dict[str, Any]:
        """Subscribes to this channel's broadcasts now and returns the snapshot
        that answers it. Only needed to receive broadcasts without a handler or
        to know the subscription is in place; ``on_message`` subscribes by itself.
        Note: this subscription is not renewed on reconnect unless a handler is
        registered."""
        return await self._hub.subscribe([self.topic], timeout)


class _ChannelHub:
    """Per-client channel state: handlers by name, pending publish refs, and
    the re-subscribe on every OPEN."""

    def __init__(self, client: FleetClient) -> None:
        self._client = client
        self._handlers: dict[str, list[ChannelHandler]] = {}
        self._acked_handlers: dict[str, ChannelHandler] = {}
        # Seen set of the acked-send receiver, keyed (channel, from, seq). Dicts keep
        # insertion order, which is first-received order, so expiry scans from the front.
        self._seen: dict[tuple[str, str, int], _Seen] = {}
        self._now: Callable[[], float] = time.monotonic
        self._pending_errors: dict[str, asyncio.Future[dict[str, Any]]] = {}
        self._snapshot_waiters: list[asyncio.Future[dict[str, Any]]] = []
        self._tasks: set[asyncio.Task[Any]] = set()
        client.on("channel.message", self._on_channel_message)
        client.on("error", self._on_error)
        client.on("snapshot", self._on_snapshot)
        client.on_state(self._on_state)

    # -- handlers + subscription

    def add_handler(self, name: str, handler: ChannelHandler) -> Callable[[], None]:
        handlers = self._handlers.setdefault(name, [])
        first = not handlers
        handlers.append(handler)
        if first and self._client.state is ConnectionState.OPEN:
            self._spawn(self._send_subscribe([f"channel:{name}"]))

        def remove() -> None:
            try:
                handlers.remove(handler)
            except ValueError:
                pass

        return remove

    def _topics(self) -> list[str]:
        return [f"channel:{n}" for n, hs in self._handlers.items() if hs]

    def _on_state(self, change: StateChange) -> None:
        if change.state is ConnectionState.OPEN:
            topics = self._topics()
            if topics:
                self._spawn(self._send_subscribe(topics))
        elif change.state is ConnectionState.CLOSED:
            err = change.error or FleetClientError("closed", "client closed")
            for fut in self._snapshot_waiters:
                if not fut.done():
                    fut.set_exception(err)
            self._snapshot_waiters.clear()

    async def _send_subscribe(self, topics: list[str]) -> None:
        try:
            await self._client.send("subscribe", {"topics": topics})
        except FleetClientError as e:
            # Dropped between OPEN and here; the next OPEN subscribes again.
            log.debug("channel subscribe %s not sent: %s", topics, e)

    async def subscribe(self, topics: list[str], timeout: float) -> dict[str, Any]:
        fut: asyncio.Future[dict[str, Any]] = asyncio.get_running_loop().create_future()
        self._snapshot_waiters.append(fut)
        try:
            await self._client.send("subscribe", {"topics": topics})
            return await asyncio.wait_for(fut, timeout)
        finally:
            if fut in self._snapshot_waiters:
                self._snapshot_waiters.remove(fut)

    def _on_snapshot(self, env: Envelope) -> None:
        # The server answers subscribes in order, so the oldest waiter gets the next snapshot.
        while self._snapshot_waiters:
            fut = self._snapshot_waiters.pop(0)
            if not fut.done():
                fut.set_result(env["payload"])
                return

    def _on_channel_message(self, env: Envelope) -> None:
        p = env["payload"]
        name = p.get("channel")
        if not isinstance(name, str):
            return
        sender = str(p.get("from", ""))
        data = p.get("data")
        acked = self._acked_handlers.get(name)
        if acked is not None and sender:
            seq = _acked_seq(data)
            if seq is not None:
                self._receive_acked(name, sender, seq, data["data"], acked)
                return
        handlers = self._handlers.get(name)
        if not handlers:
            return
        msg = ChannelMessage(channel=name, sender=sender, data=data, ts_ms=env.get("ts_ms"))
        for h in list(handlers):
            self._client._call(_bind(h), msg)

    # -- acked receive (protocol/README.md, Acked send, Receiver)

    def set_acked_handler(self, name: str, handler: ChannelHandler) -> Callable[[], None]:
        if name in self._acked_handlers:
            raise ValueError(f"channel {name!r} already has an acked handler")
        self._acked_handlers[name] = handler

        def remove() -> None:
            if self._acked_handlers.get(name) is handler:
                del self._acked_handlers[name]

        return remove

    def _receive_acked(self, name: str, sender: str, seq: int, inner: Any, handler: ChannelHandler) -> None:
        now = self._now()
        self._forget_old(now)
        key = (name, sender, seq)
        entry = self._seen.get(key)
        if entry is not None:
            if entry.done:  # accepted earlier: never redeliver, ack every copy
                self._client._call(lambda _: self._send_ack(name, sender, seq), None)
            return  # in progress: drop the repeat, no ack yet
        entry = self._seen[key] = _Seen(first=now)
        try:
            result = handler(sender, inner)
        except Exception:
            log.exception("acked handler for channel %r raised; seq %d from %s not acked", name, seq, sender)
            self._forget(key, entry)
            return
        if inspect.isawaitable(result):
            self._client._call(lambda _: self._finish_acked(key, entry, result), None)
        else:
            entry.done = True
            self._client._call(lambda _: self._send_ack(name, sender, seq), None)

    async def _finish_acked(self, key: tuple[str, str, int], entry: _Seen, result: Awaitable[None]) -> None:
        name, sender, seq = key
        try:
            await result
        except asyncio.CancelledError:
            self._forget(key, entry)
            raise
        except Exception:
            log.exception("acked handler for channel %r raised; seq %d from %s not acked", name, seq, sender)
            self._forget(key, entry)
            return
        entry.done = True
        await self._send_ack(name, sender, seq)

    async def _send_ack(self, name: str, sender: str, seq: int) -> None:
        # Once per received copy, never retried: a lost ack is answered on the next re-send.
        try:
            await self._client.send("channel.publish", {"channel": name, "to": sender, "data": {"ack": seq}})
        except FleetClientError as e:
            log.debug("ack %d to %s on channel %r not sent: %s", seq, sender, name, e)

    def _forget(self, key: tuple[str, str, int], entry: _Seen) -> None:
        if self._seen.get(key) is entry:
            del self._seen[key]

    def _forget_old(self, now: float) -> None:
        expired = []
        for key, entry in self._seen.items():
            if now - entry.first <= ACKED_MEMORY_SECONDS:
                break
            if entry.done:  # an in-progress key stays until its handler settles
                expired.append(key)
        for key in expired:
            del self._seen[key]

    # -- error replies by ref

    def expect_error(self, ref: str) -> asyncio.Future[dict[str, Any]]:
        fut: asyncio.Future[dict[str, Any]] = asyncio.get_running_loop().create_future()
        self._pending_errors[ref] = fut
        return fut

    def forget_error(self, ref: str) -> None:
        fut = self._pending_errors.pop(ref, None)
        if fut is not None and not fut.done():
            fut.cancel()

    def _on_error(self, env: Envelope) -> None:
        ref = env["payload"].get("ref")
        fut = self._pending_errors.get(ref) if isinstance(ref, str) else None
        if fut is not None and not fut.done():
            fut.set_result(env["payload"])

    # -- tasks

    def _spawn(self, coro: Awaitable[None]) -> None:
        task = asyncio.ensure_future(coro)
        self._tasks.add(task)
        task.add_done_callback(self._tasks.discard)


def _bind(handler: ChannelHandler) -> Callable[[ChannelMessage], Any]:
    def call(msg: ChannelMessage) -> Any:
        return handler(msg.sender, msg.data)

    call.__qualname__ = getattr(handler, "__qualname__", repr(handler))
    return call


def _hub(client: FleetClient) -> _ChannelHub:
    hub = getattr(client, "_fleet_channel_hub", None)
    if hub is None:
        hub = _ChannelHub(client)
        client._fleet_channel_hub = hub  # type: ignore[attr-defined]
    return hub
