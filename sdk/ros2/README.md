# fleet_agent (ROS 2)

The generic ROS 2 node that puts a robot on `fleet-server`. It is configured, not
programmed: a parameter file names the server, the robot, and the topics to read. The
node enrolls once, stays connected, declares the robot's manifest, and reports telemetry.

It is a thin layer over the [Python SDK](../python/README.md). The SDK owns the
WebSocket, enrollment, heartbeat and reconnect; `fleet_agent` maps ROS parameters and
topics onto it and contains no socket code of its own.

ROS 2 Humble or Jazzy. Python 3.10+.

## What it does today

| | |
|---|---|
| Connection | enrolls with the fleet enrollment key on the first run, saves the token to `token_file`, and reuses it on every later run and every reconnect, so the robot keeps one id |
| Manifest | built from parameters: a `drive` block when `drive.type` is set, a `battery` block when `telemetry.battery_topic` is set |
| Telemetry | at `telemetry.rate_hz`: a geographic pose from `sensor_msgs/NavSatFix`, battery percent and voltage from `sensor_msgs/BatteryState` |

Not here yet: operator twist is not forwarded to `cmd_vel`, and channels are not bridged
to topics. A robot that declares `drive.type: twist` shows the drive control in the
console, but driving it does nothing until the twist bridge lands.

## Run it on a machine with ROS 2

These are the steps for a native install (for example a laptop with Humble). All
commands run from the root of this repo unless they `cd` elsewhere.

### 1. Install the Python SDK into the ROS Python

`fleet_agent` imports the SDK (`fleet`), which is not a rosdep key, so install it with pip
for the same `python3` that ROS uses:

```bash
python3 -m pip install --user --upgrade pip
python3 -m pip install --user ./sdk/python
```

On Jazzy (Ubuntu 24.04) add `--break-system-packages` to both commands.

The first command matters on Humble: Ubuntu 22.04 ships pip 22.0, which fails to build
the SDK with `No module named 'packaging.licenses'`. Run the second command again after
pulling changes to `sdk/python`. If `pip` is missing: `sudo apt install python3-pip`.

### 2. Build the package

```bash
source /opt/ros/humble/setup.bash
mkdir -p ~/fleet_ws/src
ln -s "$PWD/sdk/ros2/fleet_agent" ~/fleet_ws/src/fleet_agent
cd ~/fleet_ws
colcon build --packages-select fleet_agent
source install/setup.bash
```

### 3. Have a server to talk to

Any reachable `fleet-server` works. To run one on the same machine with the console
built in, use Docker from the repo root:

```bash
docker build -f server/Dockerfile -t fleet-server:dev .
docker run --rm -p 8080:8080 \
  -e FLEET_BOOTSTRAP_FLEET=demo \
  -e FLEET_BOOTSTRAP_ENROLL_KEY=demo-enroll-key-0123456789abcdef \
  -e FLEET_ADMIN_TOKEN=demo-admin-token-0123456789abcdef \
  -e FLEET_MAP_CENTER=38.6488,-90.3108 \
  fleet-server:dev
```

Without Docker, build it with Go 1.26+ and Node 22 as in the
[Python SDK quickstart](../python/README.md#quickstart) (`npm run embed` in `console/`
first, or the console page is a placeholder).

### 4. Write a parameter file

Save this as `~/fleet_agent.yaml`. Every parameter is explained in
[`fleet_agent/config/fleet_agent.example.yaml`](fleet_agent/config/fleet_agent.example.yaml).

```yaml
fleet_agent:
  ros__parameters:
    url: ws://localhost:8080/ws
    name: thinkpad-01
    token_file: ~/.fleet/thinkpad-01.json
    drive.type: twist
    drive.max_v_mps: 1.5
    drive.max_w_radps: 1.5
    telemetry.fix_topic: /gps/fix
    telemetry.battery_topic: /battery_state
    telemetry.rate_hz: 1.0
```

Keep the decimals (`1.0`, not `1`): ROS 2 rejects an integer for a double parameter.

### 5. Run the node

```bash
FLEET_ENROLL_KEY=demo-enroll-key-0123456789abcdef \
  ros2 run fleet_agent fleet_agent --ros-args --params-file ~/fleet_agent.yaml
```

Expected output:

```
[INFO] [fleet_agent]: connecting to ws://localhost:8080/ws as 'thinkpad-01' (token file ~/.fleet/thinkpad-01.json), manifest {'drive': {...}, 'battery': {}}
[INFO] [fleet_agent]: no stored token: enrolling with the fleet enrollment key
[INFO] [fleet_agent]: online as r_...
```

The enrollment key is only read on the first run. After that the token file is enough,
and the "enrolling" line no longer appears.

### 6. Give it a position

A robot with a GPS driver already publishes `NavSatFix`. On a machine without one,
publish a fix and a battery state by hand, each in its own terminal (source ROS first):

```bash
ros2 topic pub -r 1 /gps/fix sensor_msgs/msg/NavSatFix \
  "{status: {status: 0}, latitude: 38.6488, longitude: -90.3108, altitude: 160.0}"
```

```bash
ros2 topic pub -r 1 /battery_state sensor_msgs/msg/BatteryState "{percentage: 0.87, voltage: 12.5}"
```

### 7. See it in the console

Mint an operator invite, open http://localhost:8080, and redeem the printed key:

```bash
curl -s -X POST -H "Authorization: Bearer demo-admin-token-0123456789abcdef" \
  http://localhost:8080/api/admin/fleets/demo/operator-invites
```

You should see:

- `thinkpad-01` in the fleet list, online;
- a marker for it on the map at 38.6488, -90.3108 (Danforth campus);
- in its panel, the pose `38.648800, -90.310800`, battery 87 %, and a drive control,
  because the manifest declares a drive and a battery.

Then check the two behaviours that matter on a real link:

- Stop the `/gps/fix` publisher. After `telemetry.pose_timeout_s` (5 s) the node stops
  reporting a pose instead of repeating a stale one. The robot stays online.
- Stop the server and start it again. The node logs `not connected ...; retry N in ...`
  and then `online as r_...` with the **same** id. (A server started with `docker run
  --rm` and no volume loses its database on restart, so the token is refused and the
  node exits; add `-v fleet-data:/var/lib/fleet` to the `docker run` above to test this.)

## Parameters

| Parameter | Default | Meaning |
|---|---|---|
| `url` | `ws://localhost:8080/ws` | the server's WebSocket endpoint (`wss://<host>/ws` when deployed) |
| `name` | hostname | display name in the console |
| `token_file` | `~/.fleet/<name>.json` | where the token is kept (mode 0600); delete it to enroll as a new robot |
| `enroll_key` | `$FLEET_ENROLL_KEY` | fleet enrollment key, read only while `token_file` is empty |
| `drive.type` | `""` | `twist` declares a drive in the manifest; empty declares none |
| `drive.max_v_mps`, `drive.max_w_radps` | `0.0` | drive limits; both must be > 0 when `drive.type` is set |
| `telemetry.fix_topic` | `""` | `sensor_msgs/NavSatFix` topic; empty = no pose |
| `telemetry.battery_topic` | `""` | `sensor_msgs/BatteryState` topic; set = the manifest declares a battery |
| `telemetry.rate_hz` | `1.0` | telemetry samples per second |
| `telemetry.pose_timeout_s` | `5.0` | drop the pose this long after the last fix; `0.0` keeps it forever |

Poses are frame-relative in the protocol (geographic or local Cartesian). `NavSatFix` is
the only pose source so far, and it is optional: a robot without GPS leaves
`telemetry.fix_topic` empty and is online without a pose. A fix has no heading, so the
pose carries no yaw and the map marker has no direction.

`BatteryState.percentage` is read as the 0 to 1 fraction the message defines.

The node exits with status 1 when the server refuses it in a way retrying cannot fix
(wrong enrollment key, revoked token, a second process using the same token file), and
with status 2 for a bad parameter. Network drops and server restarts are retried forever.

## Tests

Without a ROS 2 install, one command builds the dev image, builds `fleet-server` and
the workspace inside it, and runs the tests. It needs only Docker:

```bash
sdk/ros2/test.sh                    # ROS 2 Humble
ROS_DISTRO=jazzy sdk/ros2/test.sh   # ROS 2 Jazzy
sdk/ros2/test.sh bash               # build, then a shell with the workspace sourced
```

CI runs the same script (job `sdk-ros2`).

On a native install, after step 2, with Go installed (the launch tests build
`fleet-server` from `server/`, or use the binary named by `FLEET_SERVER_BIN`):

```bash
cd ~/fleet_ws
colcon test --packages-select fleet_agent --event-handlers console_direct+
colcon test-result --verbose
```

### Adding a test

Tests live in `fleet_agent/test/`. `test_conversions.py` covers the message mappings
with plain pytest. `test_online.py` is a launch test: it starts a real `fleet-server`
and the node, publishes ROS messages, and watches the fleet through a service client,
the way the console does. `fleet_harness.py` holds the shared pieces (`FleetServer`,
`agent_node`, `watching`); a new launch test is a new `test_*.py` file that uses them.
Both scripts pick it up without changes.
