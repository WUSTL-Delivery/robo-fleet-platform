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
| `hello` / `welcome` | client → server / reply | authenticate; identity is **derived from the token, never claimed** |
| `heartbeat` | client → server | presence; silence ⇒ offline (welcome carries the interval) |
| `manifest` | robot → server | capability declaration; console renders only what's declared |
| `telemetry` | robot → server | frame-relative pose, velocity, battery, health |
| `help.request` | robot → server | AUTONOMOUS → HELP_REQUESTED; enters intervention queue |
| `subscribe` / `snapshot` | client → server / reply | **snapshot-then-stream**: current fleet state, then live events |
| `event` | server → subscribers | typed lifecycle events (`robot.online`, `robot.help_requested`, …) |
| `lease.claim` / `lease.renew` / `lease.release` | operator → server | authority requests; every transition is server-side |
| `lease.granted` / `lease.revoked` | server → operator+robot | the lease itself; steal/expiry/operator-loss arrive as `revoked` |
| `twist` | operator → robot | body-frame setpoint **carrying lease_id**; rides the WebRTC datachannel (bus fallback in sim), robot rejects without current lease |
| `signal` | relayed via server | WebRTC offer/answer/ICE; sender sets `to`, server stamps `from` |
| `channel.publish` / `channel.message` | service ↔ server ↔ client | opaque domain channels (assignments, edge reports); at-most-once (see [Acked send](#acked-send-convention-on-channel-data)) |
| `layer.declare` / `layer.update` | service → server | map layers (GeoJSON first); console renders generically |
| `error` | server → client | `auth_failed`, `invalid_message`, `not_found`, `not_authorized`, `conflict`, `rate_limited` |

## Conventions

- **Pose is frame-relative, never assumed lat/lon** (D7): `{"frame": "geographic", lat,
  lon, alt_m?, yaw_rad?}` or `{"frame": "local", frame_id?, x_m, y_m, z_m?, yaw_rad?}`.
  Yaw is radians, CCW; 0 = +x for local frames, 0 = East (ENU) for geographic.
- Units in field names: `_m`, `_mps`, `_radps`, `_ms` (epoch millis / durations).
- Robot FSM: `AUTONOMOUS | HELP_REQUESTED | TELEOP` — platform vocabulary only.
- Versioning: `v` bumps only on incompatible envelope change; message-level evolution is
  additive (new optional fields) until a type is redesigned under a new name.

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

## Fixtures

`fixtures/valid/` must all validate (envelope + payload per catalog); `fixtures/invalid/`
must each fail. Every implementation runs both sets — they are the cross-language
contract test. Add a fixture with every schema change.

Fixtures are discovered by directory listing, so a new file needs no registration; its
`type` must be in `catalog.json`. The schemas leave channel `data` opaque, so the
contract tests check only the envelope and payload of the two acked-send fixtures. The
shapes inside `data` are checked by each SDK's own acked-send tests, which load those
same two files as their golden wire examples.
