"""Channels: the opaque domain bus (docs/INTEGRATION.md section 2.6).

The platform never looks inside ``data``. Anything shaped like an order, a
waypoint list or an edge report rides on a channel. A :class:`Channel` is a
thin view over one channel name on a :class:`~fleet.client.FleetClient`, so any
kind of client (robot, service, operator) can use it::

    assignment = client.channel("assignment")

    async def on_assignment(sender: str, data):   # sender is stamped by the server
        await assignment.publish({"ack": data["seq"]}, to=sender)

    assignment.on_message(on_assignment)           # directed + broadcast messages
    await assignment.publish(report)               # to=None: broadcast (sender excluded)
    await assignment.publish(job, to=robot_id)     # directed; raises ChannelTargetNotFound
                                                   # if that client is not connected

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
  receipt; the target can still drop off right after. If you need one, put a
  sequence number in ``data`` and have the receiver reply on the channel.

The error normally arrives within one round trip, so the window only costs
time on success. Pass ``wait=0`` for fire-and-forget (high-rate publishes).
Broadcasts default to ``wait=0``: they have no target that can be missing.

Acked sends (a request that waits for the receiver's reply on the channel) are
not part of v0. They can be added on top of this API without changing it:
``publish`` already takes an ``id`` and returns the envelope, and handlers get
the server-stamped sender to reply to.
"""

from __future__ import annotations

import asyncio
import logging
import re
import uuid
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import TYPE_CHECKING, Any, Union

from .client import ConnectionState, Envelope, FleetClientError, StateChange

if TYPE_CHECKING:
    from .client import FleetClient

__all__ = [
    "CHANNEL_NAME_PATTERN",
    "DEFAULT_NOT_FOUND_WINDOW",
    "Channel",
    "ChannelHandler",
    "ChannelMessage",
    "ChannelTargetNotFound",
    "channel",
]

log = logging.getLogger("fleet.channel")

#: Channel names as the protocol allows them (defs.schema.json#/$defs/channelName).
CHANNEL_NAME_PATTERN = re.compile(r"^[a-z0-9][a-z0-9_.-]{0,63}$")

#: Seconds a directed publish waits for a ``not_found`` reply before returning.
DEFAULT_NOT_FOUND_WINDOW = 0.5

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
        handlers = self._handlers.get(name) if isinstance(name, str) else None
        if not handlers:
            return
        msg = ChannelMessage(channel=name, sender=str(p.get("from", "")), data=p.get("data"), ts_ms=env.get("ts_ms"))
        for h in list(handlers):
            self._client._call(_bind(h), msg)

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
