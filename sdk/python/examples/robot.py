"""A robot with no hardware: enrolls once, reports a pose at 1 Hz, prints teleop.
Env: FLEET_URL (default ws://localhost:8080/ws), FLEET_ENROLL_KEY (first run only),
FLEET_NAME (default example-robot), FLEET_TOKEN_FILE (default ~/.fleet/<name>.json)."""

import asyncio
import math
import os

from fleet import FileTokenStore, Robot, local_pose

URL = os.environ.get("FLEET_URL", "ws://localhost:8080/ws")
NAME = os.environ.get("FLEET_NAME", "example-robot")


async def main() -> None:
    robot = Robot(
        URL,
        manifest={  # the console renders exactly this: a drive widget, a battery, nothing else
            "drive": {"type": "twist", "max_v_mps": 1.0, "max_w_radps": 1.5},
            "battery": {},
            "channels": ["jobs"],
        },
        name=NAME,
        enrollment_key=os.environ.get("FLEET_ENROLL_KEY"),  # used once; the token file after that
        token_store=FileTokenStore(os.environ.get("FLEET_TOKEN_FILE", f"~/.fleet/{NAME}.json")),
    )
    # Setpoints come from the operator holding the lease; zero-velocity stops come from
    # the SDK itself (deadman, revoke, lost link). A real robot drives its base here.
    robot.on_twist(lambda c: print(f"twist [{c.source}] v={c.linear_x:.2f} m/s w={c.angular_z:.2f} rad/s"))
    robot.on_lease(lambda l: print("lease", "granted to" if l.granted else "revoked:", l.operator_id or l.reason))

    jobs = robot.channel("jobs")  # opaque to the platform: any JSON your services agree on

    async def on_job(sender: str, data: object) -> None:
        print("job from", sender, data)
        await jobs.publish({"accepted": data}, to=sender)

    jobs.on_message(on_job)
    await robot.connect()
    print("online as", robot.robot_id)

    async def telemetry() -> None:  # 1 Hz; dropped, not queued, while the link is down
        for t in range(1, 10**9):
            pose = local_pose(2 * math.cos(t / 20), 2 * math.sin(t / 20), yaw_rad=t / 20 + math.pi / 2, frame_id="map")
            await robot.telemetry(pose=pose, battery=max(0, 100 - t / 60))
            await asyncio.sleep(1)

    beat = asyncio.create_task(telemetry())
    try:
        await robot.run_forever()  # heartbeats, and reconnects with the same token
    finally:
        beat.cancel()
        await robot.close()


try:
    asyncio.run(main())
except KeyboardInterrupt:
    pass
