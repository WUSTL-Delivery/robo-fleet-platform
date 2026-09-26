# sdk/ros2 — `fleet_agent`

The ROS 2 node that puts a robot on fleet-server. It is the third SDK, alongside
`sdk/typescript` (services) and `sdk/python` (robots and quick agents); it is a thin
ament package over `sdk/python`, not a reimplementation of the protocol.

One node, five wires:

| Platform | ROS |
|---|---|
| capability manifest | parameters (drive limits, cameras, channels) |
| `twist` under a lease | a `twist_mux` input, default `cmd_vel_fleet` |
| `telemetry` | `imu/data` today — see [Not wired yet](#not-wired-yet) |
| `help.request` | `~/request_help`, a `Trigger` service or a `String` topic |
| `lease.granted` / `lease.revoked` | `~/mode`, latched |

## Install

`fleet` is not on PyPI yet, so rosdep cannot resolve it. Install it into the same
interpreter the node will run under, then build normally:

```bash
pip install -e /path/to/robo-fleet-platform/sdk/python

# from the delivery-robo workspace, with this package symlinked or copied into src/
colcon build --packages-select fleet_agent
source install/setup.bash
```

## Run

First run spends a one-time enrollment key and writes a token; every run after that is
the token. Keep the key out of git and out of shell history:

```bash
FLEET_ENROLL_KEY=fp-ek-... \
  ros2 launch fleet_agent fleet_agent.launch.py url:=wss://fleet.example.org/ws
```

Mint the key with `fleetctl` (see `docs/FLEETCTL.md`). `token_file` defaults to
`/var/lib/fleet_agent/token.json`, so the robot keeps one identity across restarts —
point it somewhere writable if you are not running as root.

### Wire up twist_mux in the same change

The agent publishes onto a mux input, not onto `cmd_vel`. Add the `fleet` input to
delivery-robo's `sim/src/robo_courier/config/twist_mux.yaml` —
[`config/twist_mux_fleet.yaml`](fleet_agent/config/twist_mux_fleet.yaml) is that file
with the block already in it — or the agent will publish setpoints nothing listens to.

```yaml
fleet:
  topic   : cmd_vel_fleet
  timeout : 0.5
  priority: 50
```

Priority 50 is the deliberate part: remote teleop outranks navigation (10) and the
tracker (20), but stays under the joystick (100). Whoever is standing next to the robot
can see it, and they keep the final say over an operator watching a video feed.

`master_launch.py` does not currently start `twist_mux` at all — only
`launch_real_robot.launch.py` does. Bringing the agent up under `master_launch.py` means
adding both.

## Safety model

Three independent layers, shortest fuse first. None of them is the only brake:

| Layer | Fires after | Effect |
|---|---|---|
| SDK deadman | 300 ms without a valid setpoint | zero velocity, published immediately |
| `stop_hold_s` | lease ends | keeps publishing zero for 1.0 s |
| `twist_mux` timeout | 0.5 s without a message on the input | drops the fleet input |

The ordering matters and is not accidental. The mux falls through to the *next* priority
when an input goes stale, so going silent after a stop would hand a moving robot back to
whatever nav goal was live. So while a lease is held — and for `stop_hold_s` after it
ends — the agent republishes the last command at `publish_rate_hz`, zero included.
`stop_hold_s` is longer than the mux timeout on purpose: the base is commanded to zero
and held there until after the mux has let go. Silence is reserved for "autonomy may have
the wheel back", which makes handback need no message at all.

The robot also clamps every incoming setpoint to its own declared `max_v_mps` /
`max_w_radps`, and a NaN setpoint clamps to zero. The server checks the manifest limits
too; the robot does not assume it did.

Local obstacle avoidance stays up during takeover. This is safeguarded velocity control,
not raw actuation (DESIGN.md D3).

## Threading

The SDK is asyncio, rclpy is not, so the SDK runs on its own thread (`loop.py`). The
deadman is timed by `loop.call_later` and nothing in `rclpy.spin` can starve it.

- **SDK → ROS** (twist, lease): the handler publishes immediately from the asyncio
  thread rather than waiting for the next tick — at 20 Hz that would add up to 50 ms to
  an emergency stop. The twist publisher is written from both threads and is guarded by
  its own lock; everything else crosses via a call-soon queue the tick drains.
- **ROS → SDK** (help requests, shutdown): `AsyncioThread.submit`, which returns a
  `concurrent.futures.Future`.

## Not wired yet

delivery-robo has no `Odometry` publisher, no `NavSatFix` (the `ublox_dgnss` submodule is
not checked out), no `BatteryState` and no Nav2 action, so **the node subscribes to none
of them**. Each is a stub method in `agent_node.py` with its conversion already chosen
and its subscription left unmade — wiring one is filling the body and adding the
`create_subscription`, not making a decision.

| Stub | Blocked on | What it unlocks |
|---|---|---|
| `_on_odom` | ROB-32 / ROB-41 (autonomy stack) | `local_pose` + velocity |
| `_on_fix` | `ublox_dgnss` checkout | `geo_pose` — what the campus graph needs |
| `_on_battery` | ROB-24 (battery indicator) | `telemetry.battery`, and `declare_battery: true` |
| `_cancel_nav_goals` | ROB-41 (Nav2 in `master_launch.py`) | autonomy lets go on takeover |

**Consequence today: telemetry carries health only.** The console will list the robot,
render its drive widget, and drive it — but will not place it on the map, because nothing
on the robot can yet say where it is. Teleop works; localization does not exist to
report.

Also absent platform-side: there is no media plane yet (no WebRTC, no TURN), so `cameras`
in the manifest *declares* a camera without streaming it, and twist rides the bus rather
than a direct path. That is roadmap step 3, tracked in `docs/INTEGRATION.md` §8.

## Tests

`test/test_conversions.py` is pure and needs no ROS:

```bash
pytest test/test_conversions.py
```

It pins the things that are easy to get quietly wrong: NaN never reaching the wire (JSON
cannot represent it and the schema rejects it), an all-zero quaternion reading as *unknown
heading* rather than a confident East, battery percentage disagreeing with REP 147, and a
NaN setpoint clamping to zero. `test_flake8.py` and `test_pep257.py` follow the club
repo's convention and run under `colcon test`.
