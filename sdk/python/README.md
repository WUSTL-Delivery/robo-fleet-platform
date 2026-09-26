# fleet-sdk (Python)

The Python client for `fleet-server`. A robot (or a service) dials out over one
WebSocket, enrolls once with the fleet's enrollment key, and from then on reconnects
with the same token. The SDK owns the connection, heartbeat, reconnect, the lease check
on teleop, and the deadman stop. Your code owns what the robot does.

Python 3.10+. One runtime dependency (`websockets`).

## Install

Not published to PyPI yet (the package name is a placeholder until the project is
named). From a checkout of this repo:

```bash
python3 -m venv .venv
. .venv/bin/activate
pip install -e sdk/python
```

## Quickstart

You need a server to talk to. Locally, from the repo root (Go 1.26+):

```bash
cd server && go build -o bin/fleet-server ./cmd/fleet-server && cd ..
FLEET_LISTEN=127.0.0.1:8080 FLEET_DB=fleet.db \
FLEET_BOOTSTRAP_FLEET=demo FLEET_BOOTSTRAP_ENROLL_KEY=demo-enroll-key-0123456789abcdef \
FLEET_ADMIN_TOKEN=demo-admin-token-0123456789abcdef \
  server/bin/fleet-server
```

The enrollment key lets robots and services join fleet `demo`. The admin token is only
for minting operator invites (below); leave it unset if you do not need the console.

In a second terminal, with the venv active, run the example robot:

```bash
FLEET_ENROLL_KEY=demo-enroll-key-0123456789abcdef python sdk/python/examples/robot.py
#   online as r_...
```

The first run enrolls and saves the token to `~/.fleet/example-robot.json` (mode 0600).
Every later run reuses it, so the robot keeps the same id across restarts and
`FLEET_ENROLL_KEY` is no longer needed. Delete that file to enroll as a new robot.

The robot is now online: it declares a drive, a battery and a `jobs` channel, and
sends a pose once a second. To drive it, get an operator invite and open the console:

```bash
cd server && go build -o bin/fleetctl ./cmd/fleetctl && cd ..
FLEETCTL_SERVER=http://127.0.0.1:8080 FLEETCTL_TOKEN=demo-admin-token-0123456789abcdef \
  server/bin/fleetctl invite operator --fleet demo
```

Redeem the printed key at http://127.0.0.1:8080, take the robot over, and drive with
WASD. (A plain `go build` serves a "console not built" page there; run
`cd console && npm install && npm run embed` once and rebuild the server to embed it.) The robot's terminal prints each twist and each stop, with its source.

To point the same robot at a deployed server, change the URL:
`FLEET_URL=wss://fleet.bearcarts.com/ws` (with that fleet's enrollment key on first run).

The whole example is [`examples/robot.py`](examples/robot.py), under 60 lines.

## Connecting to a deployed server

A deployed server sits behind TLS at `wss://<host>/ws` (the club's is
`wss://fleet.bearcarts.com/ws`, running `fleet-server` 0.1.0). Before pointing a real
robot at it, run the live check. It needs only the fleet's enrollment key
(`FLEET_ENROLL_KEY` from the club deploy's secrets; the script never prints it):

```bash
FLEET_ENROLL_KEY=... python sdk/python/scripts/live_check.py
# FLEET_URL defaults to wss://fleet.bearcarts.com/ws; set it to check another server
```

It runs seven steps and prints a check mark or a cross for each, with a 15 s timeout
per step (`FLEET_STEP_TIMEOUT`):

1. the host resolves and accepts a TLS connection
2. a throwaway robot enrolls, connects and sends its manifest (its `client_id` is printed)
3. a watcher connects as a `service` and subscribes to `presence`
4. the watcher sees the robot online (in the snapshot or as a `robot.online` event)
5. the watcher sends the robot a message on channel `live-check`; the robot prints it
6. the robot replies and the watcher receives the reply
7. the robot closes and the watcher sees `robot.offline`

It ends with `LIVE CHECK PASSED` and exit status 0, or `LIVE CHECK FAILED`, a hint, and
exit status 1 (2 if `FLEET_ENROLL_KEY` is missing). It only uses messages 0.1.0
supports, so it works against the club deployment and against newer servers.

Things to know:

- **It leaves two client rows on the server.** Both clients enroll with the fleet key
  and keep their tokens in memory only, so nothing is written to your disk, but each run
  registers `live-check-robot-<id>` and `live-check-watcher-<id>` on the server. v0 has
  no way to delete a client; the rows are harmless and show as offline robots or
  services. If you already have a service token, set `FLEET_SERVICE_TOKEN` and the
  watcher uses it instead of enrolling, which leaves only the robot row.
- **Some networks block the domain.** Campus and corporate networks may answer the
  lookup with a DNS sinkhole (for example a CNAME to `sinkhole.paloaltonetworks.com`) or
  not resolve it at all. Step 1 catches that and says so; run it again from a phone
  hotspot or home network.
- **python.org Python on macOS may lack CA certificates.** If step 1 fails with
  `unable to get local issuer certificate`, run
  `/Applications/Python 3.x/Install Certificates.command` once, or prefix the command
  with `SSL_CERT_FILE=/etc/ssl/cert.pem`. A real robot hits the same error, so fix it
  before deploying one.

## Fake robot

`examples/fake_robot.py` pretends to be a robot so you can try a server by hand. It
enrolls, shows up on the console map, wanders around a point while autonomous, drives
under an operator's WASD, drains a battery, answers `jobs` channel messages, and can ask
for help.

```bash
cd sdk/python/examples
cp .env.example .env        # set FLEET_URL and FLEET_ENROLL_KEY
./run_fake_robot.sh                       # fake-robot-01
./run_fake_robot.sh --name fake-02 --help-after 30
```

`run_fake_robot.sh` loads `examples/.env` (git-ignored) and creates `sdk/python/.venv` on
first use. While it runs, type `h` + Enter to ask for help, `p` to pause wandering, `q`
to quit. The first run saves a token to `~/.fleet/<name>.json`; later runs come back as
the same robot without the key. Retire test robots with `fleetctl client revoke`.

## API overview

```python
from fleet import Robot, FileTokenStore, geo_pose, local_pose

robot = Robot(
    "ws://localhost:8080/ws",
    manifest={"drive": {"type": "twist", "max_v_mps": 1.0, "max_w_radps": 1.5},
              "battery": {}, "channels": ["jobs"]},
    name="bot-1",
    enrollment_key=key,                          # only read when the store is empty
    token_store=FileTokenStore("~/.fleet/bot-1.json"),
)

robot.on_twist(lambda cmd: base.drive(cmd.linear_x, cmd.angular_z))
robot.on_lease(lambda change: nav.cancel() if change.granted else None)

await robot.connect()                            # enroll if needed, hello, manifest
await robot.telemetry(pose=geo_pose(38.6488, -90.3108, yaw_rad=1.57), battery=87)
await robot.telemetry(pose=local_pose(1.0, 2.0, yaw_rad=0.3, frame_id="map"))
await robot.request_help("stuck at a curb")      # joins the intervention queue

jobs = robot.channel("jobs")
jobs.on_message(lambda sender, data: print(sender, data))   # directed + broadcast
await jobs.publish({"done": 17}, to=sender_id)   # directed; raises ChannelTargetNotFound if offline
await jobs.publish({"status": "idle"})           # broadcast

await robot.run_forever()                        # until close() or a terminal error
await robot.close()
```

| Call | What it does on the wire |
|---|---|
| `Robot(url, manifest=, name=, enrollment_key=, token_store=)` | nothing yet; `token_store` defaults to memory, so pass a `FileTokenStore` on anything that restarts |
| `await connect()` | `enroll.request` (first run only), `hello`, then the `manifest`. Returns the `welcome` payload |
| `await telemetry(pose=, battery=, velocity=, health=)` | one `telemetry`. Pose comes from `geo_pose()` or `local_pose()`, never bare lat/lon. Returns `False` (and drops the sample) while disconnected |
| `await request_help(reason, context=None)` | `help.request`: the robot raises its hand for an operator |
| `on_twist(handler)` | `handler(TwistCommand)`: `linear_x`, `linear_y`, `angular_z`, `source` (`operator`, `deadman`, `revoked`, `disconnected`), `lease_id`, `is_stop` |
| `on_lease(handler)` | `handler(LeaseChange)`: `granted`, `lease_id`, `operator_id`, `reason` on revoke |
| `channel(name).on_message(handler)` | `handler(sender, data)` for `channel.message`; `sender` is stamped by the server |
| `await channel(name).publish(data, to=None)` | `channel.publish`, directed or broadcast. A directed send waits 0.5 s for a `not_found` reply; silence is not a delivery receipt |
| `robot.robot_id`, `.state`, `.mode`, `.lease` | this robot's id, connection state, `autonomous`/`help`/`teleop`, current lease |
| `robot.client` | the underlying `FleetClient`, for message types `Robot` does not wrap |

Handlers may be plain functions or `async def`; a coroutine handler runs as its own task.
A handler that raises is logged and never breaks the connection. Services and operators
use `FleetClient(url, kind="service", ...)` directly, with the same `channel()` API.

The module docstrings in `fleet/robot.py`, `fleet/channel.py` and `fleet/client.py` are
the reference.

## How it stays online

- **Enroll once.** With an empty token store the client opens a one-shot socket, sends
  `enroll.request`, and saves the returned token. It never re-enrolls on its own: a new
  enrollment would be a new robot.
- **Hello every time.** Each connect sends `hello` with the stored token. The manifest
  is re-sent on every reconnect, and channel subscriptions are renewed, because the
  server forgets both when a connection drops.
- **Heartbeat** at the interval the server's `welcome` names (10 s by default). Miss about
  2.5 of them and the server marks the robot offline.
- **Reconnect** with exponential backoff and jitter (0.25 s doubling to 10 s), same token,
  after a network drop, a server restart, or a heartbeat lapse. A refusal that retrying
  cannot fix is terminal and `run_forever()` raises it: `auth_failed` (bad or revoked
  token, wrong enrollment key) or `conflict` (another process connected with the same
  token; run one process per token file).

## Deadman and teleop safety

The robot obeys a twist only when it carries the lease id the robot currently holds, and
only if its manifest declares a `drive`. Everything else fails closed, locally, without
waiting for the server:

- no valid twist for 300 ms (`deadman_ms`): a zero-velocity command with source `deadman`;
- lease revoked (handback, expiry, steal, operator lost): a stop with source `revoked`;
- link lost: a stop with source `disconnected`.

So an `on_twist` handler that forwards every command to the base is already safe. A
robot that also runs its own autonomy should pause it on `on_lease` grant and decide on
revoke whether to resume (`robot.mode` is `autonomous` after a handback, `help` after an
expiry or a lost operator).

## Tests

Every test runs against the real `fleet-server` binary on an ephemeral port (Go must be
installed; the fixture builds it once per session):

```bash
pip install -e 'sdk/python[dev]'
cd sdk/python && pytest
```
