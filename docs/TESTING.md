# Verification strategy

How we know the platform works, from schema to demo. Five layers; each layer catches what
the one below can't. Sim-first discipline applies: **nothing lands without a test at layer
1–3, and no feature is "done" until it works at layer 4 against the sim.**

## 1. Contract tests (protocol ↔ every implementation)

`protocol/` is the source of truth: JSON Schemas + a machine-readable `catalog.json`
(message type → schema) + golden fixtures in `protocol/fixtures/{valid,invalid}/`.

Every language that speaks the protocol validates **the same fixtures**:

- Go (server and Go SDK share these types): `cd sdk/go && go test ./protocol/` — validates all fixtures against
  the schemas, and round-trips them through the Go structs (unmarshal → marshal → still
  schema-valid). This is what stops the Go types drifting from the schemas.
- TS SDK: `cd sdk/typescript && npm test` — `test/contract.test.ts` validates the same
  fixtures via ajv (draft 2020-12) against the same schemas and `catalog.json`, and checks
  the generated `MESSAGE_TYPES` match the catalog.
- Python SDK: `pip install -e 'sdk/python[dev]' && cd sdk/python && pytest` —
  `tests/test_contract.py` validates the same fixtures via jsonschema (draft 2020-12), and
  the rest of the suite runs against the built `fleet-server` binary on an ephemeral port
  (Go must be installed; the fixture builds it once per session).

A protocol change is: edit schema → add/adjust fixtures → watch every language's contract
test break → fix each. The fixtures are the cross-language handshake.

## 2. Unit tests (pure logic, no I/O)

The atomic core gets the densest coverage:

- `server/internal/ops` — intervention FSM + leases with an **injected fake clock**:
  legal/illegal transitions, claim from HELP_REQUESTED and proactive claim from
  AUTONOMOUS, steal = revoke + reissue, renew extends, expiry returns robot to
  HELP_REQUESTED, exactly-one-driver invariants.
- `server/internal/registry` — presence expiry (heartbeat lapses → offline event).
- `server/internal/store` — token/enroll-key hashing, revocation, against a temp sqlite.

Run: `cd server && go test ./...`

## 3. In-process integration tests (real server, fake wire clients)

`server/integration_test.go` boots the **real server** (real gateway, real sqlite in a
temp dir, ephemeral port) and connects **fake clients over real WebSockets** — thin test
helpers speaking raw protocol JSON, the proto-SDKs. The canonical storyline, asserted
end to end:

1. Seed a fleet + enrollment key; fake **robot** enrolls → gets per-robot token →
   reconnects → hello/auth → manifest → presence online.
2. Fake **operator** hellos with its token, subscribes → receives a **snapshot** showing
   the robot online with its manifest (snapshot-then-stream).
3. Robot sends `help.request` → operator receives `robot.help_requested` event.
4. Operator claims → both sides receive the lease; robot FSM is TELEOP.
5. WebRTC **signaling relay**: operator `signal(offer)` → robot receives it stamped with
   `from`; robot answers → operator receives.
6. Operator releases → robot back to AUTONOMOUS, events fan out.

Run: `cd server && go test -run TestIntegration ./...`. Fast (<2s), no docker, runs in CI
on every push. New server features extend this storyline or add sibling storylines
(steal, operator-loss revocation, channel routing).

## 4. SDK integration tests — "two fake clients, real SDKs, real config"

The layer-3 harness graduated: once `sdk/typescript` exists, a vitest suite in
`sdk/typescript` **launches the built `fleet-server` binary** with a generated config file
(temp dir, temp sqlite, seeded enrollment key printed by `--bootstrap`), then runs the
same storyline using the public SDK surface instead of raw JSON:

```ts
const robot = new FleetRobot({ url, token: robotToken });     // fake robot via SDK
const svc   = new FleetClient({ url, token: serviceToken });  // fake brain via SDK
await robot.connect({ manifest });
const snap = await svc.subscribe(["presence", "events"]);     // snapshot-then-stream
await robot.requestHelp("stuck");
await expectEvent(svc, "robot.help_requested");
```

Same suite shape repeats in `sdk/python` (pytest) against the same binary — one server,
every SDK, identical semantics. `sim/` is this fixture productized: the sim fleet is
"N fake robots via the TS SDK," so the demo and the test share code.

`sdk/go` does the same in Go: `cd sdk/go && go test ./...`. The shared helper
`sdk/go/internal/fleettest` builds `fleet-server` once per test package (`fleettest.Main`
from `TestMain`), starts a fresh one per test (`fleettest.Start`), and offers a TCP proxy
(`Server.Proxy`) to drop, stall, or black out the connection. The SDK never imports server
packages; it builds the binary. `GOTOOLCHAIN=go1.24.0 go test ./...` holds the SDK's Go
floor: the helper still builds the server with a newer `go` from `PATH`.

Robot-specific timing tests live with `sdk/ros2` later: deadman fires at ~300ms without
valid twist (fake clock), twist rejected without current lease id.

## 5. E2E / demo verification

- `docker compose up` + sim fleet + console: the 10-minute-stranger test, run by hand
  before every milestone (later: Playwright driving the console for claim → WASD →
  handback).
- Teleop latency budget (<200ms glass-to-glass) is measured, not unit-tested: timestamp
  overlay in the video + datachannel echo timing, once real WebRTC media exists.

## CI shape (when wired)

```
lint → contract tests (all langs) → unit → layer-3 integration → build binary
     → layer-4 SDK suites against the binary
```
