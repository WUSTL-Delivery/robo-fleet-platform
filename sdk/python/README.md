# fleet-sdk (Python)

The Python client for `fleet-server`. A robot (or a service) dials out over one
WebSocket, enrolls once with the fleet's enrollment key, and from then on reconnects
with the same token. The SDK owns the connection, heartbeat, reconnect, the lease check
on teleop, and the deadman stop. Your code owns what the robot does.

Python 3.10+. One runtime dependency (`websockets`); the optional `webrtc` extra adds
`aiortc` for [twist over a WebRTC data channel](#twist-over-webrtc).

## Install

Not published to PyPI yet (the package name is a placeholder until the project is
named). From a checkout of this repo:

```bash
python3 -m venv .venv
. .venv/bin/activate
pip install -e sdk/python              # bus twist only
pip install -e 'sdk/python[webrtc]'    # also answers the operator's WebRTC offer
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
under an operator's WASD, drains a battery, accepts and acks jobs sent to it on the
`jobs` channel ([acked receive](#acked-receive)), and can ask for help.

```bash
cd sdk/python/examples
cp .env.example .env        # set FLEET_URL and FLEET_ENROLL_KEY
./run_fake_robot.sh                       # fake-robot-01
./run_fake_robot.sh --name fake-02 --help-after 30
```

`run_fake_robot.sh` loads `examples/.env` (git-ignored) and creates `sdk/python/.venv` on
first use, with the `webrtc` extra, so the fake robot answers the console's WebRTC offer
and prints `twist over the WebRTC data channel` or `twist over the bus` as the operator's
transport changes (`--no-data-channel` keeps it on the bus). It uses the STUN/TURN
servers the fleet-server is configured with; `--ice-servers` or `FLEET_ICE_SERVERS` takes
a JSON list to use instead of them. While it runs, type `h` + Enter to ask for help, `p` to pause wandering, `q`
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
jobs.on_acked(lambda sender, data: queue.put_nowait(data))  # once per acked send; the SDK acks

await robot.run_forever()                        # until close() or a terminal error
await robot.close()
```

| Call | What it does on the wire |
|---|---|
| `Robot(url, manifest=, name=, enrollment_key=, token_store=)` | nothing yet; `token_store` defaults to memory, so pass a `FileTokenStore` on anything that restarts |
| `await connect()` | `enroll.request` (first run only), `hello`, then the `manifest`. Returns the `welcome` payload |
| `await telemetry(pose=, battery=, velocity=, health=)` | one `telemetry`. Pose comes from `geo_pose()` or `local_pose()`, never bare lat/lon. Returns `False` (and drops the sample) while disconnected |
| `await request_help(reason, context=None)` | `help.request`: the robot raises its hand for an operator |
| `Robot(..., data_channel=None, ice_servers=None)` | nothing at once. With the `webrtc` extra installed the robot answers the lease holder's WebRTC offer, and sends `ice.request` when it takes a lease to learn the installation's STUN/TURN servers; see [Twist over WebRTC](#twist-over-webrtc) |
| `await client.ice_config(timeout=2.0)` | `ice.request`; returns the `ice.config` payload (`ice_servers`, `expires_at_ms`). Raises `FleetClientError` on a refusal, no answer in time, or a server that predates the message (`invalid_message`). On `FleetClient`, so `robot.client.ice_config()` |
| `await client.ice_servers(timeout=2.0)` | the same request; returns the list for `RTCConfiguration.iceServers` and never raises: any failure is `[]` |
| `on_twist(handler)` | `handler(TwistCommand)`: `linear_x`, `linear_y`, `angular_z`, `source` (`operator`, `deadman`, `revoked`, `disconnected`), `lease_id`, `is_stop`, `via` (`bus` or `p2p` for an operator setpoint, `None` for a stop) |
| `on_lease(handler)` | `handler(LeaseChange)`: `granted`, `lease_id`, `operator_id`, `reason` on revoke (`None` when the lease ended while the robot was disconnected) |
| `channel(name).on_message(handler)` | `handler(sender, data)` for `channel.message`; `sender` is stamped by the server |
| `channel(name).on_acked(handler)` | `handler(sender, data)` once per acked send, with the inner `data`; the SDK publishes `{"ack": seq}` back when it returns without raising, and again for every repeat. See [Acked receive](#acked-receive) |
| `await channel(name).publish(data, to=None)` | `channel.publish`, directed or broadcast. A directed send waits 0.5 s for a `not_found` reply; silence is not a delivery receipt |
| `robot.robot_id`, `.state`, `.mode`, `.lease` | this robot's id, connection state, `autonomous`/`help`/`teleop`, current lease |
| `robot.data_channel_enabled`, `.data_channel_open` | whether the robot answers WebRTC offers at all, and whether a twist data channel is open right now |
| `robot.client` | the underlying `FleetClient`, for message types `Robot` does not wrap |

Handlers may be plain functions or `async def`; a coroutine handler runs as its own task.
A handler that raises is logged and never breaks the connection. Services and operators
use `FleetClient(url, kind="service", ...)` directly, with the same `channel()` API.

The module docstrings in `fleet/robot.py`, `fleet/channel.py` and `fleet/client.py` are
the reference.

## Acked receive

A channel publish is at-most-once and gets no reply. When a sender needs to know its
message was accepted, the two clients use the acked-send convention on the channel's
data. The single reference for it is
[`protocol/README.md`, "Acked send"](../../protocol/README.md#acked-send-convention-on-channel-data):
the sender publishes `{"seq": n, "data": ...}` to one client and re-sends it until that
client answers `{"ack": n}` on the same channel. This SDK implements the receiving half:

```python
jobs = robot.channel("jobs")

def on_job(sender, data):        # data is the inner value; seq is not shown
    work_queue.put_nowait(data)  # returning without an error accepts it

jobs.on_acked(on_job)
```

- **Once per send.** The handler runs once per `(channel, sender, seq)` no matter how
  many copies arrive. Every copy received after it was accepted is acked again.
- **Returning accepts; raising refuses.** The ack is published when the handler returns
  (or its coroutine finishes). If it raises, no ack is sent and the message is
  forgotten, so the sender's next re-send runs the handler again. Copies that arrive
  while an `async` handler is still running are dropped without an ack.
- **Accepted is not finished.** Return well inside the sender's re-send interval (1 s by
  default) and do long work afterwards; report its result as ordinary channel data.
- **Memory.** Accepted messages are remembered for 10 minutes (`ACKED_MEMORY_SECONDS`),
  in memory. A robot that restarts forgets them, and a re-send arriving after that is
  handled a second time, so work that must not happen twice needs its own identity
  inside `data`.
- **Sharing a channel.** One acked handler per channel. Only data of exactly the shape
  `{"seq": <integer 1..2^53-1>, "data": ...}` goes to it, and then not to `on_message`
  handlers; everything else on the channel is ordinary data for `on_message`.

Sending acked messages from Python is not implemented yet.

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
- link lost: a stop with source `disconnected`;
- reconnected, and the server no longer holds the lease: a stop with source `revoked`.

A lease is never trusted across a reconnect. The server keeps it through a link blip, but
it can also end while the robot is away (expiry, handback, the operator leaving) with
nobody to tell. So the robot obeys nothing from the moment the link drops, and on the
`welcome` of the next connection it replaces what it believed with the lease that welcome
states: the same lease is kept and driving resumes, a different one is taken like a new
grant, and none drops it. A welcome that does not say (a server older than
`welcome.lease`) confirms nothing either, so against such a server every reconnect ends
the lease on the robot's side and the operator has to claim again. `on_lease` gets the
drop as a revoke with `reason=None`, since the robot was not there to hear why, and
`robot.mode` becomes `help`. See `protocol/README.md`, "A robot's lease at connect".

So an `on_twist` handler that forwards every command to the base is already safe. A
robot that also runs its own autonomy should pause it on `on_lease` grant and decide on
revoke whether to resume (`robot.mode` is `autonomous` after a handback, `help` after an
expiry or a lost operator).

## Twist over WebRTC

While an operator holds the lease, the console offers a direct WebRTC data channel and
sends twist on it instead of through the server. The contract is
[`protocol/README.md`, "Teleop data plane"](../../protocol/README.md#teleop-data-plane-twist-over-webrtc);
this SDK implements the robot's side of it with [aiortc](https://github.com/aiortc/aiortc).

```bash
pip install -e 'sdk/python[webrtc]'
```

That is all it takes: with aiortc importable, `Robot` answers the offer, and channel
twists reach the same `on_twist` handler as bus twists.

```python
robot = Robot(url, manifest=..., name="bot-1", token_store=...)
robot.on_twist(lambda cmd: base.drive(cmd.linear_x, cmd.angular_z))   # cmd.via is "bus" or "p2p"
```

There is nothing to configure on the robot. The STUN and TURN servers are the
fleet-server's configuration ([`docs/INTEGRATION.md`, 7.1](../../docs/INTEGRATION.md)), and
the robot asks for them.

| `ice_servers=` | the peer connection is created with |
|---|---|
| `None` (default) | what the server answers to `ice.request`: the installation's servers with a TURN credential minted for this robot, or none if the installation has none |
| a list, e.g. `[{"urls": "stun:stun.example.org:3478"}]` | exactly that list, every time. The server is never asked |
| `[]` | nothing, whatever the server is configured with. The server is never asked |

An explicit list wins outright; the two are never merged. A robot that names its own
servers is saying it knows better than the installation, which is rare: a bench setup, or
a TURN server only that robot can reach.

| `data_channel=` | behaviour |
|---|---|
| `None` (default) | on if aiortc is installed, off if it is not |
| `True` | on; `Robot(...)` raises `ImportError` if aiortc is missing |
| `False` | off: offers are ignored and the operator stays on the bus |

- **Nothing about authority changes.** The robot answers only the operator named in its
  current lease, for that lease id. Every channel twist passes the same lease check as a
  bus twist and restarts the same 300 ms deadman. A revoke closes the peer connection;
  closing a peer connection never ends a lease.
- **The robot asks once per lease.** It sends `ice.request` when it takes a lease (from
  `lease.granted`, or from the `welcome` of a reconnect that names a lease it did not
  know), so the answer is there before the operator's offer. Every session under that
  lease uses it until 10 s before the credential's `expires_at_ms`; after that the robot
  asks again when the next offer arrives. A new lease always asks afresh.
- **No answer is not a refusal to connect.** If the server does not answer within 2 s,
  refuses, or is too old to know the message (`error{code: invalid_message}`), the peer
  connection is created with no ICE servers. That is enough on one machine or one LAN.
  The next offer asks again.
- **No ICE server is ever assumed.** With none configured anywhere the robot uses host
  candidates only and sends nothing to a third party; there is no default public STUN
  server. Across networks (robot on LTE, operator at home) the installation needs a STUN
  server, and a TURN server where either side is behind a symmetric NAT.
- **A silent ICE server costs two seconds, not the session.** aiortc asks the STUN
  server from every interface address and waits for each. An interface that cannot reach
  it (a VPN, a container bridge) would hold the answer back five seconds, which is the
  console's whole connect timeout. The SDK caps the wait at 2 s (`GATHER_TIMEOUT_S`) and
  answers with what it has.
- **One transport at a time is the operator's job; the robot takes both.** A bus twist is
  ignored if a channel twist was obeyed within the last 300 ms, so a late bus twist cannot
  replace a newer setpoint. Channel twists carry a `seq`; older or repeated ones are
  dropped.
- **Losing the direct link is not a stop by itself.** When the peer connection closes or
  fails, the robot drops that session, keeps the lease, and waits for a new offer. The
  operator falls back to bus twist; if nothing arrives the deadman stops the robot 300 ms
  after the last twist it obeyed, as on any transport.
- **Losing the server link is.** The robot stops at once (`disconnected`) and obeys no
  channel twist until the control connection is back, because it could not hear a revoke.
- **No extra, no change.** Without aiortc, `import fleet` and everything above the data
  channel work as before, and the robot is driven over the bus.

Limits of this implementation: aiortc gathers all its candidates before answering, so the
answer takes a moment longer than a trickling peer's and the robot sends no `ice` signals
(it accepts the operator's). aiortc offers no loopback candidate, so a robot and a console
on the same machine connect through one of that machine's network addresses and need at
least one interface up. Video is not carried yet.

`fleet/webrtc.py` holds the peer (`TwistAnswerer`) and the message parser
(`parse_channel_message`); it is the only module that imports aiortc.

## Tests

Every test runs against the real `fleet-server` binary on an ephemeral port (Go must be
installed; the fixture builds it once per session):

```bash
pip install -e 'sdk/python[dev]'
cd sdk/python && pytest
```

`tests/test_webrtc.py` needs the `webrtc` extra and is skipped without it
(`pip install -e 'sdk/python[dev,webrtc]'` to run it). It drives the robot from a scripted
aiortc offerer through the server's signal relay. The same robot is driven by the
console's own twist transport in `sim/test/pyrobot.interop.test.ts`, which is opt-in:

```bash
cd sim && FLEET_PYTHON=/path/to/venv/bin/python npx vitest run test/pyrobot.interop.test.ts
```
