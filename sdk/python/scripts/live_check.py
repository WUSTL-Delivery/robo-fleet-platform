#!/usr/bin/env python3
"""Live check: prove a robot can come online at a deployed fleet-server.

Runs two throwaway clients against FLEET_URL and checks, step by step:

  1. the host resolves and the server answers on its WebSocket port
  2. a robot enrolls with FLEET_ENROLL_KEY, connects and sends its manifest
  3. a watcher (kind=service) connects and subscribes to presence
  4. the watcher sees the robot online (snapshot or robot.online event)
  5. the watcher sends the robot a channel message; the robot prints it and replies
  6. the watcher receives the reply
  7. the robot closes and the watcher sees robot.offline

Only uses what fleet-server 0.1.0 supports (enroll, hello, heartbeat, manifest,
subscribe/snapshot, presence events, channel.publish), so it works against the
club deployment as well as newer servers.

Environment:
  FLEET_ENROLL_KEY     required: the fleet enrollment key. Never printed.
  FLEET_URL            default wss://fleet.bearcarts.com/ws
  FLEET_SERVICE_TOKEN  optional: an existing service token for the watcher. When
                       unset, the watcher enrolls as a second throwaway client
                       (kind=service) with the same enrollment key.
  FLEET_STEP_TIMEOUT   seconds per step, default 15

Credentials live in memory only; nothing is written to disk. Each run leaves
one or two client rows on the server (live-check-robot-<id>,
live-check-watcher-<id>); v0 has no way to delete them.

Exit status: 0 when every step passes, 1 when a step fails, 2 on bad config.
"""

from __future__ import annotations

import asyncio
import os
import secrets
import socket
import ssl
import sys
import time
from pathlib import Path
from typing import Any
from urllib.parse import urlparse

# Run from a checkout without installing the package.
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from fleet import (  # noqa: E402
    ChannelTargetNotFound,
    Credentials,
    FleetClient,
    FleetClientError,
    MemoryTokenStore,
    Robot,
)

DEFAULT_URL = "wss://fleet.bearcarts.com/ws"
CHANNEL = "live-check"
AGENT = {"name": "fleet-sdk-python/live_check", "version": "0.0.1"}

BLOCKED_HINT = "your network may be blocking the domain; try a hotspot (or another network) and run again"

OK = "✓"
FAIL = "✗"


class StepFailed(Exception):
    def __init__(self, message: str, hint: str | None = None) -> None:
        super().__init__(message)
        self.hint = hint


def ok(msg: str) -> None:
    print(f"  {OK} {msg}", flush=True)


def info(msg: str) -> None:
    print(f"      {msg}", flush=True)


def step(n: int, title: str) -> None:
    print(f"[{n}/7] {title}", flush=True)


def hint_for(err: FleetClientError) -> str | None:
    if err.code == "auth_failed":
        return "the server rejected the credentials: check FLEET_ENROLL_KEY (or FLEET_SERVICE_TOKEN) matches the deployment"
    if err.code in ("network", "timeout"):
        return "the host resolved but the WebSocket did not open: check the URL path (/ws), the port, and that the server is up"
    return None


async def within(timeout: float, what: str, aw: Any) -> Any:
    try:
        return await asyncio.wait_for(aw, timeout)
    except asyncio.TimeoutError:
        raise StepFailed(f"timed out after {timeout:g}s waiting for {what}") from None
    except FleetClientError as e:
        raise StepFailed(f"{what}: {e}", hint_for(e)) from None


async def canonical_name(host: str) -> str | None:
    """The resolver's canonical name for host (follows CNAMEs); None if unavailable."""
    try:
        name, _, _ = await asyncio.get_running_loop().run_in_executor(None, socket.gethostbyname_ex, host)
    except OSError:
        return None
    return name


async def preflight(url: str, timeout: float) -> None:
    """DNS + TCP reachability, so a sinkholed domain fails with a clear hint."""
    u = urlparse(url)
    if u.scheme not in ("ws", "wss") or not u.hostname:
        raise StepFailed(f"FLEET_URL must be ws:// or wss://host/..., got {url!r}")
    host = u.hostname
    port = u.port or (443 if u.scheme == "wss" else 80)
    loop = asyncio.get_running_loop()
    try:
        infos = await asyncio.wait_for(loop.getaddrinfo(host, port, type=socket.SOCK_STREAM), timeout)
    except (socket.gaierror, asyncio.TimeoutError) as e:
        raise StepFailed(
            f"could not resolve {host}: {e}",
            BLOCKED_HINT,
        ) from None
    addrs = sorted({i[4][0] for i in infos})
    canonical = await canonical_name(host)
    via = f" (via CNAME {canonical})" if canonical and canonical.rstrip(".").lower() != host.lower() else ""
    ok(f"{host} resolves to {', '.join(addrs)}{via}")
    # A DNS sinkhole answers with its own name (e.g. sinkhole.paloaltonetworks.com)
    # or a null/loopback address instead of failing, so resolving is not enough.
    if canonical and "sinkhole" in canonical.lower():
        raise StepFailed(
            f"{host} is answered by a DNS sinkhole ({canonical}), not the fleet server",
            BLOCKED_HINT,
        )
    local = host == "localhost" or host.startswith("127.")
    if not local and any(a in ("0.0.0.0", "::", "::1") or a.startswith("127.") for a in addrs):
        raise StepFailed(f"{host} resolves to a null/loopback address ({', '.join(addrs)})", BLOCKED_HINT)

    tls = u.scheme == "wss"
    try:
        _, writer = await asyncio.wait_for(asyncio.open_connection(host, port, ssl=True if tls else None), timeout)
    except ConnectionRefusedError as e:
        raise StepFailed(
            f"TCP connect to {host}:{port} refused: {e}",
            "nothing is listening there: check the URL's host and port, and that the server is up",
        ) from None
    except ssl.SSLCertVerificationError as e:
        hint = BLOCKED_HINT
        if "local issuer" in str(e.verify_message):
            # Common on python.org macOS installs that never ran Install Certificates.
            hint = (
                "this Python may have no CA certificates. On macOS run "
                "'/Applications/Python 3.x/Install Certificates.command', or retry with "
                "SSL_CERT_FILE=/etc/ssl/cert.pem. If `curl -I https://" + host + "` also fails, "
                "your network may be intercepting TLS; try a hotspot"
            )
        raise StepFailed(f"TLS certificate for {host} did not verify: {e.verify_message}", hint) from None
    except (OSError, asyncio.TimeoutError) as e:
        what = "TLS handshake with" if tls else "TCP connect to"
        raise StepFailed(f"{what} {host}:{port} failed: {e!r}", BLOCKED_HINT) from None
    writer.close()
    try:
        await writer.wait_closed()
    except OSError:
        pass
    ok(f"{'TLS' if tls else 'TCP'} {host}:{port} reachable")


async def run(url: str, key: str, service_token: str | None, timeout: float) -> None:
    tag = secrets.token_hex(3)
    robot_name = f"live-check-robot-{tag}"
    watcher_name = f"live-check-watcher-{tag}"

    step(1, f"resolve and reach {url}")
    await preflight(url, timeout)

    # reconnect=None: a live check should fail loudly, not retry forever.
    robot = Robot(
        url,
        manifest={"channels": [CHANNEL]},
        name=robot_name,
        enrollment_key=key,
        token_store=MemoryTokenStore(),
        agent=AGENT,
        reconnect=None,
    )
    if service_token:
        watcher_store = MemoryTokenStore(Credentials(token=service_token, client_id="", fleet_id=""))
        watcher = FleetClient(url, kind="service", token_store=watcher_store, agent=AGENT, reconnect=None)
    else:
        watcher = FleetClient(
            url,
            kind="service",
            name=watcher_name,
            enrollment_key=key,
            token_store=MemoryTokenStore(),
            agent=AGENT,
            reconnect=None,
        )

    robot_got: asyncio.Future[tuple[str, Any]] = asyncio.get_running_loop().create_future()
    reply_got: asyncio.Future[tuple[str, Any]] = asyncio.get_running_loop().create_future()
    events: asyncio.Queue[dict[str, Any]] = asyncio.Queue()
    snapshots: asyncio.Queue[dict[str, Any]] = asyncio.Queue()

    robot_ch = robot.channel(CHANNEL)

    async def on_robot_message(sender: str, data: Any) -> None:
        print(f"      robot handler: channel {CHANNEL!r} from {sender}: {data}", flush=True)
        if not robot_got.done():
            robot_got.set_result((sender, data))
        await robot_ch.publish({"reply_to": data.get("seq") if isinstance(data, dict) else None, "from": robot_name}, to=sender)

    robot_ch.on_message(on_robot_message)

    watcher.on("event", lambda env: events.put_nowait(env["payload"]))
    watcher.on("snapshot", lambda env: snapshots.put_nowait(env["payload"]))

    try:
        step(2, f"robot enrolls and connects as {robot_name}")
        welcome = await within(timeout, "robot enroll + welcome", robot.connect())
        robot_id = welcome["client_id"]
        ok(f"robot online: client_id={robot_id} fleet_id={welcome.get('fleet_id')}")
        ok(f"manifest sent: {robot.manifest}")

        step(3, "watcher connects and subscribes to presence")
        how = "FLEET_SERVICE_TOKEN" if service_token else f"throwaway service {watcher_name}"
        w = await within(timeout, "watcher welcome", watcher.connect())
        ok(f"watcher connected ({how}): client_id={w['client_id']} kind={w.get('kind')}")
        if w.get("fleet_id") != welcome.get("fleet_id"):
            raise StepFailed(
                f"watcher is in fleet {w.get('fleet_id')} but the robot is in {welcome.get('fleet_id')}",
                "FLEET_SERVICE_TOKEN belongs to a different fleet than FLEET_ENROLL_KEY",
            )
        await watcher.send("subscribe", {"topics": ["presence"]})

        step(4, "watcher sees the robot online")

        async def wait_online() -> str:
            while True:
                snap_task = asyncio.ensure_future(snapshots.get())
                ev_task = asyncio.ensure_future(events.get())
                done, pending = await asyncio.wait({snap_task, ev_task}, return_when=asyncio.FIRST_COMPLETED)
                for t in pending:
                    t.cancel()
                if snap_task in done:
                    for r in snap_task.result().get("robots", []):
                        if r.get("robot_id") == robot_id and r.get("presence") == "online":
                            return f"snapshot lists {robot_id} presence=online (name={r.get('name')})"
                if ev_task in done:
                    ev = ev_task.result()
                    if ev.get("event") == "robot.online" and ev.get("robot_id") == robot_id:
                        return f"event robot.online robot_id={robot_id}"

        ok(await within(timeout, "robot online in snapshot or robot.online event", wait_online()))

        step(5, "watcher sends the robot a channel message")
        watcher_ch = watcher.channel(CHANNEL)

        def on_reply(sender: str, data: Any) -> None:
            if sender == robot_id and not reply_got.done():
                reply_got.set_result((sender, data))

        watcher_ch.on_message(on_reply)
        sent = {"seq": 1, "hello": "live check", "at_ms": int(time.time() * 1000)}
        info(f"publishing on {CHANNEL!r} to {robot_id}: {sent}")
        try:
            await within(timeout, "channel.publish to the robot", watcher_ch.publish(sent, to=robot_id))
        except ChannelTargetNotFound as e:
            raise StepFailed(f"server says the robot is not connected: {e}") from None
        ok("server accepted the publish (no not_found)")
        sender, data = await within(timeout, "the robot's handler to receive it", robot_got)
        ok(f"robot received it from {sender}")

        step(6, "watcher receives the robot's reply")
        _, reply = await within(timeout, "the robot's reply", reply_got)
        ok(f"reply from {robot_id}: {reply}")

        step(7, "robot closes; watcher sees robot.offline")
        await robot.close()
        ok("robot closed")

        async def wait_offline() -> str:
            while True:
                ev = await events.get()
                if ev.get("event") == "robot.offline" and ev.get("robot_id") == robot_id:
                    return f"event robot.offline robot_id={robot_id}"

        ok(await within(timeout, "robot.offline event", wait_offline()))
    finally:
        await robot.close()
        await watcher.close()


def main() -> int:
    url = os.environ.get("FLEET_URL", DEFAULT_URL).strip()
    key = os.environ.get("FLEET_ENROLL_KEY", "").strip()
    service_token = os.environ.get("FLEET_SERVICE_TOKEN", "").strip() or None
    try:
        timeout = float(os.environ.get("FLEET_STEP_TIMEOUT", "15"))
    except ValueError:
        print(f"{FAIL} FLEET_STEP_TIMEOUT must be a number of seconds", file=sys.stderr)
        return 2
    if not key:
        print(f"{FAIL} FLEET_ENROLL_KEY is not set (export it from the club deploy's secrets)", file=sys.stderr)
        return 2

    print(f"fleet live check against {url}", flush=True)
    print(f"  enrollment key: set ({len(key)} chars, not shown)", flush=True)
    try:
        asyncio.run(run(url, key, service_token, timeout))
    except StepFailed as e:
        print(f"  {FAIL} {e}", flush=True)
        if e.hint:
            print(f"      hint: {e.hint}", flush=True)
        print("LIVE CHECK FAILED", flush=True)
        return 1
    except KeyboardInterrupt:
        print(f"  {FAIL} interrupted", flush=True)
        return 1
    print("LIVE CHECK PASSED: robot online, channel message round trip, robot offline", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
