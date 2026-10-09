# protocol/ — v0

**Source of truth for everything on the wire.** Server, console, SDKs, and sim validate
against these schemas (JSON Schema draft 2020-12); language types are generated from or
hand-mirrored to them and kept honest by the shared fixtures (see `docs/TESTING.md`,
layer 1). A schema mentioning delivery/campus/order vocabulary is a failed extraction
(DESIGN.md D7) — domain payloads ride opaque channels only.

> `$id` host `fleetplatform.local` is a placeholder until the project is named.

## Envelope

Every control-plane message is one JSON envelope over WSS:

```json
{ "v": 0, "type": "telemetry", "id": "optional-correlation-id",
  "ts_ms": 1755100000000, "payload": { … } }
```

`catalog.json` maps each `type` to the schema for its `payload`. Unknown types are
rejected for typed clients; domain traffic never invents envelope types — it rides
`channel.publish` / `channel.message` with an opaque `data`.

## Message catalog (v0)

| type | direction | purpose |
|---|---|---|
| `enroll.request` / `enroll.response` | client → server / reply | one-time: enrollment key → per-client token (Tailscale auth-key pattern) |
| `hello` / `welcome` | client → server / reply | authenticate; identity is **derived from the token, never claimed**. A robot's `welcome` also states the lease it holds (see [A robot's lease at connect](#a-robots-lease-at-connect)) |
| `heartbeat` | client → server | presence; silence ⇒ offline (welcome carries the interval) |
| `manifest` | robot → server | capability declaration; console renders only what's declared |
| `telemetry` | robot → server | frame-relative pose, velocity, battery, health |
| `help.request` | robot → server | AUTONOMOUS → HELP_REQUESTED; enters intervention queue |
| `subscribe` / `snapshot` | client → server / reply | **snapshot-then-stream**: current fleet state, then live events |
| `event` | server → subscribers | typed lifecycle events (`robot.online`, `robot.help_requested`, `operator.online`, …) |
| `lease.claim` / `lease.renew` / `lease.release` | operator → server | authority requests; every transition is server-side. A claim takes a robot from another operator only with `steal: true` (see [Claiming a lease](#claiming-a-lease)) |
| `lease.granted` / `lease.revoked` | server → operator+robot | the lease itself; steal/expiry/operator-loss arrive as `revoked` |
| `watch` | operator → server | which robot the operator is looking at, or `null` for none; the fleet hears it as `operator.watching` (see [Watching a robot](#watching-a-robot)) |
| `twist` | operator → robot | body-frame setpoint **carrying lease_id**; rides the WebRTC datachannel (bus fallback in sim), robot rejects without current lease |
| `signal` | relayed via server | WebRTC offer/answer/ICE; sender sets `to`, server stamps `from` |
| `ice.request` / `ice.config` | robot or operator → server / reply | the installation's STUN/TURN servers with a short-lived TURN credential for the asker (see [ICE servers](#ice-servers)) |
| `channel.publish` / `channel.message` | service ↔ server ↔ client | opaque domain channels (assignments, edge reports); at-most-once (see [Acked send](#acked-send-convention-on-channel-data)) |
| `layer.declare` / `layer.update` | service → server | map layers (GeoJSON first); console renders generically |
| `error` | server → client | `auth_failed`, `invalid_message`, `not_found`, `not_authorized`, `conflict`, `rate_limited`; a refused `lease.claim` also carries the `lease` in the way |

## Conventions

- **Pose is frame-relative, never assumed lat/lon** (D7): `{"frame": "geographic", lat,
  lon, alt_m?, yaw_rad?}` or `{"frame": "local", frame_id?, x_m, y_m, z_m?, yaw_rad?}`.
  Yaw is radians, CCW; 0 = +x for local frames, 0 = East (ENU) for geographic.
- Units in field names: `_m`, `_mps`, `_radps`, `_ms` (epoch millis / durations).
- Robot FSM: `AUTONOMOUS | HELP_REQUESTED | TELEOP` — platform vocabulary only.
- Versioning: `v` bumps only on incompatible envelope change; message-level evolution is
  additive (new optional fields) until a type is redesigned under a new name.

## Claiming a lease

```json
{ "v": 0, "type": "lease.claim", "id": "claim-1", "payload": { "robot_id": "r_1a2b3c4d" } }
{ "v": 0, "type": "lease.claim", "id": "claim-2", "payload": { "robot_id": "r_1a2b3c4d", "steal": true } }
```

`steal` is optional; leaving it out is the same as `false`. What a claim does depends on
who holds the robot's lease when the server handles it:

| the robot's lease is held by | plain claim | claim with `steal: true` |
|---|---|---|
| nobody | granted | granted |
| the claiming operator | granted: a new lease replaces the old one | same |
| another operator of the fleet | **refused**: `error{code: conflict}`, nothing changes | granted: the holder's lease is revoked (`reason: "stolen"`) and a new one is issued |

- **A refused claim changes nothing and tells nobody else.** No `lease.revoked`, no
  `lease.granted`, no event. The holder keeps driving and never learns of it.
- **Two claims at the same moment.** When two operators send a plain claim for the same
  free robot, exactly one is granted and the other gets the conflict. Neither can take
  the robot from the other by accident; taking it is always the explicit `steal`.
- **The conflict names the lease in the way**, so a console can say who is driving
  without a fresh snapshot:

  ```json
  { "code": "conflict", "message": "robot is leased to another operator", "ref": "claim-1",
    "lease": { "lease_id": "ls_7f8e9d0c", "robot_id": "r_1a2b3c4d",
               "operator_id": "o_9c8d7e6f", "expires_at_ms": 1755100015000 } }
  ```

  `lease` is the same object the holder was granted and that `snapshot` lists on the
  robot. It is set on this one error and no other. `ref` is the claim's envelope `id`,
  absent when the claim had none.
- **This conflict is a reply, not a disconnect.** The server also sends
  `error{code: conflict}` as the last frame before it closes a connection that a newer
  one replaced. That notice has neither `ref` nor `lease`; a client tells the two apart
  by those fields and MUST NOT treat a refused claim as the end of its connection.
- **Other fleets.** A claim on a robot of another fleet is `not_found`, with or without
  `steal` and whether or not the robot is leased, exactly like an id that does not exist.
  It never gets the conflict, so a lease never leaks across fleets.
- A lease past its `expires_at_ms` still counts as held until the server's sweep revokes
  it (`reason: "expired"`, within `sweep_ms`); a plain claim in that gap is refused.

**Changed in place.** Before `steal` existed, every claim on a held robot stole it. A
client that relied on that now gets the conflict and has to send `steal: true` to take
over from another operator. Claims on a free robot, renewals and handback are unchanged.

Golden examples: `fixtures/valid/lease-claim.json`, `fixtures/valid/lease-claim-steal.json`,
`fixtures/valid/error-claim-conflict.json`.

## A robot's lease at connect

The server does not end a lease because the robot's connection dropped: a Wi-Fi blip
must not end a takeover. But a lease can end while the robot is away (handback, expiry,
the operator leaving), and the `lease.revoked` sent at that moment reaches no robot. So
every `welcome` to a robot states the lease the server holds for it as that connection
begins:

```json
{ "client_id": "r_1a2b3c4d", "fleet_id": "f_00000001", "kind": "robot",
  "server_time_ms": 1755100040123, "heartbeat_interval_ms": 10000,
  "lease": { "lease_id": "ls_7f8e9d0c", "robot_id": "r_1a2b3c4d",
             "operator_id": "o_9c8d7e6f", "expires_at_ms": 1755100048000 } }
```

`lease` is the same object as `lease.granted`, or `null` when the server holds no lease
for the robot. The server sends the member on every robot welcome, the first one included.
A welcome to an operator or a service has no `lease` member.

**The robot replaces what it believed with what the welcome says.** MUST, in every case:

| `welcome.lease` | the robot believed it held | the robot |
|---|---|---|
| a lease | that same `lease_id` | keeps it and is driven again |
| a lease | nothing, or a different lease | takes it exactly as it takes `lease.granted`: zero velocity first, any peer connection of the old lease closed |
| `null` | a lease | drops it exactly as on `lease.revoked`: zero velocity, peer connection closed, lease forgotten |
| `null` | nothing | nothing to do |
| member absent | anything | same as `null` |

- **A robot never trusts its own memory of a lease across a reconnect.** From the moment
  its control connection is lost until the next `welcome`, it obeys no twist on either
  transport (there is no bus, and channel twist is refused while the connection is not
  open; see [the data channel](#the-data-channel)). The stop at the moment of loss is
  immediate and waits for nothing. Only being driven again waits, and it waits for the
  one message that proves the lease is still the server's.
- **A welcome without the member confirms nothing.** A server older than this field
  sends none, so a robot that reconnects to one holds no lease afterwards and its
  operator has to claim again. That is the price of failing closed; the alternative is a
  robot that obeys a lease nobody can vouch for.
- **Nothing crosses the welcome.** The server orders each robot welcome with every
  grant and revocation of that robot's lease. No `lease.granted` or `lease.revoked`
  for the robot is sent on a connection before its `welcome`, the welcome is exact as
  of its place in the stream, and each one that follows applies on top of it in order.
  A robot that reads its connection in order always ends up holding what the server
  holds.
- **`null` does not say why.** The reason was announced while the robot was away, in
  the fleet's `robot.lease_revoked` or `robot.lease_released`. The welcome carries
  neither the reason nor the robot's intervention state, so a robot cannot tell a
  handback from an expiry here. The SDKs' robots treat it as the cautious one: they
  count themselves as needing help, not as handed back.
- **A robot cannot be claimed while it is offline** (`error{code: not_found}`), so a
  welcome that names a lease the robot does not know is rare: the robot process
  restarted while it was leased, or the grant was sent in the instant its connection
  dropped.
- The deadman is untouched by any of this: 300 ms after the last twist it obeyed, the
  robot stops, whatever its lease.

**The fleet hears a revocation whether or not the robot is connected.**
`robot.lease_revoked` goes to the `events` subscribers of the robot's fleet for every
revocation (`expired`, `operator_lost`, `stolen`), and `robot.lease_released` for every
handback, including while the robot is offline. A subscriber therefore sees the robot
return to the queue the moment it happens, with the `help` entry in the event, and MUST
accept these events for a robot whose presence is `offline`.

Golden examples: `fixtures/valid/welcome-robot-lease.json`,
`fixtures/valid/welcome-robot-no-lease.json`, `fixtures/valid/welcome.json` (no member:
not a robot's, or an older server's).

## Help details (the intervention queue entry)

While a robot is `HELP_REQUESTED`, its `snapshot` entry carries `help`:

```json
{ "reason": "nav_goal_failed", "context": { "attempts": 3 }, "requested_at_ms": 1755100002000 }
```

- `reason` and `context` are the robot's `help.request` payload, verbatim. `context` is
  omitted when the robot sent none.
- `requested_at_ms` is epoch milliseconds on the **server's** clock at the moment the
  robot entered the queue. Order the queue by it, oldest first.
- `help` is absent in every other state (`AUTONOMOUS`, `TELEOP`).

The same object arrives live, so a subscriber never needs a fresh snapshot to build or
order the queue:

| message | where | when |
|---|---|---|
| `event` `robot.help_requested` | `data` is the help object | the robot raised its hand |
| `event` `robot.lease_revoked` | `data.help` | the revocation put the robot back in the queue (`expired`, `operator_lost`) |
| `lease.revoked` (to robot and operator) | `help` | same |

A robot that returns to the queue after a claim (lease expiry, operator loss) keeps its
**original** `reason`, `context` and `requested_at_ms`, so it sorts ahead of robots that
asked later. Handback (`lease.release`) closes the entry; the next `help.request` opens a
new one with a new time. A steal (`reason: "stolen"`) leaves the robot in `TELEOP` and
carries no `help`.

If the robot never asked (an operator claimed it proactively and then lost it), the
server opens the entry itself: `reason` is the revocation reason (`expired` or
`operator_lost`), there is no `context`, and `requested_at_ms` is the time of the
revocation.

The server rejects a `help.request` whose `reason` is empty or longer than 256 characters
with `invalid_message`.

## Operator presence

Who is at a console, and which robot each of them is looking at. A `snapshot` lists the
fleet's operators next to its robots, and the `presence` topic carries `operator.online` /
`operator.offline` as they come and go and `operator.watching` as they move between robots.

### The operator entry

```json
{ "operator_id": "o_9c8d7e6f", "name": "ada", "online": true, "watching": "r_1a2b3c4d" }
```

- `operator_id` is the operator's client id, the same id a lease carries as `operator_id`.
- `name` is the name given at enrollment; omitted when there is none.
- `online` is true while the operator has a live connection.
- `watching` is the id of the robot the operator is looking at (see
  [Watching a robot](#watching-a-robot)). It is omitted, never `null`, when the operator
  is watching none, and it is never set while `online` is false.
- The entry only ever gains fields. Consumers MUST keep working when one they do not know
  appears, and SHOULD replace their whole stored entry with each one they receive.

### In the snapshot

`snapshot.operators` lists every operator of the fleet that is not revoked, online or
not, oldest enrollment first. The server always sends it (an empty array when the fleet
has no operators); the schema leaves it optional so that a snapshot from an older server
still validates. The subscriber, if it is an operator, is in the list itself.

### Events

All three events (`operator.online`, `operator.offline`, `operator.watching`) ride the
`presence` topic and have the same shape:

```json
{ "event": "operator.online", "operator_id": "o_9c8d7e6f",
  "data": { "operator_id": "o_9c8d7e6f", "name": "ada", "online": true } }
```

- **An event names its subject with `robot_id` or `operator_id`, never both.** Every
  `robot.*` event carries `robot_id` and no `operator_id`; every `operator.*` event
  carries `operator_id` and no `robot_id`. A consumer that looks up `robot_id` on every
  event must first check the event name (or that `robot_id` is present).
- `data` is the operator entry as of the event, exactly as a snapshot taken at that
  moment would list it. `operator.offline` carries it with `online: false`. Upsert it by
  `operator_id`; no fresh snapshot is needed, including for an operator who enrolled
  after the subscriber's snapshot. The entry is whole every time: one that arrives
  without `watching` means the operator is watching nothing now.
- Like all events they go to the operator's own fleet only.

### What "online" means

- One live connection per client id: a second connection with the same token replaces
  the first, which is closed with `error{code: conflict}`. Two browser tabs signed in as
  the same operator are therefore one operator with one connection, the newer tab's. The
  replacement emits **no** event: the operator never went offline. `operator.offline` is
  sent only when the operator's current connection ends (socket closed, heartbeat
  lapsed, token revoked), and `operator.online` only when a connection arrives for an
  operator who had none.
- An operator does not receive its own `operator.online`: it is not subscribed yet when
  it connects. It finds itself in its snapshot.
- A second connection does replace what the operator is watching; see
  [Watching a robot](#watching-a-robot).
- A revoked operator gets a last `operator.offline` and is absent from later snapshots.
- `operator.offline` is also the moment the server revokes that operator's leases
  (`robot.lease_revoked`, reason `operator_lost`, on the `events` topic).

Golden examples: `fixtures/valid/snapshot.json`, `fixtures/valid/event-operator-online.json`,
`fixtures/valid/event-operator-offline.json`.

### Watching a robot

An operator tells the server which robot it has open, so that everyone else in the fleet
can see who is looking at what:

```json
{ "v": 0, "type": "watch", "id": "watch-1", "payload": { "robot_id": "r_1a2b3c4d" } }
{ "v": 0, "type": "watch", "payload": { "robot_id": null } }
```

`robot_id` is required: a robot id to watch that robot (replacing whatever the operator
watched before, one robot at a time), or `null` to watch none. Watching is presence only.
It grants nothing, the robot is not told, and it is independent of leases: an operator
can drive one robot and watch another, and any number of operators can watch the same one.

When the value changed, the server sends `operator.watching` to the fleet's `presence`
subscribers, carrying the operator's entry like the other operator events:

```json
{ "event": "operator.watching", "operator_id": "o_9c8d7e6f",
  "data": { "operator_id": "o_9c8d7e6f", "name": "ada", "online": true, "watching": "r_1a2b3c4d" } }
```

After `watch {robot_id: null}` the same event arrives with no `watching` in `data`. A late
joiner needs no event: `snapshot.operators` carries `watching` on each entry.

- **No reply.** A `watch` that is accepted is not answered. The sender, if it subscribed
  to `presence`, receives its own `operator.watching` like everyone else.
- **A redundant watch is silent.** Naming the robot the operator already watches, or
  `null` when it watches none, changes nothing and emits nothing.
- **Operators only.** From a robot or a service it is `error{code: not_authorized}`.
- **The robot must be one the sender's snapshot lists**: a robot of its own fleet that is
  not revoked, online or offline. Anything else is `error{code: not_found}` with the same
  message in every case: an id that does not exist, a robot of another fleet, a client
  that is not a robot. So ids never leak across fleets. A missing, empty or non-string
  `robot_id` is `invalid_message`. A refused `watch` leaves the previous value in place
  and tells nobody else.
- **It lasts as long as the connection that sent it.** The server forgets it when that
  connection ends, so a client MUST send `watch` again after every connect, including
  the SDKs' automatic reconnects, if it is still showing a robot.
  - Disconnect, heartbeat lapse and token revocation end in `operator.offline`, whose
    entry has no `watching`. That one event clears it; no `operator.watching` is sent.
  - A second connection with the same token replaces the first without the operator
    going offline (see below). The new connection starts out watching nothing, so if the
    old one was watching a robot the fleet gets `operator.watching` with no `watching`.
    If it was not, the replacement is silent as before.
- **The robot going offline changes nothing.** If the robot is revoked, every operator
  watching it gets an `operator.watching` with no `watching`.
- **Rate limited.** `watch` is metered per connection with its own bucket, like
  `telemetry`. A `watch` over the limit is dropped and answered with
  `error{code: rate_limited, ref}` (at most one notice per second), and the server keeps
  the last value that got through. A console SHOULD send only the selection it has
  settled on (debounce rapid changes), give each `watch` an envelope `id`, and on a
  `rate_limited` error send its current selection once more after a pause.

Golden examples: `fixtures/valid/watch.json`, `fixtures/valid/watch-clear.json`,
`fixtures/valid/event-operator-watching.json`,
`fixtures/valid/event-operator-watching-cleared.json`.

## Acked send (convention on channel data)

`channel.publish` is at-most-once and a successful publish gets no reply. **Acked send**
is how one client learns that another client received a directed message. It is a
convention between the two clients, carried entirely inside the opaque `data`; the server
does not know about it and no schema or envelope type changes. This section is the single
reference: an SDK that offers acked sends implements exactly this, so that a sender and a
receiver written in different languages interoperate.

MUST / SHOULD / MAY are used in the RFC 2119 sense.

### Shapes

Two shapes of `data` are reserved, on every channel:

| name | `data` | sent by |
|---|---|---|
| acked message | `{ "seq": <seq>, "data": <any JSON value> }` | sender, directed (`to`) |
| ack | `{ "ack": <seq> }` | receiver, directed back to the sender |

- A value is an **acked message** only if it is a JSON object with exactly the members
  `seq` and `data`, and `seq` is a valid seq. It is an **ack** only if it is a JSON object
  with exactly the member `ack`, and `ack` is a valid seq. Anything else (extra members,
  a string or fractional or out-of-range seq) is ordinary channel data and is handed to
  the application untouched. `null` is a legal inner `data`; a missing `data` is not.
- A **valid seq** is a JSON integer in `1 … 9007199254740991` (2^53 − 1). Hold it in a
  64-bit integer; real values exceed 32 bits (see allocation below).
- Applications MUST NOT publish ordinary data of exactly these two shapes.
- The inner `data` is what the receiving application sees. `seq` is not shown to it.

Golden examples: `fixtures/valid/channel-publish-acked.json` (what the sender puts on the
wire) and `fixtures/valid/channel-message-ack.json` (what the sender gets back).

### Sender

- **Directed only.** An acked message is published with `to`, never `broadcast`.
- **The sender allocates `seq`.** It MUST be unique per (sender client id, channel) and
  MUST NOT be reused, including across a restart of the sender, for at least 10 minutes
  after its last transmission. One counter for the whole client, shared by every channel
  and target, satisfies this. A sender that does not persist its counter MUST seed it
  with the Unix time in milliseconds when it starts and add one per send. Seqs carry no
  ordering meaning and need not be contiguous as seen by any one receiver.
- **Envelope `id`.** Each transmission MUST carry an envelope `id` (≤ 64 chars) that the
  sender can map back to the pending send; that is the only way to match the server's
  `error.ref`. Re-sends MAY reuse the id. The receiver never sees it.
- **Re-send.** Until the send is resolved, the sender re-publishes the same
  `data` (same `seq`, same inner `data`) on an interval. Default interval 1 s; it MUST NOT be shorter than
  250 ms and MAY grow (back off) up to 5 s. Re-sends count against the sender's
  `channel.publish` rate limit.
- **Deadline.** Every acked send has a deadline measured from the first transmission,
  chosen by the caller. Default 10 s; it MUST NOT exceed 5 minutes. The deadline keeps
  running while the sender itself is disconnected; re-sending pauses and resumes with
  the same `seq` once it is reconnected.
- **Resolution.** A pending send ends in exactly one of these ways, after which the
  sender stops re-sending and never reuses the `seq`:

  | outcome | trigger | what the sender may conclude |
  |---|---|---|
  | acked | a `channel.message` on the same channel whose `from` equals the send's `to` and whose `data` is `{ "ack": seq }` | the receiving application accepted the data |
  | not found | `error{code: not_found}` whose `ref` is a transmission of this send | the target is not connected. Fail immediately; do not wait out the deadline. If this was the first transmission the data was not delivered. If it was a re-send, an earlier copy may have been delivered and its ack lost: unknown |
  | rejected | `error` with code `invalid_message` or `not_authorized` and a matching `ref` | permanent; fail immediately |
  | timed out | the deadline passes | unknown: the data may or may not have been delivered |

- `error{code: rate_limited}` does not resolve a send. That transmission was dropped;
  the next interval re-sends it.
- An ack that matches no pending send (late, duplicate, from a different client than the
  target, unknown seq) is ignored silently.
- A caller that tries again after "not found" or "timed out" is making a new send with a
  new `seq`. Because the earlier outcome may be unknown, data whose effect must not
  happen twice needs its own identity inside the inner `data`.

### Receiver

The receiver keeps a **seen set** keyed by `(channel, from, seq)`. `from` is the client id
stamped by the server, so the set is unaffected by either side reconnecting.

On a `channel.message` whose `data` is an acked message:

1. **New key.** Record it as in progress, then hand the inner `data` and `from` to the
   application.
   - When the application accepts it (the handler returns without error), mark the key
     done and publish the ack.
   - If the application fails it, remove the key and send nothing. The sender's next
     re-send is then treated as new.
2. **Key in progress.** Drop the repeat. Do not deliver it again and do not ack yet.
3. **Key done.** Do not deliver it again. Publish the ack again, every time.

- **The ack** is `channel.publish {channel: <same channel>, to: <from>, data: {"ack": seq}}`.
  It is sent once per received copy, never re-sent on a timer, and never itself acked.
  If publishing it fails (the sender has gone offline: `not_found`), drop it; the
  sender's next re-send will be answered.
- **An ack means accepted, not finished.** Handlers SHOULD return well inside the
  sender's re-send interval and do long work afterwards, reporting its result as
  ordinary channel data of the application's own design.
- **Memory.** A done key MUST be remembered for at least 10 minutes after it was first
  received. That is twice the longest sender deadline, so no legal re-send can arrive
  after its key was forgotten. Entries are one integer each; the publish rate limit
  bounds how many a single sender can create.
- The seen set MAY live in memory only. A receiver that restarts forgets it, and a
  re-send that arrives afterwards is delivered a second time. So the guarantee is:
  **at-least-once while the sender keeps trying, and exactly-once to the application
  within one receiver process lifetime.**
- Receivers MUST NOT infer loss or order from gaps in `seq`. Different seqs are
  delivered in arrival order.
- Ordinary data and acked messages may share a channel. A receiver that does not
  implement this convention sees the reserved shapes as plain data, so both ends of a
  channel have to agree to use it.

## Teleop data plane (twist over WebRTC)

While an operator holds a robot's lease, twist can ride a direct WebRTC data channel
between the two instead of the bus. This section is the single reference for that path:
the console, the sim robot, the Python SDK and the ROS 2 agent implement exactly this.
MUST / SHOULD / MAY are used in the RFC 2119 sense.

Nothing about authority changes (DESIGN.md D2, D3). The channel carries data only.
Claim, steal and handback are still `lease.*` messages to the server, the robot still
obeys twist only when it bears the current `lease_id`, and its deadman still fires
300 ms after the last twist it obeyed, whichever transport that twist came on. Opening
or closing a peer connection never grants, extends or ends a lease.

### Roles

- The **operator offers**, the **robot answers**. The operator creates the peer
  connection and the data channel after it receives `lease.granted` for a new lease.
  A robot never offers.
- There is at most one peer connection per lease. Each attempt is a **session** with an
  id chosen by the operator: an opaque string, unique per offer. A later offer under
  the same lease replaces the earlier session.
- The direct path is optional. A robot that does not implement it ignores the offer and
  is driven over the bus; an operator MUST keep working when no answer comes.

### Signaling

Offer, answer and ICE candidates travel as `signal` envelopes. The sender sets `to`;
the server delivers the message to that client with `from` set to the sender's client
id and `to` removed, and relays `data` untouched. It relays only inside one fleet, and
replies `error{code: not_found}` to the sender when the target is not connected.

| `kind` | sent by | `data` |
|---|---|---|
| `offer` | operator | `{ "session": "<id>", "lease_id": "<lease>", "type": "offer", "sdp": "<SDP>" }` |
| `answer` | robot | `{ "session": "<id>", "type": "answer", "sdp": "<SDP>" }` |
| `ice` | either | `{ "session": "<id>", "candidate": { "candidate": "candidate:...", "sdpMid": "0", "sdpMLineIndex": 0 } }` |

- `candidate` is the WebRTC `RTCIceCandidateInit` shape. `sdpMid` and `sdpMLineIndex`
  may be `null`. Candidates are trickled: the SDP may also contain some. The end of
  candidates is not signaled; a receiver MUST ignore an `ice` whose inner `candidate`
  string is empty or missing.
- A side sends `ice` for a session only after it has sent that session's offer
  (operator) or answer (robot), and holds back candidates gathered earlier until then.
  A receiver MUST hold candidates that arrive before it has applied the remote
  description, and add them afterwards.
- Trickling is optional for the sender. A side MAY finish gathering first, put every
  candidate in its SDP and send no `ice` at all (the Python SDK's robot does, because
  aiortc gathers that way). So a receiver MUST NOT wait for an `ice` before it starts
  connecting, and MUST work with an SDP that carries no candidates when they follow as
  `ice`.
- **The robot answers an offer only if** it currently holds a lease, the signal's
  `from` equals that lease's `operator_id` (from `lease.granted`), and `data.lease_id`
  equals that lease's id. Any other offer is dropped without a reply. `ice` is accepted
  only from that same operator and only for the current session id.
- **The operator accepts** `answer` and `ice` only when `from` is the robot it leased
  and `session` is its current session.
- ICE servers (STUN/TURN) are configuration of the installation, and each peer asks
  the server for them with `ice.request` before it creates its peer connection (see
  [ICE servers](#ice-servers)). None are needed on loopback or a single LAN, and an
  installation may have none. Neither side may assume a default public STUN server.

### ICE servers

Two peers on different networks need a STUN server to find a direct path and a TURN
server to relay when there is none. Which servers those are is bootstrap configuration
of the fleet-server (`stun_urls`, `turn_urls`, `turn_secret`; DESIGN.md D1, D11). The
server hands the addresses out and signs a credential. It never relays media and never
talks to the TURN server (D2).

```json
{ "v": 0, "type": "ice.request", "id": "ice-1", "payload": {} }
```

```json
{ "v": 0, "type": "ice.config", "payload": {
    "ice_servers": [
      { "urls": ["stun:turn.example.org:3478"] },
      { "urls": ["turn:turn.example.org:3478?transport=udp", "turns:turn.example.org:5349?transport=tcp"],
        "username": "1755103600:o_9c8d7e6f", "credential": "H5hhCnIJ5Q0i88m4zVts9A7C8Sc=" }
    ],
    "expires_at_ms": 1755103600000,
    "ref": "ice-1" } }
```

- **`ice_servers` is WebRTC's `RTCIceServer` list**, to be passed unchanged as
  `RTCConfiguration.iceServers` (`urls`, `username`, `credential` are WebRTC's names,
  which is why they are not this protocol's usual style). `urls` is always an array.
  A STUN entry has no `username` or `credential`; a TURN entry has both.
- **Nothing configured is an empty list**: `{ "ice_servers": [] }`. That is a normal
  answer, not an error. The peer connection is then created with no ICE servers and
  finds host candidates only, which is all loopback or one LAN needs.
- **Robots and operators only.** From a service it is `error{code: not_authorized}`.
  The request has no fields and names no id; the answer depends only on who asks.
- **`ref`** is the request's envelope `id`, absent when it had none. Give each request
  an `id`: a refusal also names it (`error.ref`).
- **The TURN credential is short-lived and the asker's own.** `username` is
  `<expiry>:<client id>`, the expiry in Unix seconds; `credential` is
  `base64(HMAC-SHA1(turn_secret, username))`. This is the time-limited shared-secret
  scheme of TURN servers (coturn: `use-auth-secret`), so the TURN server verifies it
  with the same secret and nothing is stored. The secret itself is never sent.
  `expires_at_ms` is that expiry in epoch milliseconds on the server's clock; it is
  absent when no entry carries a credential. The lifetime is `turn_credential_ttl_s`
  (default one hour).
- **Ask for each session; do not keep the answer around.** Credentials expire, and a
  client can sit connected for days before it needs a peer connection, which is why
  this is a request and not part of `welcome`.
  - The operator sends `ice.request` before each offer: every session, including a
    retry under the same lease.
  - The robot sends it when it receives `lease.granted` with a new lease id, so the
    answer is there by the time the offer comes. It MAY use that answer for every
    session under that lease until `expires_at_ms`; without one, or past that time, it
    asks when the offer arrives.
  - Neither creates a peer connection with a credential past `expires_at_ms`. A
    session that is already running when its credential expires is left alone: there
    is no way to hand a live peer connection a new one. Whether its relay survives is
    up to the TURN server (coturn checks the expiry when the allocation is made; some
    servers check it again on every refresh), so the lifetime should be longer than a
    teleop session, and a relayed session that does drop falls back to the bus like
    any other.
- **Never wait on it to drive.** Twist starts on the bus when the lease is granted
  (see [One transport at a time](#one-transport-at-a-time)), so asking costs the driver
  nothing. If no `ice.config` arrives within 2 s, or the request is refused, the client
  SHOULD go ahead with an empty list rather than give up on the direct path. An older
  server answers `error{code: invalid_message}` (unknown message type) with the
  request's `ref`: treat that as an empty list too.
- The two peers are told the same servers but hold different credentials. Neither
  sends its `ice.config` to the other.

Golden examples: `fixtures/valid/ice-request.json`, `fixtures/valid/ice-config.json`
(its credential is the real HMAC of its username under the secret
`north-star-turn-secret`, usable as a test vector), `fixtures/valid/ice-config-empty.json`.

### The data channel

The operator creates one channel before making the offer, in-band (not `negotiated`):

| option | value |
|---|---|
| label | `twist` |
| `ordered` | `false` |
| `maxRetransmits` | `0` |

So a twist is delivered at most once, possibly out of order, and never late because of
an earlier loss. The robot closes any channel with a different label. Every message is
one text frame holding one UTF-8 JSON object, at most 1024 bytes. A receiver MUST
ignore, without closing the channel, anything that is not valid JSON, not an object,
or not one of the three shapes below.

**Twist** (operator → robot) is the bus `twist` payload, not the envelope, plus `seq`:

```json
{ "lease_id": "ls_7f8e9d0c", "seq": 42, "linear": { "x_mps": 0.5 }, "angular": { "z_radps": -0.3 } }
```

- `lease_id`, `linear` (`x_mps`, optional `y_mps`) and `angular` (`z_radps`) mean
  exactly what they mean in the `twist` schema, and numbers must be finite.
- `seq` is a positive integer, at most 2^53 - 1, that the operator increases with every
  twist it sends on the channel. It exists because the channel is unordered.
- The robot processes a twist in this order: (1) drop it if `seq` is not greater than
  the highest `seq` it has obeyed on this channel; (2) drop it unless `lease_id` is the
  lease it currently holds; (3) obey it, restart the deadman, and remember `seq`. A
  twist dropped at step 2 MUST NOT move the remembered `seq`. The mark starts at 0 for
  each new channel.
- A robot whose control connection to the server is not open MUST NOT obey twist from
  the channel: it could not hear a revocation. It resumes when the connection is back
  and its `welcome` states the same lease (see
  [A robot's lease at connect](#a-robots-lease-at-connect)); if the welcome states
  another lease or none, the channel is closed with the lease it belonged to.
- Only an obeyed twist moves the remembered `seq`. A twist the robot does not obey for
  any other reason (its control connection is down, its manifest declares no drive) is
  dropped like one refused at step 2 and leaves the mark where it was.

**Ping** (operator → robot) and **pong** (robot → operator) are the operator's liveness
check, because a browser can take many seconds to report a dead peer connection:

```json
{ "ping": 1755100003000 }
{ "pong": 1755100003000 }
```

The robot answers every `ping` at once with a `pong` carrying the same number. The
number is opaque to the robot (the console sends its clock in milliseconds and reads
the round trip from the echo). A ping is not a twist: it MUST NOT restart the deadman.

### One transport at a time

The operator sends each twist on exactly one transport.

- It starts on the bus when the lease is granted, so driving never waits for WebRTC.
- It switches to the channel when the channel is open **and** the first `pong` has
  arrived. From then on it sends nothing on the bus.
- It switches back to the bus (the fallback rule) the moment any of these happens: the
  channel closes, the peer connection reports `failed` or `closed`, sending throws, or
  a ping has gone unanswered for 1000 ms (pings every 250 ms). It then closes that
  session, and MAY offer a new one after a pause (the console waits 3 s, doubling per
  consecutive failure up to 30 s). An offer that has not produced a first `pong` within
  5 s counts as a failure.
- Because the channel can lose a message, a zero twist (the stop on releasing the last
  key) SHOULD be repeated on it: the console sends it three times, 100 ms apart, unless
  a newer twist is sent first.

The robot accepts twist from both transports at any time under the same lease, with one
rule for the moment of switching: **a bus twist is ignored if the robot obeyed a
data-channel twist within the last 300 ms** (the deadman window). That covers both
directions:

- *Bus to channel.* A bus twist sent just before the switch can arrive after the first
  channel twists, because the bus is the slower path. It is ignored, so it cannot
  replace a newer setpoint.
- *Channel to bus.* The first bus twists after a fallback may be ignored for up to
  300 ms after the last channel twist the robot obeyed. During that time the robot
  holds that last setpoint, and if nothing else is obeyed its deadman stops it at the
  300 ms mark exactly as it would have. After that the bus twists drive it.

A robot never applies one setpoint twice, because no setpoint is ever sent twice on two
transports. The one ordering the rule cannot fix is a channel twist delayed in the
network until after the operator has fallen back; it is obeyed if its `seq` is new, and
its effect ends within 300 ms by the deadman.

### Teardown

Teardown is cleanup that follows a lease decision. It is never how a lease ends.

| event | robot | operator |
|---|---|---|
| `lease.revoked` for the held lease, any reason (`released`, `stolen`, `expired`, `operator_lost`) | closes the peer connection and forgets the lease | closes the peer connection and stops sending |
| the operator releases (`lease.release`) or leaves the robot's page | closes on the `lease.revoked` that follows | sends one zero twist, then closes |
| `lease.granted` with a different lease id (a steal) | closes the old peer; only the new operator's offer is answered | (the old operator gets `lease.revoked`, above) |
| a new offer under the same lease | closes the old session, answers the new one | |
| peer connection `failed` / `closed`, channel closed | closes the session; keeps the lease and waits for a new offer | falls back to the bus; may offer again |
| control connection to the server lost | keeps the peer but obeys no channel twist until it is back | keeps driving on the channel if it is live; the server revokes the lease if the operator stays gone |
| `welcome` after a reconnect | same lease: resumes on the peer it kept. Another lease or none: closes the peer and forgets the old lease | |

A twist that arrives on a channel after the lease is gone bears a lease the robot no
longer holds and is dropped like any other.

### Fixtures

`fixtures/datachannel/` holds the golden data-channel messages: `twist.json`,
`ping.json`, `pong.json`. They are not envelopes, so they live outside
`fixtures/valid/` and the envelope contract tests do not read them. Each implementation
of this section loads them in its own tests, as the acked-send fixtures are used.

## Fixtures

`fixtures/valid/` must all validate (envelope + payload per catalog); `fixtures/invalid/`
must each fail. Every implementation runs both sets — they are the cross-language
contract test. Add a fixture with every schema change.

Fixtures are discovered by directory listing, so a new file needs no registration; its
`type` must be in `catalog.json`. The schemas leave channel `data` opaque, so the
contract tests check only the envelope and payload of the two acked-send fixtures. The
shapes inside `data` are checked by each SDK's own acked-send tests, which load those
same two files as their golden wire examples.
