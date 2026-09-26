# fleet-platform — Architecture & Design Decisions

> Handoff doc for future Claude sessions. Everything here was settled in design discussions
> (2026-08-07 → 2026-08-11) with Jaxon. Read CLAUDE.md first for product framing; this doc
> records the *decisions and their rationale* so they don't get re-litigated.
>
> External records: Tandem canvas `C9DBDB5L` (full discussion notes, https://tandemcanvas.com/c/C9DBDB5L) ·
> Artifact "Command Brain's Promotion" (shareable one-pager: diagram, pseudocode, why-not-Kafka,
> server feature list) at https://claude.ai/code/artifact/2357cbba-3b80-46fc-843e-0d5c023e48b7 ·
> older full boundary-map artifact at https://claude.ai/code/artifact/3fc7ff0b-3cf3-4ec3-9290-3010aebc2214

## One-breath pitch

**We're building the control server everyone connects to — plus the ops UI to see it and
drive it.** Everything domain-shaped stays in the services that connect to it. Category:
open-source **remote operations platform** for robot fleets ("Tailscale + Slack + a 911
dispatch console for robots"). The pitch is **multiplayer (N robots × M humans), not massive
scale**.

## D1. Consumption model: application + SDKs, never clone-and-edit

- Server + console = a **deployable app** (Grafana model): `docker compose up`, config file, done.
- SDKs = the imported libraries (ROS-like in spirit; the extension boundary is the **network
  protocol**, not linking).
- Clone-and-edit configuration is rejected — it's a fork and kills upgrades.
- Config splits three ways: **bootstrap config** (~10-key yml: url, secrets, storage, TURN) /
  **runtime state** (DB, managed via console UI: fleets, robots, tokens, operators) /
  **domain config** (doesn't exist server-side — arrives over the wire as manifests + layer
  declarations). Mantra: *config file describes the installation; database describes the
  world; the wire describes the robots.*
- No data sources: the platform never pulls. **Everything dials in as a client** with a token.
- Deliberately excluded forever: server/console plugin systems, server-side scripting
  (the back doors through which domain code sneaks in).

## D2. Two planes

- **Control plane** (WSS → server): presence, queue, leases, telemetry, layers, bus,
  signaling. Latency-tolerant. Per-robot steady load ≈ one chat user (~1 KB/s).
- **Data plane** (WebRTC, P2P): teleop video + twist, operator-browser ↔ robot directly.
  Server does signaling only. ICE races LAN / hole-punched / TURN paths concurrently;
  failure of direct paths looks like "stayed on relay," never a stall.
- Robots are **never servers**: both WebRTC peers dial out (hole punching). No port
  forwarding anywhere in the system, ever.
- The control plane stays light **by law**: bus enforces per-client size/rate limits. Heavy
  data (video, point clouds) goes to the data plane (declared stream types, for operator
  eyes) or rides the robot's own separate pipeline beside the platform (ML ingest, rosbag
  upload — fine and normal). "One connection" means *ops needs exactly one*, not exclusivity.

## D3. Trust & authority model

- **P2P channel carries only data (video, twist). Every state transition is a server-side
  action** (claim/steal/handback/resolve go through the server; P2P teardown is cleanup,
  never source of truth).
- Authority = server-issued **lease**: exclusive, expiring, renewable, revocable. Steal =
  revoke + reissue, never share. Robot accepts twist only bearing the current lease.
- Silence = death: heartbeats server-side; lease expiry; robot-side **deadman** (~300ms
  without valid twist during TELEOP → zero velocity). Robot fails closed independently of
  the server — *physics never waits on the network stack's opinion*.
- Intervention state machine per robot: `AUTONOMOUS → HELP_REQUESTED → (claim) → TELEOP →
  (handback/resolve) → AUTONOMOUS`. Escalation *policy* (when to ask) is domain, on the
  robot's club node; escalation *mechanics* (queue/lease/teleop/replay) are platform.
- Day-one metric: **HELP_REQUESTED → first operator eyeball** latency.

## D4. Scope boundary & the litmus test

Platform vocabulary (all of it): connection, manifest, twist, pose, stream, layer, event,
presence, claim, lease, queue, replay. **Zero domain words.**

**Litmus test for every feature:** would a drone fleet or warehouse fleet need it
*unchanged*? → platform. Would only a campus delivery fleet recognize it? → club repo.

Traffic is ~80% **typed** (platform understands and acts: manifest, twist, pose, streams,
layers, presence, help) and ~20% **opaque** (domain channels, post-office routed). The bus
is ~2% of the engineering; it is NOT the product — it exists because every client already
holds an authenticated connection for the ops layer.

Control contract: console sends **intent (Twist)**; manifest declares contract type
(`drive: {type: "twist", max_v, max_w}`); console renders the matching widget; robot's
onboard controller realizes it per morphology. WASD semantics are platform-owned end to end.

## D5. Sibling-repo reality (audited 2026-08-07, both repos read end-to-end)

`../delivery-gdg-platform` (club monorepo) — memory vs. code:
- Orders/accounts: in Next.js; Go `InsertOrder` handler **commented out** → no order reaches
  the backend today. Matcher exists but is 1-order↔1-robot FIFO, zero factors. Pathing:
  **does not exist** (3-line comment stubs). `apps/command`: disconnected TCP/UDP echo demo.
- Orphaned-but-written: gRPC RobotService, state manager, Kafka plumbing, DB CRUD.
- Live WS hub bugs: match for unknown robot `return`s and **kills the hub goroutine**; map
  writes under RLock; nil-deref on early disconnect; pointer-compare on RobotID.

`../delivery-robo` (robot) — leaner than CLAUDE.md claims:
- **No Nav2 at all**, no lidar/RealSense drivers, no map/odom frames, no odometry
  (`ArduinoComms::sendMsg` never reads the port → encoder feedback dead; `open_loop: true`
  is load-bearing). Real: BNO08x IMU node, RTK/NTRIP bringup (nothing consumes the fix),
  1-motor+servo Arduino firmware, joystick→serial teleop (the only way it moves).
- External-twist injection point: `/cmd_vel` into twist_mux in the `sim/` stack; the
  currently-run `ros2_ws` path bypasses cmd_vel entirely.
- ⚠️ **NTRIP credentials committed in plaintext** in
  `ros2_ws/src/my_bringup/launch/master_launch.py` — rotate + move to env.
- ⚠️ Roadmap step 3 (real hardware) needs a real /cmd_vel path + encoder fix first.

## D6. Club integration: consume the SCOPE, not the CODE

Resolves the CLAUDE.md open decision about porting from `authoritative`/`command`: **port
nothing**. delivery-gdg doesn't become part of the OSS — it becomes the OSS's **first
customer**. Its robot-ops guts were the prototype; prototypes get retired.

| In delivery-gdg today | Fate |
|---|---|
| `internal/wsockets/` hub | → platform connections + presence |
| `apps/command` | deleted |
| gRPC RobotService | → manifest + telemetry protocol |
| `internal/state/` manager (robot+order store) | **stays club-side** — branch point for the brain's world model, hardened into a real FSM |
| Kafka robot-update plumbing | → platform bus |
| Robot rows/CRUD in `pkg/db.go` | → platform DB |
| `sim/delivery_roboman_client` | → `fleet_agent` SDK |
| Matcher (+13 tests), Next.js app, orders | **stays club-side** |

Carried as inputs only: `{type,payload}` envelope; robot FSM from `robot.proto`; the lesson
that presence-by-socket-lifecycle is too weak (→ heartbeats + leases).

**The command brain's promotion** (key mental model): the old "command server" = hub
(sockets/presence/relay → moves into platform) + brain (decisions → stays club-side as a
pure SDK service). The brain digests platform presence + telemetry + events into a world
model, decides (match/route/reroute), publishes — `send(robot, ch, payload)` per-robot or
`broadcast(ch, payload)` fleet-wide — accepting no inbound connections (purely an SDK
client on one outbound WebSocket). The brain's world model branches off from
`apps/authoritative/internal/state/` (manager.go/model.go): the robot+order registry with
the delivery FSM vocabulary (`IDLE → ASSIGNED → MOVING_TO_PICKUP → … → RETURNING`) and the
`GetAvailableRobots` dispatchability query. Today it is a state *store* (raw setters, no
transition rules); the brain hardens it into a state *machine* — enforced transitions plus
the dispatch loop (order + available robot → matcher → path service → assignment) — fed by
SDK snapshot-then-stream instead of the gRPC/WS hub, with its own `IsOnline` tracking
retired in favor of platform presence. Delivery FSM (club) and platform FSM
(`AUTONOMOUS | HELP_REQUESTED | TELEOP`) stay disjoint vocabularies. Club↔club calls go direct
(their infra, their choice — Kafka beside the platform is fine, bridged by a ~30-line SDK
client); **anything robot-facing or operator-visible rides the platform.**

**Sequencing:** vertical slice runs against the sim first. delivery-gdg migrates at roadmap
step 4, NOT earlier — don't couple protocol churn to the club semester (club starts
~2026-08-21).

Robot-side boundary mirror: generic `fleet_agent` (ships) ↔ **thin club node** (~50 lines,
theirs: assignment parsing, leg sequencing to Nav2, escalation policy, event emission) ↔
Nav2/base.

## D7. Generality enforcement (the OSS is general only if these hold)

1. **The wire is the wall** — generality is decided in `protocol/`; a schema mentioning
   delivery/campus/order = failed extraction. Checkable in code review.
2. **Second morphology in the sim from day one** (diff-drive + ackermann or a "drone" dot).
3. **Personal-fleet tier is the second reference user** (indoor, no GPS, fleet of one).
   Concrete trap already caught: **pose must be frame-relative (geographic OR local
   Cartesian), never assumed lat/lon.** ← candidate new design discipline for CLAUDE.md.
4. **10-minute stranger test**: two commands → console + sim fleet → drive a struggling
   robot; quickstart mentions no delivery anything.
5. Console = the fleet's **ops room, not your product's frontend**. Layer vocabulary is
   small + standardized (GeoJSON first; occupancy grid is the likely v-next for indoor);
   producers convert (RViz-Marker/Grafana/Foxglove precedent). No plugins; gaps grow the
   protocol via versioned layer/stream types.

## D8. Why not Kafka / Redis / RabbitMQ (recurring question — settled)

1. **Wrong topology**: brokers assume reachable clients on stable datacenter TCP; ours are
   NAT'd robots on flaky Wi-Fi + browsers. Presence must be first-class.
2. **Wrong delivery contract**: durable·ordered·retried delivers stale WASD late and in
   order after a hiccup. Driving needs latest-wins / drop-stale / never-retry (WebRTC).
   Different contract, not a slower broker.
3. **No ops layer**: no manifests→UI, queue, leases, deadman, teleop, replay, console —
   that's 98% of the product.

## D9. Competitive landscape (verified via web, 2026-08)

- **Transitive Robotics** — nearest neighbor. OSS framework (MQTT-based) + `transAct`
  dashboard template (Apache-2.0). Their Remote Teleop **does real P2P WebRTC**
  (~100ms, STUN/TURN, gamepads, browser-side soft deadman) — but it's a **paid capability,
  $20/robot/month**, and has **no** intervention queue, no multi-operator
  presence/claim/lease, no replay, browser-side-only safety. Differentiation sentence:
  *"they built the phone call and charge for it; we're building the 911 dispatch system —
  and giving away the phone call with it."*
- **Viam**: robot-side OSS, fleet/control plane proprietary cloud. **Foxglove**:
  observability, closed-source since 2023. **Open-RMF**: task orchestration, complementary
  (it's what adopters' command brains do). **ARMADA** (arXiv 2510.02298): research
  validation of the intervention-queue pattern. Hobby tier: roboportal/webrtc_ros glue.
- **P2P teleop is table stakes, not the innovation.** The unoccupied position: self-hosted
  OSS combining intervention queue + authority semantics (lease/exactly-one-driver/steal/
  spectate) + teleop + replay + manifest-driven console. Re-verify landscape before OSS
  release (roadmap step 5).

## D10. Server design: modular monolith, one process

```
server/
  gateway/    ws accept · auth · heartbeats · per-client send queues (BACKPRESSURE HERE)
  registry/   presence · manifests · fleets · tokens
  ops/        queue + lease state machine — THE ATOMIC CORE, one lock domain
              (this module is WHY it's a monolith: no network hop inside a transition)
  bus/        channel routing · size/rate limits · fan-out
              (no domain imports EVER; payloads typed as bytes)
  signaling/  webrtc offer/answer/ICE relay (+ maybe embedded TURN)
  replay/     recording index + storage
  store/      INTERFACE: sqlite today, postgres later ← tier-3 seam, drawn now, built never
  web/        console static files + http api
```

The server's nine jobs: connection fabric · registry/presence · telemetry fan-out · bus ·
intervention system · WebRTC signaling · replay · layer streams · console+API.
Robots are **always connected** (idle included) — the connection IS presence; orders/path
updates/broadcasts arrive over the same pipe. No second server.

Fan-out engineering (the real work at scale): per-subscriber rate limits, viewport-scoped
subscriptions, backpressure. NOT multi-instance replication — the server is stateful;
naive replicas split presence/leases.

**Language**: team decides. Workload is I/O-bound chat-server class; the latency-critical
media path is P2P in existing WebRTC stacks regardless. Choose on dev velocity, contributor
accessibility, deploy story, WS+WebRTC-signaling ecosystem. Claude's recommendation on
record: **Go server** (pion for signaling/TURN/future SFU; single static binary; Jaxon
writes Go) + **TypeScript console/SDK/sim**; all-TS is the runner-up if solo velocity
dominates. C++/Rust rejected (perf not needed; velocity/contributor cost real). GC-pause
fear retired: modern GC ~sub-ms; server adds ~0.1ms to a 15-100ms network budget.

## D11. Deployment & failure

- **Tier 1 (v1's only target)**: docker compose, one box — `fleet-server` (×1, stateful,
  serves console) + `coturn` + volume (sqlite + replays). $10 VPS carries hundreds of
  robots. Recovery: restart.
- **Tier 2**: same containers on k8s, still ONE server replica, Postgres, coturn ×N.
  Packaging, not architecture. (Compose and k8s are alternatives; compose can't "ask k8s
  for pods.")
- **Tier 3 (designed-for, unbuilt)**: state → Postgres+Redis, shard by fleet, TURN/SFU
  multiply regionally. Only when a real deployment demands it.
- **Scaling law: the smart part stays singular; the dumb parts (relay, console assets,
  SFU) multiply.** Per-teleop-session server cost ≈ a chat message (P2P carries the rest);
  there is nothing to "spin up" per session.
- **Failure story**: platform down → ops room dark, fleet does NOT freeze (club node +
  Nav2 onboard; fleet_agent redials with backoff; stuck robots wait in HELP_REQUESTED).
  TURN down → direct-path teleop still works. Deadman never depends on the server.
- Project hosts nothing (no SaaS); every fleet self-hosts an instance.
- Spectator fan-out is the one true P2P ceiling → SFU tier later, no protocol change.

## D12. SDK plan

| Package | Install | Audience |
|---|---|---|
| `sdk/ros2` (`fleet_agent`) | ROS workspace/apt | ROS2 robots — manifest, twist→cmd_vel, camera→WebRTC, lease check, deadman, request_help, channels-as-topics |
| `sdk/python` | pip | non-ROS robots, scripts, bridges (also fine for services) |
| `sdk/typescript` | npm | backend services (command brain, path service), sim |

Operators need no SDK (browser). The real contract is `protocol/` (JSON Schema, language-
neutral, types generated — quicktype or similar); SDKs are convenience, not gatekeepers.
Build order: schemas → TS SDK (sim + console need it) → python → fleet_agent (step 3).

## D13. Languages confirmed (2026-08-13) + robot-side media stack

D10's recommendation ran the gauntlet again (C++ client? Go on robot? Python too slow?)
and is **confirmed**: **Go server** (pion; WSS via coder/websocket) + **TypeScript
console/SDK/sim**. The new decision is the robot-side P2P client, which D10 left open:

- **The operator-side P2P client is the browser** — Chrome's WebRTC stack, free with the
  TS console. No operator P2P SDK exists.
- **`fleet_agent` is Python (rclpy)** for everything control-plane (WSS, manifest,
  twist→/cmd_vel, deadman, signaling). The **media path rides GStreamer `webrtcbin`** —
  camera → hardware encoder → DTLS/SRTP entirely in the C pipeline; Python only assembles
  it and exchanges SDP. aiortc is an acceptable dev/sim fallback behind the same backend
  interface, not the hardware target.
- Rationale: the latency-critical bytes (H.264 encode, packetization) run in C in *every*
  language option; twist is ~50 bytes at 20–50Hz (language contribution: noise); the
  deadman is a 300ms deadline Python meets trivially. So the binding language is chosen on
  velocity + ROS integration + club-member accessibility → Python. C++ rejected robot-side
  for the same reason D10 rejected it server-side.
- **Documented escape hatch, not built**: a Go/pion `fleet-agentd` sidecar + ~100-line
  rclpy shim over a localhost socket, if the Python control plane ever measurably hurts.
- Polyglot confusion is bounded by design: two core languages (Go infra, TS user-facing) +
  Python only at the robot edge; `protocol/` schemas are the single contract and each
  language's types are generated/validated from them (see docs/TESTING.md layer 1).

## D14. Operator access: one-time invites minted by `fleetctl` (2026-09-25)

- An operator gets a token by redeeming a **single-use, expiring invite key** through the
  existing `enroll.request` flow with `kind: operator`. No passwords are stored; the
  redeemed token is kept by the browser.
- Invites are minted by **`fleetctl`**, a separate admin CLI (gcloud/kubectl-style), which
  calls an **admin HTTP API** on the running server authenticated by an installation-level
  admin token (`FLEET_ADMIN_TOKEN`; the admin API is off when unset). `fleetctl` never
  touches the database.
- Rejected: a flag on `fleet-server` itself to mint invites. That only works on the box
  running the server and is a testing convenience, not a streamlined path.
- Later, a signed-in operator may mint invites from the console through the same API.

## D15. Console stack (2026-09-25)

**Vite + React + MapLibre GL, no SSR.** Built to static assets and embedded in
`fleet-server` via `go:embed`, so the deployment stays one image. It consumes
`sdk/typescript`. MapLibre is open source and renders local-frame poses on a blank style as
well as geographic ones (D7).

## Open decisions (still)

- Project name (rename before SDK imports spread). License: Apache-2.0 working default.
  (Go module path + schema `$id` host are placeholders until the rename.)

## Where the build actually is (2026-09-26)

Roadmap steps 1–2 are done and step 3 has started. Shipped: `protocol/` v0 schemas, the
Go server (auth, fleets, presence, intervention queue, channels, layers, WebRTC
signaling), `sdk/typescript`, `sdk/python`, `sdk/ros2` (`fleet_agent`), the console
(fleet list, live map, take over / WASD / hand back), `sim/`, and `fleetctl`. The honest
gap list lives in `docs/INTEGRATION.md` §8 and is the thing to read before designing
against any of it.

**Next work item: console layer rendering** (roadmap step 4). Layers already exist end to
end in the server and in both the TypeScript and Python SDKs, but the console cannot draw
them, so a service's declared layers have nowhere to go. The club path service — campus
waypoint graph, weighted A* — is being built against that hole right now, which makes
this the one platform gap a reference-deployment consumer is already waiting on. D4's
litmus test applies to the renderer: it consumes GeoJSON plus styling rules and never
learns the name `campus-graph`.

Then, in rough order: `fleet_agent`'s telemetry stubs as the robot's ROS inputs land
(odometry, `NavSatFix`, `BatteryState`, Nav2 — `sdk/ros2/README.md` names the blocker for
each); WebRTC media, so teleop video stops riding the bus; the project rename, which gets
more expensive every week that SDK imports spread; then replay.

Sim-first still holds: nothing lands without working against `sim/`.
