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
  AUTONOMOUS, a plain claim on a held lease is refused (and of racing plain claims
  exactly one is granted), steal = revoke + reissue, renew extends, expiry returns robot to
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

One Go test crosses languages: `TestSendAckedToThePythonRobot` runs
`sdk/python/examples/fake_robot.py` and sends it an acked message from the Go client, so
the Go sender and the Python receiver of the acked-send convention are checked against
each other. It skips unless a Python that has the Python SDK's dependencies is available:
`FLEET_TEST_PYTHON=/path/to/venv/bin/python go test ./fleet`.

Robot-specific timing tests live with `sdk/ros2` later: deadman fires at ~300ms without
valid twist (fake clock), twist rejected without current lease id.

## 5. E2E / demo verification

- The two-operator demo below: the 10-minute-stranger test, run by hand before every
  milestone (later: Playwright driving the console for claim → WASD → handback).
- Teleop latency budget (<200ms glass-to-glass) is measured, not unit-tested: timestamp
  overlay in the video + datachannel echo timing, once real WebRTC media exists.

### The two-operator demo

Two people share one help queue for a fleet of 20 simulated robots. It needs no hardware
and no accounts. From a fresh clone to both people driving takes a few minutes; the build
itself took 21 seconds on a laptop with nothing cached.

You need Go 1.26 or newer, Node 20 or newer (with npm), and curl. The script checks for
them and says which one is missing.

```bash
git clone <this repo> fleet-platform && cd fleet-platform
./scripts/demo.sh
```

The script builds the console into `fleet-server`, starts the server on a throwaway
database, creates a fleet and two operator invites, and starts the sim robots. It then
prints the console URL and the two invite keys, and keeps running:

```
Ready in 21s: 20 sim robots are online and will start asking for help.

  Console:  http://localhost:8090/

  Operator invites (each works once; paste one on the sign-in screen):
    operator 1:  fp-oi-...
    operator 2:  fp-oi-...
```

Ctrl-C stops the server and the sim and deletes the database. The port is the first free
one from 8090, so a `fleet-server` you already run on 8080 is not touched; use the URL the
script prints. `./scripts/demo.sh --help` lists the options (`--port`, `--count`,
`--help-rate`, `--operators`, `--lan`, `--keep`).

**Two operators need two sign-ins.** The console keeps its operator token in the browser,
per address, so two tabs of one browser profile are the same operator. Pick one:

- Two people on one network: start with `./scripts/demo.sh --lan`. The second person opens
  the second URL the script prints (this machine's network address).
- One person, two windows: use two browser profiles, or a normal window and a private one.
- One person, one profile: open `http://localhost:8090/` in one tab and
  `http://127.0.0.1:8090/` in another. The browser treats them as different sites, so each
  signs in separately.

Then follow the steps. The names `alice` and `bob` are whatever each person types at
sign-in.

1. **Sign in.** Each person opens the console URL, pastes one invite key, types a name,
   and presses **Redeem invite**. Each sees the map with 20 robots, the **Robots** list
   (`20 online / 20`), and both names in the bar at the top, their own marked `(you)`.
2. **Watch the queue fill.** Within a minute or so a robot asks for help. Both people see
   the same entry appear under **Help queue**: the robot's name, why it asked
   (`low_confidence`, `path_blocked` or `localization_degraded`), and how long it has
   waited. The longest wait is at the top. The robot turns amber on the map.
3. **Alice claims and drives.** Alice presses **Claim** on the first entry. Her right-hand
   pane shows `You have control. Hold W A S D to drive.` and the entry leaves the queue on
   both screens. Bob's top bar shows `alice driving sim-NN`. Alice holds **W**: `Command`
   shows a speed and the robot moves on both maps.
4. **Check the link.** In Alice's pane, `Link` reads `Direct (WebRTC)` with a round-trip
   time in milliseconds. Her drive commands go straight to the robot, not through the
   server. If it reads `Server relay`, driving still works; the commands take the slower
   path through the server.
5. **Bob spectates.** Bob clicks `driving sim-NN` next to Alice's name. His pane shows
   `alice is driving. You are watching read-only.` with the robot's live speed, and no
   drive keys. Alice's pane lists Bob under `Watching`.
6. **Bob steals.** Bob presses **Take control from alice**. He now has the drive keys.
   Alice's pane changes to `bob is driving. You are watching read-only.` with the note
   `bob took control from you.`
7. **Hand back.** Bob presses **Hand back**. The robot's state returns to `AUTONOMOUS`
   and it drives itself again. It may ask for help again later.
8. **Race for one robot.** Both people press **Claim** on the same queue entry at the same
   moment. One gets control. The other sees `alice claimed it first.` (with the winner's
   name) and is left watching read-only, still connected. Exactly one person drives.

What to do if a step does not match:

| You see | Cause |
|---|---|
| `port N is already in use` | you passed `--port` for a port that is taken; leave it off |
| The second window is already signed in as the first person | both windows share one browser profile and one address; see the three options above |
| `Help queue` stays empty for minutes | chance; raise the rate with `--help-rate 0.5` |
| An invite is refused | each invite works once; restart the script, or pass `--operators 4` for spares |

## CI shape (when wired)

```
lint → contract tests (all langs) → unit → layer-3 integration → build binary
     → layer-4 SDK suites against the binary
```
