# Integrating an application with fleet-platform

> Audience: someone building a fleet application on top of this repo. The worked example
> throughout is the club's delivery system,
> [`delivery-gdg-platform`](https://github.com/WUSTL-Delivery/delivery-gdg-platform), which
> is the reference deployment. Everything here is verified against the server as of commit
> `b4faef9` (protocol v0). Where v0 has a gap, the gap is called out rather than papered over.

The one-sentence model: **your app is a client, not a fork.** Robots and your backend
services each hold one outbound WebSocket to `fleet-server`; the platform owns
connections, presence, the intervention queue, leases, and the message bus; your app owns
every domain decision (orders, matching, pathing, the campus graph) and talks to robots
*through* the platform on opaque channels. Nothing delivery-shaped is added to this repo.

```
delivery-gdg-platform (yours)                      fleet-platform (this repo)
┌──────────────────────────────┐                   ┌──────────────────────────────┐
│ command brain  (service) ────┼── wss ───────────►│                              │
│ path service   (service) ────┼── wss ───────────►│   fleet-server               │
│ kafka bridge   (service) ────┼── wss ───────────►│   auth · presence · queue    │
│ Next.js app, orders, Kafka   │  (unchanged)      │   leases · bus · signaling   │
└──────────────────────────────┘                   │                              │
┌──────────────────────────────┐                   │   ops console (browser)      │
│ robot: fleet_agent + thin    ┼── wss ───────────►│                              │
│ club node → Nav2             │                   └──────────────────────────────┘
└──────────────────────────────┘
```

Contents

1. [Run the server locally](#1-run-the-server-locally)
2. [Wire protocol essentials](#2-wire-protocol-essentials)
3. [The four integration roles](#3-the-four-integration-roles)
4. [Worked example: a service in TypeScript](#4-worked-example-a-service-in-typescript)
5. [Worked example: a robot in Python](#5-worked-example-a-robot-in-python)
6. [Migration map for delivery-gdg-platform](#6-migration-map-for-delivery-gdg-platform)
7. [Deploying next to the club stack](#7-deploying-next-to-the-club-stack)
8. [What v0 does not do yet](#8-what-v0-does-not-do-yet)
9. [Testing your integration](#9-testing-your-integration)

---

## 1. Run the server locally

Prerequisites: Go 1.26+, or Docker. No external database (sqlite is embedded).

From source:

```bash
cd server
go build -o bin/fleet-server ./cmd/fleet-server

cat > fleet.yml <<'EOF'
listen: ":8080"
db: "fleet.db"
heartbeat_interval_ms: 10000   # clients heartbeat at this rate; ~2.5 missed => offline
lease_ttl_ms: 15000            # teleop lease expires this long after grant/renew
sweep_ms: 1000                 # how often lapsed heartbeats / expired leases are swept
client_msgs_per_sec: 50        # per connection, telemetry and channel.publish each
client_msgs_burst: 100         # back-to-back allowance before throttling starts
map_center: "38.6488,-90.3108" # optional: console map opens here (lat,lon)
map_radius_m: 1000             # how much around map_center to show
map_lock: true                 # keep the operator's view inside that area
EOF

# First run: create a fleet and print an enrollment key (shown once, stored hashed).
./bin/fleet-server -config fleet.yml -bootstrap club-fleet
#   INFO bootstrap: created fleet fleet=club-fleet id=f_...
#   enrollment key for fleet "club-fleet": fp-ek-...
#   INFO fleet-server listening addr=:8080 db=fleet.db
```

Endpoints:

| Path       | What                                                        |
|------------|-------------------------------------------------------------|
| `/ws`      | the only client endpoint; every robot, service, operator     |
| `/healthz` | `{"ok":true}`                                               |
| `/`        | the ops console, when it is embedded in the build (§8)     |
| `/api/console/config` | the console's installation config, e.g. `{"map": {"center": {"lat", "lon"}, "radius_m", "lock"}}`; public, nothing secret |

Running `-bootstrap` again on an existing fleet mints another enrollment key for it. An
enrollment key is reusable until it is revoked or expires, so treat it as a secret: it
lets anyone register a robot or service into your fleet. Keys are managed at runtime
with `fleetctl` ([FLEETCTL.md](FLEETCTL.md)) against the admin API (`FLEET_ADMIN_TOKEN`, D14), never by editing the
database:

```bash
fleetctl enroll-key create --fleet club-fleet [--ttl 720h]   # no --ttl: never expires
fleetctl enroll-key list   --fleet club-fleet                # id, state, created, expires, revoked
fleetctl enroll-key revoke --fleet club-fleet ek_...         # stops new enrollments with it
```

Revoking or expiring a key only stops **new** enrollments (`enroll.request` answers
`auth_failed`, the same as an unknown key). Clients that already enrolled with it keep
their tokens. Keys minted by `-bootstrap`, by `FLEET_BOOTSTRAP_ENROLL_KEY`, or before
expiry existed never expire.

Revoking a single client (a lost robot, a lost operator laptop) is the per-client
counterpart, through the same admin API:

```bash
fleetctl client list   --fleet club-fleet             # id, kind, name, state, created, revoked (never tokens)
fleetctl client revoke --fleet club-fleet r_...       # kills that one token, now
```

The token stops authenticating at once. If the client is connected, the server sends it
`error{code: auth_failed, message: "token revoked"}` and closes the socket; teardown is the
ordinary disconnect path (a robot goes `robot.offline`, an operator's leases are revoked
with reason `operator_lost`). Every later `hello` with that token gets `auth_failed`, which
both SDKs treat as terminal, so a revoked robot does not reconnect-loop. A revoked robot
also drops out of snapshots. Nothing else in the fleet is touched. There is no un-revoke:
enroll it again as a new client.

From the published image, with no config file (every key has a `FLEET_*` environment
variable; env beats file beats default):

```bash
docker run --rm -p 8080:8080 -v fleet-data:/var/lib/fleet \
  -e FLEET_BOOTSTRAP_FLEET=club-fleet \
  -e FLEET_BOOTSTRAP_ENROLL_KEY=$(openssl rand -hex 24) \
  ghcr.io/<owner>/fleet-server:0.1.0
```

The two `FLEET_BOOTSTRAP_*` variables are the declarative alternative to `-bootstrap`:
you choose the key (at least 16 characters), and on every start the server makes sure the
fleet exists and the key is registered for it. Idempotent, so it belongs in a compose file
or a CI secret; the clients that will enroll get the same value. Revocation wins over
the environment: once that key is revoked with `fleetctl enroll-key revoke`, a restart
does not re-register it, and the server logs a warning that `FLEET_BOOTSTRAP_ENROLL_KEY`
names a revoked key. To rotate, set a new value; the old key keeps working until revoked.

`fleet-server -version` prints the stamped release; `fleet-server -healthcheck` is what
the image's HEALTHCHECK runs. Tags and the release process are in `docs/RELEASING.md`.

---

## 2. Wire protocol essentials

The full contract is `protocol/` (JSON Schema, draft 2020-12) and it is the source of
truth; `protocol/README.md` has the message catalog. This section is the subset you need
to integrate. There is no SDK yet (§8), so today you speak raw JSON over WebSocket. The
shapes below are exactly what the SDKs will wrap.

### 2.1 Envelope

Every message, both directions, is one JSON text frame:

```json
{ "v": 0, "type": "telemetry", "id": "optional-correlation-id", "ts_ms": 1755100000000, "payload": { } }
```

`v` must be `0`. `payload` is always an object. `id` is echoed back as `ref` on any
`error` reply, so set it when you want to correlate failures. Unknown `type` gets an
`error` with code `invalid_message`.

### 2.2 Identity: enroll once, then hello every time

Client identity is derived from a token server-side and never claimed in a message.
Getting a token is a one-shot exchange on its own socket:

```
client                                   fleet-server
  │── enroll.request {enrollment_key, kind, name} ──►│  key → fleet; mints token
  │◄─ enroll.response {token, client_id, fleet_id} ──│
  │◄─ close ─────────────────────────────────────────│  (enroll always closes)
```

`kind` is `robot` or `service`. Store the token (it is shown once). Every later
connection opens with `hello`:

```
  │── hello {token, agent?} ───────────────────────►│  token → client_id, fleet, kind
  │◄─ welcome {client_id, fleet_id, kind,           │
  │            server_time_ms, heartbeat_interval_ms}│
```

Rules enforced by the gateway:

- The first frame must be `enroll.request` or `hello`, within 10 s, or the socket is closed.
- One live connection per identity. A second `hello` with the same token wins and the
  older socket gets `error{code: conflict}` then close. Reconnect with the same token;
  never re-enroll on reconnect.
- A token revoked by an admin (§1, `fleetctl client revoke`) closes its live socket with
  `error{code: auth_failed}` and is refused on every later `hello`. Treat `auth_failed`
  as terminal; do not retry it.
- Client ids are prefixed by kind: `r_…` robot, `s_…` service, `o_…` operator.

### 2.3 Heartbeat or die

After `welcome`, send `{"v":0,"type":"heartbeat","payload":{}}` every
`heartbeat_interval_ms`. Miss ~2.5 intervals and the server sends
`error{code: rate_limited, message: "heartbeat lapsed"}`, closes the socket, and (for a
robot) emits `robot.offline`. Presence is heartbeat-based for *every* kind, including your
backend services. Do not rely on the TCP socket staying open as proof of liveness.

### 2.4 Who may send what

The server checks the client kind on every message. Sending something outside your
kind's column returns `error{code: not_authorized}`.

| Message | robot | service | operator | Notes |
|---|:-:|:-:|:-:|---|
| `heartbeat` | ✓ | ✓ | ✓ | |
| `manifest` | ✓ | | | capability declaration; console renders only what is declared |
| `telemetry` | ✓ | | | fans out as `event{robot.telemetry}` to `telemetry` subscribers |
| `help.request` | ✓ | | | AUTONOMOUS → HELP_REQUESTED; emits `robot.help_requested` |
| `subscribe` | ✓ | ✓ | ✓ | reply is a `snapshot`, then live `event`s |
| `channel.publish` | ✓ | ✓ | ✓ | opaque domain payload, see 2.6 |
| `signal` | ✓ | ✓ | ✓ | WebRTC offer/answer/ice relay to `to`, server stamps `from` |
| `layer.declare` / `layer.update` | | ✓ | | GeoJSON map layers, see 2.7 |
| `lease.claim` / `lease.renew` / `lease.release` | | | ✓ | intervention authority |
| `twist` | | | ✓ | must carry a live `lease_id` the sender holds |

Everything is scoped to the fleet the token belongs to. A `to` target in another fleet
looks identical to a disconnected one: `error{code: not_found}`.

### 2.5 Subscribe: snapshot, then stream

```json
{ "v": 0, "type": "subscribe", "payload": { "topics": ["presence", "events", "telemetry", "channel:edge_report"] } }
```

Topics:

| Topic | You receive |
|---|---|
| `presence` | `event{robot.online}`, `event{robot.offline}` |
| `events` | `event{robot.help_requested}`, `robot.lease_granted`, `robot.lease_released`, `robot.lease_revoked` |
| `telemetry` | `event{robot.telemetry, robot_id, data: <the telemetry payload>}` for every robot in the fleet |
| `layers` | every `layer.declare` / `layer.update` from services in the fleet; the retained ones are replayed right after the snapshot (2.7) |
| `channel:<name>` | `channel.message` for broadcasts on that channel |

The immediate reply is one `snapshot` listing every robot the fleet has ever enrolled,
with `presence`, FSM `state` (`AUTONOMOUS | HELP_REQUESTED | TELEOP`), the `manifest` if
online, and the current `lease` if any. This is what makes a service restartable: rebuild
your world model from the snapshot plus your own database, then apply events. Subscribes
are additive and can be repeated; each one returns a fresh snapshot.

### 2.6 Channels: the only place your domain vocabulary goes

The platform never inspects `data`. Anything shaped like an order, a waypoint list, or an
edge report rides here.

```json
{ "v": 0, "type": "channel.publish",
  "payload": { "channel": "assignment", "to": "r_1a2b3c4d", "data": { "order_id": 17, "waypoints": ["n3","n7","n9"] } } }
```

- Exactly one of `to` (a connected client id in your fleet) or `"broadcast": true`.
- Directed publish to a client that is not connected fails with `not_found`. Nothing is queued.
- Broadcast reaches (a) every client subscribed to `channel:<name>` and (b) every online
  robot whose manifest lists `<name>` in `channels`. The sender is excluded.
- Receivers get `channel.message {channel, from, data}` with `from` stamped by the server.
- Delivery is **at-most-once by design**. If you need an acknowledgement, put a sequence
  number in `data` and have the receiver reply on the same channel. Make assignments
  idempotent on the robot side.
- Payload cap is 64 KB per envelope; the per-client send queue is 64 messages and a client
  that cannot keep up is disconnected rather than allowed to stall the fleet.

Channel names match `^[a-z0-9][a-z0-9_.-]{0,63}$`.

### 2.7 Layers: putting your data on the ops map

A service declares a layer once, then pushes GeoJSON updates. The console renders it
generically according to `style`; there is no layer-specific code in the platform.

```json
{ "v": 0, "type": "layer.declare",
  "payload": { "layer_id": "campus-graph", "kind": "geojson", "title": "Campus waypoint graph",
               "style": { "line-color-by": "properties.eta_band" } } }
{ "v": 0, "type": "layer.update",
  "payload": { "layer_id": "campus-graph", "data": { "type": "FeatureCollection", "features": [] } } }
```

The server keeps the latest `layer.declare` and the latest `layer.update` for each layer
id in the fleet. A client that subscribes to `layers` gets its `snapshot`, then every
retained layer (declare, then latest update, ordered by `layer_id`), then the live stream,
so a console that opens late sees the map without the service re-sending. Retained layers
survive the service disconnecting for up to 5 minutes (a restart or a Wi-Fi blip); after
that they are dropped. Retention is in memory: a server restart forgets them, so a
service should still re-declare and re-send when it (re)connects.

### 2.8 Errors

`error{code, message, ref}` with `code` one of `auth_failed`, `invalid_message`,
`not_found`, `not_authorized`, `conflict`, `rate_limited`. `ref` is the `id` of the
message that caused it when there was one. Errors do not close the socket except during
the handshake, on heartbeat lapse (`rate_limited`), on takeover by a newer connection
(`conflict`), and on token revocation (`auth_failed`); in those cases the error is the
last frame before the close.

`telemetry` and `channel.publish` are rate limited per connection, each type with its own
token bucket (`client_msgs_per_sec`, `client_msgs_burst`; defaults 50/s and 100). Over the
limit, the server drops the message and replies `error{code: rate_limited, ref}` naming
it, at most once per second however many it drops; the socket stays open. Nothing else
(heartbeats, leases, subscribe, signaling) is throttled. Treat the notice as "send less
often": telemetry is latest-wins, so there is nothing to resend.

---

## 3. The four integration roles

This is how the pieces of delivery-gdg-platform map onto platform client kinds. The design
rationale is `docs/DESIGN.md` D6 and `docs/proposals/control-server.md` §2 to §8; this
section is the operational version.

### 3.1 Command brain (kind: `service`)

Replaces `apps/command` and the socket hub half of `apps/authoritative`. One outbound
WebSocket, no inbound listener.

| It does | On the wire |
|---|---|
| Learn which robots exist and are online | `subscribe ["presence","events","telemetry"]` → snapshot, then events |
| Track pose for progress / ETA judgement | `event{robot.telemetry}` |
| Know when a robot is in teleop (do not dispatch to it) | `robot.lease_granted` / `robot.lease_released` / `robot.lease_revoked` |
| Dispatch an assignment | `channel.publish {channel:"assignment", to: robot_id, data: {…}}` |
| Confirm the robot took it | robot replies on the same channel; brain re-sends until it does |
| Replan after handback | on `robot.lease_released`, robot reports leg invalidated on a channel; brain routes fresh |

The delivery FSM (`IDLE → ASSIGNED → MOVING_TO_PICKUP → …`) stays in the brain, branched
from `apps/authoritative/internal/state/`. The platform FSM
(`AUTONOMOUS | HELP_REQUESTED | TELEOP`) is separate and the two never merge: a robot can be
`MOVING_TO_DROPOFF` and `TELEOP` at the same moment. Retire the state manager's own
`IsOnline` field; presence now has one source of truth.

### 3.2 Path service (kind: `service`)

| It does | On the wire |
|---|---|
| Publish the campus graph for operators to see | `layer.declare {layer_id:"campus-graph", kind:"geojson", style}` then `layer.update` on every weight change |
| Learn actual traversal times | `subscribe ["channel:edge_report"]`, receive `channel.message` per completed leg |

Whether the brain calls the path service over Kafka, gRPC, or in-process is the club's
choice; that is club-to-club traffic and the platform is not involved.

### 3.3 Kafka bridge (kind: `service`)

Roughly thirty lines. Subscribes to `presence`, `events`, `telemetry`, and whichever
channels the club wants mirrored, and produces to the existing Kafka topics
(`robot-update` today) for consumers that stay on Kafka. One direction only in v1:
platform → Kafka. Do not replay Kafka into `channel.publish`; a durable log feeding an
at-most-once command bus re-sends stale commands at robots after a restart. Anything that
needs to command a robot uses the platform directly.

### 3.4 Thin club node on the robot (kind: `robot`, via `fleet_agent`)

The generic `fleet_agent` (planned, `sdk/ros2`, Python) owns the socket, manifest,
heartbeat, twist to `/cmd_vel`, lease check, and deadman. The club node is the ~50 lines
that make it a delivery robot:

| It does | On the wire |
|---|---|
| Declare which channels it speaks | manifest `channels: ["assignment", "edge_report"]` |
| Receive an assignment and run it leg by leg on Nav2 | `channel.message {channel:"assignment"}` |
| Acknowledge the assignment | `channel.publish {channel:"assignment", to: <from>, data:{ack: seq}}` |
| Report each leg | `channel.publish {channel:"edge_report", broadcast:true, data:{edge_id, seconds, blocked?}}` |
| Escalate when a leg fails (policy is club code) | `help.request {reason, context}` |
| Stop autonomy on takeover | on `lease.granted`, cancel the active Nav2 goal; on `lease.revoked`, report leg invalidated and wait for a new assignment |

Until `fleet_agent` exists, the robot side is the Python SDK used directly, as in §5.

---

## 4. Worked example: a service in TypeScript

A minimal command brain against the current server, no dependencies (Node 22 has a global
`WebSocket`). It enrolls if it has no token, connects, subscribes, and dispatches an
assignment to every robot that comes online. This is the shape the future
`sdk/typescript` will wrap; the SDK will not change any message on the wire.

```ts
// brain.ts — run with: node --experimental-strip-types brain.ts
// env: FLEET_URL=ws://localhost:8080/ws  FLEET_ENROLL_KEY=fp-ek-...  (first run)
//      FLEET_TOKEN=fp-tk-...                                         (after that)
type Envelope = { v: 0; type: string; id?: string; ts_ms?: number; payload: any };

const url = process.env.FLEET_URL ?? "ws://localhost:8080/ws";

function envelope(type: string, payload: unknown, id?: string): string {
  return JSON.stringify({ v: 0, type, id, ts_ms: Date.now(), payload } satisfies Envelope);
}

// One-shot: enrollment key → token. The server closes the socket after replying.
async function enroll(key: string): Promise<string> {
  return new Promise((resolve, reject) => {
    const ws = new WebSocket(url);
    ws.onopen = () => ws.send(envelope("enroll.request", { enrollment_key: key, kind: "service", name: "brain" }));
    ws.onmessage = (m) => {
      const env: Envelope = JSON.parse(String(m.data));
      if (env.type === "enroll.response") resolve(env.payload.token);
      else reject(new Error(`enroll failed: ${JSON.stringify(env.payload)}`));
    };
    ws.onerror = reject;
  });
}

async function main() {
  const token = process.env.FLEET_TOKEN ?? (await enroll(process.env.FLEET_ENROLL_KEY!));
  console.log("token (store this):", token);

  const ws = new WebSocket(url);
  let heartbeat: ReturnType<typeof setInterval> | undefined;
  const online = new Map<string, unknown>(); // robot_id → manifest (the world model, tiny)

  ws.onopen = () => ws.send(envelope("hello", { token, agent: { name: "brain", version: "0.0.1" } }));

  ws.onmessage = (m) => {
    const env: Envelope = JSON.parse(String(m.data));
    switch (env.type) {
      case "welcome":
        heartbeat = setInterval(() => ws.send(envelope("heartbeat", {})), env.payload.heartbeat_interval_ms);
        ws.send(envelope("subscribe", { topics: ["presence", "events", "telemetry", "channel:edge_report"] }));
        break;

      case "snapshot": // rebuild the world model; then events keep it current
        console.log("snapshot:", env.payload.robots.map((r: any) => `${r.robot_id}:${r.presence}/${r.state}`));
        for (const r of env.payload.robots) {
          if (r.presence !== "online") continue;
          online.set(r.robot_id, r.manifest);
          if (r.state === "AUTONOMOUS") dispatch(r.robot_id); // robots that were already up when we (re)started
        }
        break;

      case "event":
        switch (env.payload.event) {
          case "robot.online":
            online.set(env.payload.robot_id, null);
            dispatch(env.payload.robot_id);
            break;
          case "robot.offline":
            online.delete(env.payload.robot_id);
            break;
          case "robot.help_requested":
          case "robot.lease_granted":
          case "robot.lease_released":
          case "robot.lease_revoked":
            console.log("ops:", env.payload.event, env.payload.robot_id, env.payload.data);
            break;
          case "robot.telemetry":
            // env.payload.data.pose is frame-relative: check pose.frame before reading lat/lon
            break;
        }
        break;

      case "channel.message": // e.g. edge_report broadcasts, or an assignment ack
        console.log("channel", env.payload.channel, "from", env.payload.from, env.payload.data);
        break;

      case "error":
        console.error("server error:", env.payload);
        break;
    }
  };

  ws.onclose = () => { clearInterval(heartbeat); /* reconnect with backoff, same token */ };

  // Domain payload; the platform never looks inside `data`. Use `id` so a failure
  // (robot went offline between event and publish) comes back with `ref`.
  function dispatch(robotId: string) {
    const assignment = { seq: Date.now(), order_id: 17, waypoints: ["n3", "n7", "n9"], deadline_ms: Date.now() + 600_000 };
    ws.send(envelope("channel.publish", { channel: "assignment", to: robotId, data: assignment }, `assign-${robotId}`));
  }
}

main();
```

What is deliberately missing, because it belongs in your code: retrying an assignment until
the robot acks it on the channel, and the delivery state machine. What is deliberately
missing because the SDK will provide it: reconnect with backoff, a typed event emitter, a
request/ack helper for channels.

---

## 5. Worked example: a robot in Python

The robot side uses the Python SDK in `sdk/python` (`pip install -e sdk/python` from a
checkout; not on PyPI yet). It owns the socket, enrollment, heartbeat, reconnect, the
manifest, the lease check on twist, and the deadman. This is what `fleet_agent` will
build on; the club node is the `on_assignment` and leg-reporting parts.

The generic, runnable version (no ROS, no delivery vocabulary) is
[`sdk/python/examples/robot.py`](../sdk/python/examples/robot.py), and
[`sdk/python/README.md`](../sdk/python/README.md) walks through running it against a
local server. Here is the same shape as the club robot would use it:

```python
# robot.py — env: FLEET_URL, FLEET_ENROLL_KEY (first run only)
import asyncio, os
from fleet import FileTokenStore, Robot, geo_pose

robot = Robot(
    os.environ.get("FLEET_URL", "ws://localhost:8080/ws"),
    # Capability manifest: the console renders exactly this, nothing more.
    manifest={
        "drive": {"type": "twist", "max_v_mps": 1.5, "max_w_radps": 2.0},
        "cameras": [{"id": "front", "label": "RealSense RGB"}],
        "battery": {},
        "channels": ["assignment", "edge_report"],   # domain channels the club node speaks
    },
    name="delivery-01",
    enrollment_key=os.environ.get("FLEET_ENROLL_KEY"),         # used once
    token_store=FileTokenStore("/var/lib/fleet_agent/token.json"),  # same robot id across restarts
)
assignment = robot.channel("assignment")
edge_report = robot.channel("edge_report")

async def on_assignment(sender, data):
    # club node: ack, then run Nav2 legs. Idempotent on data["seq"].
    await assignment.publish({"ack": data["seq"]}, to=sender)
    # ... per completed leg:
    await edge_report.publish({"edge_id": "e12", "seconds": 41.5})   # broadcast
    # ... when a leg fails past the club's escalation policy:
    await robot.request_help("nav_goal_failed", {"attempts": 3})

assignment.on_message(on_assignment)
# Lease-gated, fail-closed twist: operator setpoints, plus zero-velocity stops from the
# deadman (300 ms without a valid twist), a revoke, or a lost link.
robot.on_twist(lambda cmd: cmd_vel.publish(cmd.linear_x, cmd.angular_z))
# Granted: cancel the active Nav2 goal. Revoked: report the leg invalidated, await a new assignment.
robot.on_lease(lambda change: nav.cancel() if change.granted else nav.invalidate_leg())

async def main():
    await robot.connect()
    async def telemetry():   # ~1 Hz is plenty for the control plane; video never rides this socket
        while True:
            await robot.telemetry(pose=geo_pose(38.6488, -90.3108, yaw_rad=1.57), battery=87.5,
                                  velocity={"v_mps": 0.0, "w_radps": 0.0}, health={"gps_fix": "rtk_fixed"})
            await asyncio.sleep(1)
    asyncio.create_task(telemetry())
    await robot.run_forever()   # heartbeats; reconnects with backoff, same token

asyncio.run(main())
```

`cmd_vel` and `nav` stand in for the ROS side. What the SDK already does, so the club node
does not: re-sends the manifest and renews channel subscriptions on every reconnect,
drops telemetry while the link is down instead of queueing it, ignores twist that does
not carry the current lease id, and stops the base locally on deadman, revoke, or
disconnect without waiting for the server. A `conflict` (a second process using the same
token) or `auth_failed` (revoked token, wrong key) is terminal and `run_forever()` raises
it.

### 5.1 Wire level, for other languages

The SDK adds nothing to the wire. A robot in another language speaks §2 directly:
`enroll.request` once on a throwaway socket and keep the token; then on every connect
`hello` → `welcome`, `manifest`, a `heartbeat` every `welcome.heartbeat_interval_ms`,
`telemetry` as it likes, and `help.request` to raise its hand. It obeys `twist` only while
`lease_id` matches the last `lease.granted` for it, zeroes velocity ~300 ms after the last
valid twist and immediately on `lease.revoked` or a dropped socket, and reconnects with
backoff using the same token. `sdk/python/fleet/robot.py` and `client.py` are a readable
reference implementation (about 1,000 lines together, mostly comments and edge cases); the schemas in `protocol/schemas/`
and `TestIntegrationStoryline` (§9) are the authority.

---

## 6. Migration map for delivery-gdg-platform

What in the club repo maps to what on the platform. This is D6's table made concrete.

| In `delivery-gdg-platform` today | Becomes |
|---|---|
| `internal/wsockets/` hub, `Message{type, payload}` | the platform envelope; hub deleted |
| `apps/command` (TCP/UDP relay demo) | deleted; the brain is a `service` client |
| `robot.proto` `PositionUpdate{latitude, longitude, heading, speed}` | `telemetry.pose {frame:"geographic", lat, lon, yaw_rad}` + `telemetry.velocity.v_mps`. Heading in degrees becomes yaw in radians, CCW, 0 = East |
| `robot.proto` `BatteryUpdate{battery_level, is_charging}` | `telemetry.battery {pct}`; `is_charging` goes in `telemetry.health` or a channel, it is not platform vocabulary |
| `robot.proto` `StatusUpdate{RobotStatus}` (IDLE, ASSIGNED, …) | **stays club-side.** Robot publishes it on a channel (e.g. `delivery_status`); the brain's FSM consumes it |
| `RobotMatch{robot_id, order_id}` over the hub | `channel.publish {channel:"assignment", to: robot_id, data}` from the brain |
| Kafka `robot-update` topic fed by the hub | the bridge service, fed by `presence` + `telemetry` subscriptions |
| `internal/state/` manager (`RobotState.IsOnline`, `GetAvailableRobots`) | stays as the brain's world model; `IsOnline` retired in favor of `snapshot` + `robot.online/offline` |
| Robot rows in `pkg/db.go` | platform DB (fleets, clients, tokens). Club DB keeps users, orders, deliveries |
| `apps/authoritative` port 8080 `/ws` | `fleet-server` `/ws` |
| Matcher, Next.js app, orders, gRPC `OrderHandler` | unchanged |

The migration is scheduled for roadmap step 4, after the vertical slice and queue work are
done against the sim, so protocol churn does not land on the club mid-semester.

---

## 7. Deploying next to the club stack

The club's `deployments/docker-compose.yml` already runs Caddy in front of the Next.js app.
`fleet-server` is one more stateful container behind it, consumed as a **prebuilt image**
from this repo's CI (image coupling: the tag in compose is the club's pin, bumped in a
reviewed commit; see `docs/RELEASING.md`). This is what the club repo now carries:

```yaml
  fleet-server:
    image: ${FLEET_SERVER_IMAGE:-ghcr.io/jaximus808/fleet-server:0.1.0}
    restart: unless-stopped
    environment:
      FLEET_LISTEN: ":8080"
      FLEET_DB: /var/lib/fleet/fleet.db
      FLEET_HEARTBEAT_INTERVAL_MS: "10000"
      FLEET_LEASE_TTL_MS: "15000"
      FLEET_BOOTSTRAP_FLEET: club-fleet
      FLEET_BOOTSTRAP_ENROLL_KEY: ${FLEET_ENROLL_KEY:?}   # same secret the clients enroll with
    volumes:
      - fleet-data:/var/lib/fleet       # sqlite: fleets, clients, tokens
    healthcheck:
      test: ["CMD", "fleet-server", "-healthcheck"]
      interval: 15s
    # no host port in prod; Caddy proxies to it
volumes:
  fleet-data:
```

Set `FLEET_SERVER_IMAGE=fleet-server:dev` in `.env` to run a locally built image instead
of the pin (`docker build -f server/Dockerfile -t fleet-server:dev .` from the repo root).

Caddy: add a host for the fleet and terminate TLS there. The server speaks plain
WebSocket; robots and services dial `wss://fleet.<domain>/ws`.

```caddy
fleet.{$DOMAIN_NAME} {
	reverse_proxy fleet-server:8080
}
```

Notes:

- `apps/authoritative` also binds host port 8080 for its own `/ws`. Do not publish
  `fleet-server` on the host at all; let Caddy route by hostname.
- Exactly one `fleet-server` replica. It is stateful (presence, leases live in memory).
  Recovery is restart; robots redial with backoff and stuck robots wait in
  `HELP_REQUESTED`. Do not scale it horizontally.
- Teleop video needs a TURN server (`coturn`) beside it once WebRTC media lands. Not
  needed for anything in this document.
- The enrollment key is generated by the operator (`openssl rand -hex 24`), stored as
  the club's `FLEET_ENROLL_KEY` secret, and seeded into fleet-server via the
  `FLEET_BOOTSTRAP_*` variables. No one-time command on the box.
- The club's first client is `apps/fleet-bridge` in delivery-gdg-platform: it enrolls
  with that key once, keeps its token on a volume, and forwards presence, telemetry and
  intervention events to Kafka (§3.3).

---

## 8. What v0 does not do yet

Read this before designing against the server. Each item is a known gap, not a hidden one.

| Gap | Consequence for you | Where it lands |
|---|---|---|
| **SDKs are source-only, and there is no ROS 2 node yet.** `sdk/typescript` and `sdk/python` exist (§5), but neither is published to npm or PyPI, and `fleet_agent` (`sdk/ros2`) is not written | install from a checkout (`pip install -e sdk/python`); a ROS robot wires the Python SDK's `on_twist` / `on_lease` / `telemetry` to its topics by hand until `fleet_agent` does it | package publishing with the project rename; `fleet_agent` in the real-hardware phase |
| **Operator access is invite-only, from the CLI.** Operators redeem a single-use, expiring invite minted by `fleetctl invite operator` (D14), which needs the server's `FLEET_ADMIN_TOKEN`. There are no passwords, no roles, and no way to mint an invite from the console | whoever holds the admin token onboards every operator; an operator's token lives in their browser, and losing it means a new invite (revoke the lost one with `fleetctl client revoke`) | console-side invites through the same admin API, later |
| **The console is basic.** It shows robots live on a map, renders what each manifest declares, and does take over / WASD / hand back. It does not render declared map layers yet, and a plain `go build` does not include it (`npm run embed` in `console/`, or the Docker image) | a service's layers have nowhere to show yet; build the image or embed the console before pointing an operator at `/` | layer rendering with the layer-streams work |
| **Layer retention is in memory only** | the server replays each layer's latest declare and update to late `layers` subscribers, but a server restart forgets them, and a service's layers are dropped 5 minutes after it disconnects, so re-declare and re-send on every (re)connect | persisting layers in the store, if a deployment needs it |
| **Rate limits are per connection and per type, not per subscriber** | a chatty `telemetry` or `channel.publish` loop is throttled (§2.8), not disconnected; a slow *reader* still overflows its 64-deep send queue and is dropped | per-subscriber fan-out limits, later |
| **Twist rides the bus as a fallback**; no WebRTC media, no TURN | fine for sim and for testing the club node's lease handling; not for driving a real robot | roadmap step 3 |
| **No replay / black-box recording** | | later milestone |
| **Snapshot lists robots only** | you cannot discover other services or operators from a snapshot | if a real need appears |
| **Placeholders**: Go module path `fleetplatform/server`, schema `$id` host `fleetplatform.local` | do not hard-code either in club code | project rename |

---

## 9. Testing your integration

- **Contract tests.** `protocol/fixtures/valid/*.json` must all validate and
  `protocol/fixtures/invalid/*.json` must all fail, in every language that touches the
  wire. Run them against your own serializer before trusting it; the server's Go types
  already run them (`sdk/go/protocol/contract_test.go`). Add a fixture whenever you find a
  shape the schemas do not pin down.
- **The reference storyline.** `server/internal/app/integration_test.go`
  (`TestIntegrationStoryline`) is the canonical end-to-end flow: enroll, connect, manifest,
  subscribe, telemetry, help, claim, twist, signaling, handback, channel broadcast,
  offline. It runs the real server on an ephemeral port with fake JSON clients. If your
  client and that test disagree about a message, the test is right.
- **Run it yourself.** `cd server && go test ./...` takes a few seconds and needs nothing
  installed beyond Go.
- **Local two-terminal check.** Start the server (§1), run §5's robot in one terminal and
  §4's brain in another. You should see the brain's snapshot list the robot online, then a
  `channel.message` ack come back after it dispatches. Kill the robot and the brain sees
  `robot.offline` within about 2.5 heartbeat intervals.

The broader verification plan, including the planned SDK-level suites that launch the
built binary, is `docs/TESTING.md`.
