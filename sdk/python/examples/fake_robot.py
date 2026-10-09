"""A fake robot for trying a fleet-server by hand: it enrolls, shows up on the console map,
wanders while autonomous, drives under an operator's WASD, drains a battery, and
accepts acked sends on the "jobs" channel (it prints each job and the SDK acks it).

Run it with ./run_fake_robot.sh (loads examples/.env), or directly:

    FLEET_URL=ws://localhost:8080/ws FLEET_ENROLL_KEY=... python fake_robot.py --name fake-01

While it runs, type a command and press Enter:
    h  ask for help (the robot goes to HELP_REQUESTED in the console)
    p  pause / resume wandering
    q  quit

With the SDK's webrtc extra installed (pip install -e '..[webrtc]'), it also answers the
operator's WebRTC offer and takes twist from the data channel; it prints which transport
the operator's twist is arriving on. Without the extra it is driven over the bus.

The first run enrolls with FLEET_ENROLL_KEY and saves a token to ~/.fleet/<name>.json;
later runs reuse that token and come back as the same robot, with no key needed.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import math
import os
import random
import sys
import time

from fleet import FileTokenStore, Robot, TwistCommand, geo_pose

M_PER_DEG_LAT = 111_320.0


def parse_args() -> argparse.Namespace:
    env = os.environ.get
    p = argparse.ArgumentParser(description="Fake robot for a fleet-server.")
    p.add_argument("--url", default=env("FLEET_URL", "ws://localhost:8080/ws"))
    p.add_argument("--name", default=env("FLEET_NAME", "fake-robot-01"))
    p.add_argument("--token-file", default=env("FLEET_TOKEN_FILE"), help="default ~/.fleet/<name>.json")
    p.add_argument("--origin", default=env("FAKE_ORIGIN", "38.6488,-90.3108"), help="lat,lon to start near")
    p.add_argument("--radius", type=float, default=float(env("FAKE_RADIUS_M", "80")), help="wander radius, m")
    p.add_argument("--speed", type=float, default=float(env("FAKE_SPEED_MPS", "0.8")), help="wander speed, m/s")
    p.add_argument("--hz", type=float, default=float(env("FAKE_TELEMETRY_HZ", "2")), help="telemetry rate")
    p.add_argument("--help-after", type=float, default=float(env("FAKE_HELP_AFTER_S", "0")),
                   help="ask for help after N seconds (0 = never)")
    p.add_argument("--no-wander", action="store_true", help="sit still unless an operator drives")
    p.add_argument("--no-data-channel", action="store_true",
                   help="never answer a WebRTC offer; twist over the bus only")
    p.add_argument("--ice-servers", default=env("FLEET_ICE_SERVERS", ""),
                   help='STUN/TURN servers as JSON, e.g. \'[{"urls": "stun:stun.example.org:3478"}]\', '
                        "to use instead of the fleet-server's. Default: ask the server for the "
                        "installation's (none configured there means host candidates only)")
    return p.parse_args()


class Body:
    """Kinematics of a diff-drive robot on a flat patch around an origin (ENU, yaw 0 = east)."""

    def __init__(self, lat0: float, lon0: float) -> None:
        self.lat0, self.lon0 = lat0, lon0
        self.x = random.uniform(-20, 20)  # metres east of the origin
        self.y = random.uniform(-20, 20)  # metres north
        self.yaw = random.uniform(-math.pi, math.pi)
        self.v = 0.0
        self.w = 0.0
        self.battery = 100.0

    def step(self, dt: float) -> None:
        self.yaw = (self.yaw + self.w * dt + math.pi) % (2 * math.pi) - math.pi
        self.x += self.v * math.cos(self.yaw) * dt
        self.y += self.v * math.sin(self.yaw) * dt
        self.battery = max(0.0, self.battery - dt * (0.004 + 0.02 * abs(self.v)))

    def pose(self) -> dict:
        lat = self.lat0 + self.y / M_PER_DEG_LAT
        lon = self.lon0 + self.x / (M_PER_DEG_LAT * math.cos(math.radians(self.lat0)))
        return geo_pose(lat, lon, yaw_rad=self.yaw)


async def main() -> None:
    args = parse_args()
    lat0, lon0 = (float(s) for s in args.origin.split(","))
    body = Body(lat0, lon0)
    wander = not args.no_wander

    robot = Robot(
        args.url,
        name=args.name,
        manifest={
            "drive": {"type": "twist", "max_v_mps": 1.5, "max_w_radps": 2.0},
            "battery": {},
            "cameras": [{"id": "front", "label": "Front camera (fake)"}],
            "channels": ["jobs"],
        },
        enrollment_key=os.environ.get("FLEET_ENROLL_KEY") or None,
        token_store=FileTokenStore(args.token_file or f"~/.fleet/{args.name}.json"),
        # None: answer WebRTC offers if aiortc is installed, stay on the bus if it is not.
        data_channel=False if args.no_data_channel else None,
        ice_servers=json.loads(args.ice_servers) if args.ice_servers else None,
    )
    via = None  # the transport the operator's twist last arrived on

    def on_twist(cmd: TwistCommand) -> None:
        # Operator setpoints, or a zero from the SDK (deadman, revoke, lost link).
        nonlocal via
        body.v, body.w = cmd.linear_x, cmd.angular_z
        if cmd.source != "operator":
            print(f"  stop ({cmd.source})")
        elif cmd.via != via:
            via = cmd.via
            print("  twist over the WebRTC data channel" if via == "p2p" else "  twist over the bus")

    def on_lease(change) -> None:
        nonlocal via
        via = None
        if change.granted:
            print(f"  operator {change.operator_id} took over")
        else:
            print(f"  control handed back ({change.reason})")
            body.v = body.w = 0.0

    robot.on_twist(on_twist)
    robot.on_lease(on_lease)

    jobs = robot.channel("jobs")

    def on_job(sender: str, data: object) -> None:
        # An acked send: called once per job even when the sender re-sends it. Returning
        # accepts the job, and the SDK then replies {"ack": seq} to the sender.
        print(f"  job from {sender}: {data}")

    jobs.on_acked(on_job)
    jobs.on_message(lambda sender, data: print(f"  message from {sender}: {data}"))  # plain data

    try:
        await robot.connect()
    except Exception as e:  # noqa: BLE001 - print something useful, not a traceback
        print(f"could not connect to {args.url}: {e}")
        if "certificate" in str(e).lower():
            print("hint: set SSL_CERT_FILE=/etc/ssl/cert.pem (python.org Python on macOS has no CA bundle)")
        if "enroll" in str(e).lower() or "auth" in str(e).lower():
            print("hint: first run needs FLEET_ENROLL_KEY; a revoked robot needs a new --name")
        return
    print(f"{args.name} online as {robot.robot_id} at {args.url}")
    print("data channel:", "answers WebRTC offers" if robot.data_channel_enabled
          else "off (bus twist only)" if args.no_data_channel
          else "off (bus twist only; install the webrtc extra to turn it on)")
    print("commands: h = ask for help, p = pause/resume wandering, q = quit")

    stop = asyncio.Event()
    started = time.monotonic()
    helped = False

    def on_stdin() -> None:
        nonlocal wander
        raw = sys.stdin.readline()
        if raw == "":  # EOF (Ctrl-D): stop listening, keep running
            asyncio.get_running_loop().remove_reader(sys.stdin)
            return
        line = raw.strip().lower()
        if line in ("q", "quit"):
            stop.set()
        elif line == "h":
            asyncio.ensure_future(ask_help("operator requested from keyboard"))
        elif line == "p":
            wander = not wander
            print("  wandering", "on" if wander else "paused")

    async def ask_help(reason: str) -> None:
        try:
            await robot.request_help(reason)
            print(f"  asked for help: {reason}")
        except Exception as e:  # noqa: BLE001
            print(f"  help request failed: {e}")

    if sys.stdin.isatty():
        asyncio.get_running_loop().add_reader(sys.stdin, on_stdin)

    async def simulate() -> None:
        nonlocal helped
        dt = 1.0 / args.hz
        target_turn = 0.0
        while not stop.is_set():
            if robot.mode == "teleop":
                pass  # body.v / body.w come from on_twist
            elif wander and body.battery > 0:
                # Drift toward a new turn rate now and then; head home near the edge.
                if random.random() < 0.1:
                    target_turn = random.uniform(-0.4, 0.4)
                if math.hypot(body.x, body.y) > args.radius:
                    home = math.atan2(-body.y, -body.x)
                    err = (home - body.yaw + math.pi) % (2 * math.pi) - math.pi
                    target_turn = max(-0.8, min(0.8, err))
                body.v, body.w = args.speed, target_turn
            else:
                body.v = body.w = 0.0

            body.step(dt)
            await robot.telemetry(
                pose=body.pose(),
                battery=round(body.battery, 1),
                velocity={"v_mps": round(body.v, 3), "w_radps": round(body.w, 3)},
            )
            if args.help_after and not helped and time.monotonic() - started > args.help_after:
                helped = True
                await ask_help(f"simulated fault after {args.help_after:.0f}s")
            await asyncio.sleep(dt)

    sim = asyncio.create_task(simulate())
    runner = asyncio.create_task(robot.run_forever())
    await asyncio.wait([asyncio.create_task(stop.wait()), runner], return_when=asyncio.FIRST_COMPLETED)
    sim.cancel()
    await robot.close()
    if runner.done() and robot.client.close_error:
        print(f"disconnected: {robot.client.close_error}")
    print("bye")


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        print("\nbye")
