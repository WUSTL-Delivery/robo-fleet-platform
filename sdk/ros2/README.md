# fleet_agent (ROS 2)

The generic ROS 2 node that puts a robot on `fleet-server`. It is configured, not
programmed: a parameter file names the server, the robot, and the topics to use. The
node enrolls once, stays connected, declares the robot's manifest, reports telemetry,
turns an operator's twist into `cmd_vel` behind a lease check and a deadman, and gives the
robot's own application node the fleet's channels as plain ROS topics and "ask for help"
as a service. The application node never opens a socket.

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
| Teleop | with `drive.type: twist`: the twist of the operator who holds the lease is clamped to the drive limits and published as `geometry_msgs/Twist` on `cmd_vel.topic`; zero velocity when it should stop (see [Teleop and the stop](#teleop-and-the-stop)) |
| Twist transport | over the server's bus, and, with `aiortc` installed, over the operator's direct WebRTC data channel (`data_channel: auto`); one lease check and one deadman for both |
| Lease state | latched on `fleet/lease` (`fleet_agent_msgs/Lease`) for the robot's application node |
| Channels | each name in `channels` becomes `fleet/ch/<name>/in` and `fleet/ch/<name>/out` (`fleet_agent_msgs/ChannelMsg`, data as JSON text); see [Channels](#channels) |
| Help | the service `fleet/request_help` (`fleet_agent_msgs/RequestHelp`) puts the robot in the operators' queue; see [`fleet/request_help`](#fleetrequest_help) |

Not here yet: no camera. `cmd_vel` is `geometry_msgs/Twist` only (no `TwistStamped`
option yet).

The repo has two packages: `fleet_agent` (the node) and `fleet_agent_msgs` (its
interfaces: the messages `Lease` and `ChannelMsg`, and the service `RequestHelp`).

## Run it on a machine with ROS 2

These are the steps for a native install (for example a laptop with Humble). All
commands run from the root of this repo unless they `cd` elsewhere. The work is on the
branch `feat/queue-presence-sdks-teleop`:

```bash
git fetch origin
git checkout feat/queue-presence-sdks-teleop
git pull
```

### 1. Install the Python SDK into the ROS Python

`fleet_agent` imports the SDK (`fleet`), which is not a rosdep key, so install it with pip
for the same `python3` that ROS uses:

```bash
python3 -m pip install --user --upgrade pip
python3 -m pip install --user './sdk/python[webrtc]'
```

On Jazzy (Ubuntu 24.04) add `--break-system-packages` to both commands.

The first command matters on Humble: Ubuntu 22.04 ships pip 22.0, which fails to build
the SDK with `No module named 'packaging.licenses'`. Run the second command again after
pulling changes to `sdk/python`. If `pip` is missing: `sudo apt install python3-pip`.

`[webrtc]` adds [aiortc](https://github.com/aiortc/aiortc), which lets the node take twist
over the operator's direct WebRTC data channel as well as over the server. It installs
from binary wheels (aiortc, and `av` for codecs), and has been tested here on Linux arm64
with Humble (Python 3.10) and Jazzy (Python 3.12), in the dev container only. It has not
been tried on a native x86-64 machine or on the robot's computer. If it will not install
(pip tries to compile `av` and fails, or there is no wheel for the platform), install
without it:

```bash
python3 -m pip install --user ./sdk/python
```

Nothing else changes: the node logs `it arrives over the bus only (aiortc is not
installed)` and operator twist comes through the server, which is how it worked before.
`python3 -c "import aiortc; print(aiortc.__version__)"` tells you which case you are in.

### 2. Build the packages

```bash
source /opt/ros/humble/setup.bash
mkdir -p ~/fleet_ws/src
ln -sfn "$PWD/sdk/ros2/fleet_agent" ~/fleet_ws/src/fleet_agent
ln -sfn "$PWD/sdk/ros2/fleet_agent_msgs" ~/fleet_ws/src/fleet_agent_msgs
cd ~/fleet_ws
colcon build --packages-select fleet_agent_msgs fleet_agent
source install/setup.bash
```

`fleet_agent_msgs` is a message package, so the build needs a C++ compiler and
`rosidl_default_generators`. A desktop install has both; on a bare `ros-base` install:
`sudo apt install build-essential ros-humble-rosidl-default-generators ros-humble-geometry-msgs`.
Build again after every pull that touches `sdk/ros2` (this one adds a message and a
service to `fleet_agent_msgs`). Every terminal that runs the node, echoes or publishes a
`fleet/...` topic, or calls the help service must source `~/fleet_ws/install/setup.bash`.

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
    cmd_vel.topic: /cmd_vel
    channels: [jobs, status]
    telemetry.fix_topic: /gps/fix
    telemetry.battery_topic: /battery_state
    telemetry.rate_hz: 1.0
```

Keep the decimals (`1.0`, not `1`): ROS 2 rejects an integer for a double parameter.
`jobs` and `status` are the two channels steps 10 and 11 use; a real robot lists the
channels its application node and the fleet's services agreed on.

On a real robot, `cmd_vel.topic` is the highest-priority input of the robot's twist mux,
not the base's `cmd_vel` itself. On a laptop with no base, `/cmd_vel` is fine: nothing
listens, and step 8 just echoes it.

### 5. Run the node

```bash
FLEET_ENROLL_KEY=demo-enroll-key-0123456789abcdef \
  ros2 run fleet_agent fleet_agent --ros-args --params-file ~/fleet_agent.yaml
```

Expected output:

```
[INFO] [fleet_agent]: operator twist goes to '/cmd_vel', limited to 1.5 m/s and 1.5 rad/s, deadman 300 ms; it arrives over the bus or a WebRTC data channel
[INFO] [fleet_agent]: channel 'jobs': in on '/fleet/ch/jobs/in', out on '/fleet/ch/jobs/out', acked sends are answered by fleet_agent
[INFO] [fleet_agent]: channel 'status': in on '/fleet/ch/status/in', out on '/fleet/ch/status/out', acked sends are answered by fleet_agent
[INFO] [fleet_agent]: connecting to ws://localhost:8080/ws as 'thinkpad-01' (token file ~/.fleet/thinkpad-01.json), manifest {'drive': {...}, 'battery': {}, 'channels': ['jobs', 'status']}
[INFO] [fleet_agent]: no stored token: enrolling with the fleet enrollment key
[INFO] [fleet_agent]: online as r_...
```

The first line ends with `over the bus only (aiortc is not installed)` if step 1 was done
without `[webrtc]`.

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

### 8. Drive it from the console

Open two more terminals (source ROS and `~/fleet_ws/install/setup.bash` in each):

```bash
ros2 topic echo /cmd_vel
```

```bash
ros2 topic echo /fleet/lease --qos-durability transient_local --qos-reliability reliable
```

The lease terminal prints the latched state at once: `held: false`, `mode: autonomous`,
`connected: true`. Then, in the console:

1. Select `thinkpad-01` and click **Take over**. The lease terminal prints `held: true`
   with the `lease_id`, your `operator_id` and `mode: teleop`. The node logs
   `lease ls_... granted to operator o_...`.
2. Hold **W** (or the up arrow). `/cmd_vel` prints `linear.x: 0.75` ten times a second
   (the console's speed slider starts at half of `drive.max_v_mps`). **A** / **D** turn:
   `angular.z` is plus or minus 0.75.
3. Release the key. `/cmd_vel` prints zero velocity a few times in a quarter of a
   second, then goes quiet. Quiet is deliberate: a twist mux falls back to its other inputs.
4. Click **Hand back**. The lease terminal prints `held: false`, `reason: released`,
   `mode: autonomous`.

### 9. See the stop when the link dies

The point of the node: the robot stops by itself, whatever happens to the network or the
server. Do each of these **while holding W**, watching `/cmd_vel` and the node's log.

| Break it | How | Expect on `/cmd_vel` | Node log |
|---|---|---|---|
| Server freezes (no close, like a dead Wi-Fi link) | `docker pause <container>` (find it with `docker ps`); undo with `docker unpause` | zero within 300 ms | `cmd_vel zeroed: deadman: no valid twist` |
| Server dies | `docker kill <container>` | zero at once | `cmd_vel zeroed: link to fleet-server lost`, then `not connected ...; retry N` |
| Operator disappears | close the console tab | zero within 300 ms | `cmd_vel zeroed: lease revoked` or `deadman: no valid twist`, whichever comes first, and `lease ls_... ended: operator_lost` |
| Node is stopped | Ctrl-C in the node's terminal | one final zero | the process exits |

To pull the real network, the server has to be on another machine. Run step 3 there, put
its address in `url` (`ws://<server-ip>:8080/ws`), open the console at
`http://<server-ip>:8080`, and then, while holding W:

```bash
nmcli radio wifi off     # zero on /cmd_vel within 300 ms; the log says "deadman: no valid twist"
nmcli radio wifi on      # the node logs "online as r_..." with the same id
```

With Wi-Fi off nothing tells the node the link is gone, so `fleet/lease` still says
`connected: true` for a while. That is expected: the stop does not wait for the node to
find out. `connected: false` appears when the socket finally times out or reconnects.

One more, with the lease terminal of step 8 open: click **Take over**, then stop the
server and start it again (with the `-v fleet-data:/var/lib/fleet` volume of step 7, so
the robot's token survives). A restarted server has no leases. The node logs
`lease ls_... ended: while disconnected (reason unknown)` as it comes back online, and the
lease terminal prints `held: false`, `reason: ''`, `mode: help`, `connected: true`.

### 10. Receive on a channel, with the Go dispatcher

The application node's side of a channel is two topics. Play the application with
`ros2 topic echo` (source ROS and `~/fleet_ws/install/setup.bash` first):

```bash
ros2 topic echo /fleet/ch/jobs/in
```

Then start the example dispatcher, a small Go service that hands a job to a robot that is
online and autonomous every 2 seconds, on the channel `jobs`, and wants a receipt for
each. With Go 1.24 or newer, from the repo root:

```bash
cd sdk/go
FLEET_URL=ws://localhost:8080/ws FLEET_ENROLL_KEY=demo-enroll-key-0123456789abcdef \
  FLEET_TOKEN_FILE=/tmp/dispatcher.json go run ./examples/dispatcher
```

Without Go, the same in Docker (Linux, from the repo root):

```bash
docker run --rm --network host -v "$PWD":/src:ro -w /src/sdk/go \
  -e FLEET_URL=ws://localhost:8080/ws -e FLEET_ENROLL_KEY=demo-enroll-key-0123456789abcdef \
  -e FLEET_TOKEN_FILE=/tmp/dispatcher.json -e GOFLAGS=-buildvcs=false golang:1.26 go run ./examples/dispatcher
```

The dispatcher prints one line per job, and the echo terminal one message per job:

```
snapshot: 1 robots
acked job-1 by r_...
acked job-2 by r_...
```

```
stamp: ...
sender: s_...
to: ''
data: '{"id":"job-1","task":"inspect"}'
acked: true
---
```

(If `ros2 topic echo` dies with `!rclpy.ok()`, the ROS CLI daemon is stale: `ros2 daemon
stop`, then run it again.)

`acked: true` means the dispatcher asked for a receipt and `fleet_agent` gave it one, so
the application has nothing to answer. Now check the three things that make this safe:

- **Stop the echo** (Ctrl-C) and leave the dispatcher running. Nothing subscribes to the
  in topic, so the node does not ack: it logs `channel 'jobs': not acking a message from
  s_..., nothing subscribes to '/fleet/ch/jobs/in'`, and after 5 s the dispatcher prints
  `requeue job-N (1 waiting): fleet: timeout: ... no ack within 5s`. Start the echo
  again: the job at the head of the dispatcher's queue is delivered and acked.
- **Take over** the robot in the console. The dispatcher stops choosing it (it prints
  `requeue ...: no robot is online and AUTONOMOUS`) until you hand back.
- Each job id is printed once in the echo terminal, even though the dispatcher re-sends a
  job every second until it hears the ack.

### 11. Send on a channel

Start a listener in place of a real service. It is a few lines on the Python SDK that
print what arrives on the channel `status`:

```bash
python3 - <<'EOF'
import asyncio
from fleet import FleetClient

async def main():
    client = FleetClient("ws://localhost:8080/ws", kind="service", name="listener",
                         enrollment_key="demo-enroll-key-0123456789abcdef")
    client.channel("status").on_message(lambda sender, data: print("status from", sender, data, flush=True))
    await client.connect()
    print("listening as", client.client_id, flush=True)
    await client.wait_closed()

asyncio.run(main())
EOF
```

It prints `listening as s_...`. In another terminal, publish on the out topic the way the
application node would. `to` is left empty, so this is a broadcast to the channel:

```bash
ros2 topic pub --once -w 1 /fleet/ch/status/out fleet_agent_msgs/msg/ChannelMsg \
  "{data: '{\"state\": \"en_route\", \"leg\": 3}'}"
```

The listener prints `status from r_... {'state': 'en_route', 'leg': 3}`. To send to one
client only, set `to` to its id (the listener's `s_...`):

```bash
ros2 topic pub --once -w 1 /fleet/ch/status/out fleet_agent_msgs/msg/ChannelMsg \
  "{to: 's_PASTE_THE_ID', data: '{\"state\": \"arrived\"}'}"
```

Two things to try: a `to` that is not connected (`to: 's_nobody'`) makes the node log
`channel 'status': s_nobody is not connected, the message was dropped`; and `data` that is
not JSON (`data: 'hello'`) makes it log `dropped an out message whose data is not JSON`.
A JSON string needs its quotes: `data: '\"hello\"'`.

### 12. Ask for help

With the console open (step 7) and the lease terminal of step 8 running:

```bash
ros2 service call /fleet/request_help fleet_agent_msgs/srv/RequestHelp \
  "{reason: 'nav_goal_failed', context: '{\"attempts\": 3}'}"
```

The call returns `accepted=True, code='', message='help.request sent'`. In the console
the robot shows **Needs help** in the fleet list and appears in the queue with the reason
and the context. The lease terminal prints `mode: help`. Click **Take over** and then
**Hand back**: the queue entry closes and the lease terminal ends on `mode: autonomous`.

The two ways it says no, without waiting:

```bash
ros2 service call /fleet/request_help fleet_agent_msgs/srv/RequestHelp "{reason: ''}"
# accepted=False, code='invalid_message', message='reason must be 1 to 256 characters'
```

Stop the server, call it again with a reason, and it answers `accepted=False,
code='offline'` at once. Nothing is queued: the application asks again once
`fleet/lease` says `connected: true`.

## Teleop and the stop

An operator's twist reaches `cmd_vel.topic` only if all of these hold:

- the manifest declares a drive (`drive.type: twist`);
- the twist bears the id of the lease this robot was granted and still holds (checked by
  the SDK, and again by the node);
- after clamping: linear speed to `drive.max_v_mps` (the vector is scaled, so direction
  is kept), yaw rate to `drive.max_w_radps`. A twist with a NaN or infinite component is
  published as a stop.

Zero velocity is published:

| When | How fast |
|---|---|
| no valid twist for the deadman window | on the topic within `cmd_vel.deadman_ms` (300 ms) of the last valid twist |
| the lease is revoked, handed back, or stolen | at once |
| the socket to the server closes or resets | at once |
| the node shuts down (SIGINT, SIGTERM, fatal link error) | at once |

None of this asks the server anything. The deadman is two independent timers, each firing
50 ms before the deadline so that zero is on the topic inside the window:

- the Python SDK's timer, on the node's link thread (the thread that owns the socket);
- a watchdog on the ROS thread, on a steady-clock timer (it ignores sim time), that looks
  only at when the last valid twist was published.

Either stops the robot alone. If the SDK's event loop blocks or its thread dies, the
watchdog stops the robot; if the ROS executor stalls, the SDK's timer does.

With `data_channel: auto` and aiortc installed, the console's twist also arrives over a
direct WebRTC data channel (lower latency than through the server; the node answers the
offer of the operator who holds the lease and of nobody else). It enters at the same
point as bus twist: the same lease check, clamp, deadman and watchdog apply, and a
revoked lease closes the channel. The node passes no ICE servers of its own; where they
come from is the SDK's business. `data_channel: off` keeps twist on the bus.

A stop (including a zero twist from the operator) is published once at once and five
more times 50 ms apart, then the node stops publishing. While a lease is held and the
operator is idle, the topic is silent.

What the node cannot do, and the robot must:

- **If the `fleet_agent` process is killed or the computer hangs, nothing publishes
  zero.** The base controller or twist mux needs its own command timeout (a twist_mux
  input timeout of about 0.5 s, or the motor driver's watchdog). Treat that as required.
- The node does not decide who drives between teleop and autonomy. It publishes on one
  topic and reports the lease; the mux priority and the application node do the rest.
- After a stall (of the network or of the node), twist that was queued in the TCP stream
  is delivered late and all at once, and is obeyed for up to one deadman window. Twist
  carries no age the robot can trust yet.

### `fleet/lease`

`fleet_agent_msgs/Lease`, latched (reliable, transient local, depth 1), published when
it changes. The application node subscribes with the same QoS and gets the current state
even if it starts late.

| Field | Meaning |
|---|---|
| `held` | an operator holds the lease now |
| `lease_id` | the lease held, or the one that just ended; empty before the first |
| `operator_id` | who is driving, while `held` |
| `reason` | why the last lease ended: `released`, `expired`, `stolen`, `operator_lost`. Empty when it ended while the robot was disconnected (a server restart, or it ran out during a network drop): the robot learns on reconnecting that it is gone, not why |
| `mode` | `teleop` while held; then `autonomous` after a handback, `help` if the lease was lost for any other reason or for none known. Also `help` after `fleet/request_help` |
| `connected` | the link to the server is up |

Typical use: on `held: true`, cancel the active navigation goal; on `held: false` with
`mode: autonomous`, replan and resume; with `mode: help`, stay put and raise the hand
again. A short link drop does not end a lease (`held` stays true, `connected` goes false).
On every reconnect the server states the lease it holds for the robot and the node
believes that, not its memory: if the lease is gone, `held` goes false as `connected`
comes back.

## Channels

Channels are the fleet's opaque message bus (`docs/INTEGRATION.md`, 2.6): a dispatcher
sends a robot its assignment on one, the robot reports progress on another. The platform
never looks inside the data, and neither does `fleet_agent`. For every name in the
`channels` parameter it creates two topics of type `fleet_agent_msgs/ChannelMsg`
(reliable, keep last 10):

| Topic | Direction | |
|---|---|---|
| `fleet/ch/<name>/in` | `fleet_agent` publishes | every message for this robot on the channel: sent to it directly, or broadcast |
| `fleet/ch/<name>/out` | the application publishes | sent to the client named in `to`, or broadcast when `to` is empty |

```
builtin_interfaces/Time stamp   # in: when fleet_agent received it
string sender                   # in: who sent it (r_..., s_..., o_...), stamped by the server
string to                       # out: client id to send to; empty = broadcast
string data                     # the channel data as JSON text
bool acked                      # in: an acked send that fleet_agent answered (see below)
```

`data` is JSON text because channel data is any JSON value and ROS has no type for that:
`json.loads(msg.data)` on the way in, `json.dumps(...)` on the way out. The application
replies to a message by publishing with `to` set to its `sender`.

The names are declared in the manifest (`channels`), which is what makes a broadcast on
them reach this robot. A name is part of a topic name, so it must start with a lowercase
letter and hold only lowercase letters, digits and single underscores. The protocol also
allows `.`, `-` and a leading digit in a channel name; such a channel cannot be bridged,
and the node refuses to start rather than rename it.

Delivery is at most once in both directions, like the bus itself. An out message
published while the link is down is dropped with a warning, not queued. A message sent to
a client that is not connected is dropped and logged. Out data that is not JSON is dropped
and logged.

### Acked sends

A sender that needs to know its message was taken (a dispatcher handing over a job) uses
an *acked send* ([`protocol/README.md`](../../protocol/README.md#acked-send-convention-on-channel-data)):
it publishes `{"seq": n, "data": ...}` to the robot and re-sends every second until the
robot answers `{"ack": n}`. Who answers is a per-channel choice:

| | default | channel listed in `raw_channels` |
|---|---|---|
| who sends `{"ack": n}` | `fleet_agent`, through the SDK's `on_acked` | the application, by publishing it on the out topic with `to` = the sender |
| what the in topic carries | the inner `data`, with `acked: true` | the whole `{"seq": n, "data": ...}`, with `acked: false` |
| re-sends of the same `seq` | acked again, not published again (remembered for 10 minutes) | published every time; the application recognises repeats |
| when is the ack sent | when the message is published and at least one node subscribes to the in topic | when the application decides |

The default is the one to use unless there is a reason not to: the application node sees
each job once and has no protocol to implement. What the ack then means is exact and
limited: *the message was handed to a ROS subscription on this robot*. It does not mean
the application acted on it, and with nothing subscribed there is no ack at all, so a
dispatcher never believes a job was taken by a robot whose application is not running.
The memory of answered `seq`s lives in the `fleet_agent` process: restart it, and a
re-send that arrives afterwards is delivered again. A job whose effect must not happen
twice carries its own id inside the data (the example dispatcher's jobs do).

Use `raw_channels` when the ack has to mean more, for example "the planner accepted these
waypoints": the application gets the `seq`, decides, and publishes
`{"ack": <seq>}` itself.

Ordinary messages (anything that is not exactly `{"seq", "data"}`) are published as they
are on either kind of channel, with `acked: false`. That includes an `{"ack": n}` sent to
the robot, which matters to an application that makes acked sends of its own.

## `fleet/request_help`

`fleet_agent_msgs/RequestHelp`. The application calls it when autonomy gives up; the
robot goes from `AUTONOMOUS` to `HELP_REQUESTED` and shows up in every operator's queue.

```
string reason     # why, for the operator: 1 to 256 characters
string context    # optional: the JSON text of an object, e.g. {"attempts": 3}
---
bool accepted     # true: help.request was written to an open connection
string code       # "" | offline | invalid_message | timeout
string message
```

| `code` | When | What to do |
|---|---|---|
| (empty), `accepted: true` | the request was sent | wait; `fleet/lease` shows `mode: help`, then `held: true` when an operator takes over |
| `offline` | no connection to the server right now | ask again when `fleet/lease` says `connected: true`; nothing was queued |
| `invalid_message` | `reason` is empty or longer than 256 characters, or `context` is not a JSON object. Checked in the node with the server's own rule and the server's own error code | fix the request |
| `timeout` | the node's link thread did not take the request within 2 s (it is stuck; this should not happen) | ask again; report it |

The call returns in milliseconds and never waits for an operator. The service callback
does not block the node's executor: it waits in a callback group of its own, so the
`cmd_vel` watchdog keeps running while the request is handed to the link thread. Requests
are answered one at a time.

`accepted` means sent, not queued by the server. The server sends no confirmation for a
help request, and it silently ignores one from a robot that is already waiting or already
being driven (the first request, with its reason and its time, stands). If the server
refuses something the node let through (a rate limit), the node logs
`fleet-server answered <code>: <message>` and the service has already said `accepted`.
The reliable signal is `fleet/lease`.

## Parameters

| Parameter | Default | Meaning |
|---|---|---|
| `url` | `ws://localhost:8080/ws` | the server's WebSocket endpoint (`wss://<host>/ws` when deployed) |
| `name` | hostname | display name in the console |
| `token_file` | `~/.fleet/<name>.json` | where the token is kept (mode 0600); delete it to enroll as a new robot |
| `enroll_key` | `$FLEET_ENROLL_KEY` | fleet enrollment key, read only while `token_file` is empty |
| `drive.type` | `""` | `twist` declares a drive in the manifest; empty declares none |
| `drive.max_v_mps`, `drive.max_w_radps` | `0.0` | drive limits; both must be > 0 when `drive.type` is set. Declared in the manifest, and operator twist is clamped to them |
| `cmd_vel.topic` | `cmd_vel` | where operator twist is published (`geometry_msgs/Twist`); only with `drive.type` set |
| `cmd_vel.deadman_ms` | `300` | zero velocity is on the topic within this long of the last valid twist; an integer, 100 to 1000 |
| `data_channel` | `auto` | `auto`: also take twist over the operator's WebRTC data channel when aiortc is installed; `off`: bus only |
| `channels` | none | channel names to bridge to `fleet/ch/<name>/in` and `/out`, and to declare in the manifest |
| `raw_channels` | none | the channels (a subset of `channels`) whose acked sends the application answers itself |
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
(wrong enrollment key, revoked token, a second process using the same token file) or its
link thread dies, and with status 2 for a bad parameter. Network drops and server restarts are retried forever.

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
colcon test --packages-select fleet_agent --event-handlers console_direct+ --pytest-args -s
colcon test-result --verbose
```

### Adding a test

Tests live in `fleet_agent/test/`.

| File | Kind | Covers |
|---|---|---|
| `test_conversions.py` | plain pytest | message mappings, twist clamping, channel names, channel data as JSON text |
| `test_gate.py` | plain pytest, fake clock | the lease check, the watchdog, stop repeats |
| `test_params.py` | node constructed, never connected | `data_channel`, `channels`, parameters that must fail at startup |
| `test_online.py` | launch test, real server | enroll, manifest, telemetry, reconnect |
| `test_teleop.py` | launch test, real server and operator | twist to `cmd_vel`, `fleet/lease`, and the stop when: twist stops, the lease is handed back, the network goes silent, the socket resets, the server is killed; a lease the restarted server no longer has; twist over the WebRTC data channel (skipped without aiortc) |
| `test_channels.py` | launch test, real server, a service client, and the Go example dispatcher | in and out topics, broadcasts, acked sends (answered, not answered with no subscriber, raw), bad out messages, `fleet/request_help` accepted, refused and offline. The dispatcher test is skipped without a Go toolchain |
| `test_watchdog.py` | node inside the test process | the stop when the SDK loop is blocked, the link thread is dead, or the ROS executor is not spinning; a help request while the link thread is stuck |

A launch test starts a real `fleet-server` and the node, and watches the fleet through
a service client, the way the console does. `fleet_harness.py` holds the shared pieces:
`FleetServer`, `agent_node`, `watching`, plus `operating` (an operator that claims and
drives, over the bus or a data channel), `TcpProxy` (blackhole or cut the node's
connection), `Recorder` (timestamps `cmd_vel` and `fleet/lease`), `ChannelTap` (the
application node's two topics of one channel), `peering` (a service client that sends and
collects channel messages) and `request_help`. A new test is a new `test_*.py` file that
uses them. Both scripts pick it up without changes.

The container installs the SDK with the `webrtc` extra and prints the aiortc version it
got, so every test, the deadman tests included, runs with the data channel enabled. If
the extra will not install on some platform, the script says so loudly, installs the SDK
without it, and the data channel test is skipped.

The stop tests time each stop from the last non-zero `cmd_vel` to the zero that follows
and assert it is within 300 ms. A shared runner or a laptop VM sometimes freezes every
process for 100 ms or more, which no user-space timer survives, so the recorder measures
those freezes and each bound is extended by exactly that much. With `--pytest-args -s`
(or `pytest -s`) every test prints its numbers, for example
`deadman: zero 253 ms after the last non-zero cmd_vel, host stall 0 ms`.
