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
| `channel.publish` / `channel.message` | service ↔ server ↔ client | opaque domain channels (assignments, edge reports); at-most-once |
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

## Fixtures

`fixtures/valid/` must all validate (envelope + payload per catalog); `fixtures/invalid/`
must each fail. Every implementation runs both sets — they are the cross-language
contract test. Add a fixture with every schema change.
