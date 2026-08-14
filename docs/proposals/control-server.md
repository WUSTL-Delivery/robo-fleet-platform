# Proposal: The Control Server — Robot Entry Point & Dispatch Integration

> Status: **proposed** · Author: Jaxon · 2026-08-11
> Companion to `docs/DESIGN.md` (D1–D12). This document specifies the control server —
> the platform's `server/` component — and how the club's delivery system
> (`../delivery-gdg-platform`) runs on top of it.

## 1. Summary

The control server is the **single server every robot connects to on boot**: connection
fabric, presence, telemetry fan-out, message bus, intervention queue, WebRTC signaling,
and replay — one process, one box.

For the club deployment it carries the full delivery loop:

1. Robot boots → dials out over WSS to `fleet.wudelivery.<tld>` → authenticated with its
   per-robot credential, capability manifest registered, **presence = online**.
2. Club services (matcher, path service, command brain) learn the robot is available
   through SDK subscriptions to platform events.
3. A match + waypoint path, computed club-side, is delivered to the robot as an
   **assignment payload** over the bus. The robot's club node parses it and executes it
   leg by leg with Nav2; execution is fully abstracted from the platform.
4. The robot streams pose/state/battery telemetry up the same socket; the platform fans
   it out to the ops console and to subscribed services.
5. When autonomy fails, the robot raises help → intervention queue → an operator claims
   a **lease** → P2P WebRTC teleop (video + twist) → handback → the brain replans.
6. Per-leg traversal reports flow as domain events from the robot to the path service,
   which updates the campus graph's edge weights.
7. A small **bridge service** forwards platform events to the club's Kafka topics for
   services that consume them there.

## 2. Role in the system

The control server owns exactly the platform vocabulary: connections, manifests,
presence, telemetry, channels, the intervention queue, leases, signaling, replay. It is
the source of truth for *who is connected, who has authority over which robot, and what
each robot can do*.

Everything delivery-shaped — orders, matching, pathing, the campus graph — lives in
`delivery-gdg-platform` as services that connect to the platform like any other client.
The club's decision-making role survives as the **command brain**: a club-side service
built on the TypeScript SDK that accepts no inbound connections — purely a client,
holding a single outbound WebSocket to the control server. It digests presence +
telemetry + events into a world model, decides (match / route / reroute), and
publishes — `send(robot, channel, payload)` per-robot or `broadcast(channel, payload)`
fleet-wide.

This replaces the club's current `apps/command` and the socket-hub portion of
`apps/authoritative` (per the D6 migration table); the matcher, orders, and the Next.js
app stay club-side unchanged.

### 2.1 Why the separation is worth the extra hop

Splitting brain from control server means an assignment travels
`brain → control server → robot` instead of `brain → robot`. That extra hop is a
deliberate trade, and it's cheap where it's paid and valuable where it buys:

**What the hop costs.** The added leg is brain → control server: two server-side
processes, same VPS or same region, so ~1 ms plus an in-memory socket lookup. The slow
leg of any message to a robot is the radio link (campus Wi-Fi/LTE, 15–100 ms), and that
leg exists in every possible design. Moreover, the traffic riding this path —
assignments, presence, telemetry, edge reports — is latency-tolerant: whether a
waypoint list arrives in 40 ms or 45 ms changes nothing. The one flow where latency
genuinely matters, teleop video + twist, **bypasses the server entirely** (P2P WebRTC;
the server only does signaling). The hop is paid exactly where it's free.

**What the hop buys.** Robots are behind NAT and must dial out to *something*; the only
real alternatives to this design are worse:

- *Brain as the socket server* — the brain would have to own WSS accept, auth,
  heartbeats, reconnects, presence, and backpressure alongside delivery logic. That is
  the current `apps/authoritative` architecture, hub bugs included, and it means every
  delivery-logic change redeploys the process robots depend on for connectivity and
  safety.
- *Robots hold two connections* (control server for teleop, brain for assignments) —
  two presence truths that can disagree, duplicated reconnect logic, and a flaky robot
  link spent twice.

With the split, there is **one connection fabric and one source of truth** for
presence, leases, and authority; the brain is a decision process that can crash,
redeploy, or be rewritten mid-semester **without dropping a single robot connection**
(and can rebuild its world model on restart — see §4); and the control server stays
generic enough to open-source. This is the same trade made by every authoritative game
server, by Slack's message servers, and by Tailscale's DERP relays: clients connect to
dumb, stable fabric; smart services sit behind it.

**It also keeps horizontal scaling open.** Because the brain addresses robots by
identity (`send("robot_42", …)`), never by socket, the connection fabric can later
become *multiple* control servers behind a routing layer — look up which server holds
the robot, forward the bytes — with **zero changes** to the brain, the SDK, the robot
agent, or the protocol. The stateful core (presence, queue, leases) is what makes
naive replication unsafe: exactly-one-driver requires atomic state transitions, so two
servers must never both reason about the same robot's lease. The designed path (D11
tier 3) is therefore **shard by fleet** — each fleet's robots, queue, and leases live
whole on one server, keeping every authority decision local and atomic while capacity
scales across fleets. v1 deliberately runs a single instance: one small VPS carries
hundreds of robots (~1 KB/s per robot; teleop is P2P, so a session costs the server
about as much as a chat message), and the components that hit limits first — TURN
relay bandwidth, spectator fan-out — are dumb ones (coturn, later an SFU) that already
multiply independently.

**Next steps on this axis:** none in v1 beyond keeping the seam clean — the `store/`
interface (sqlite → postgres) is drawn now and built never, and nothing in `ops/` may
assume it can see more than one fleet's state "for convenience." Fleet-sharding gets
built only when a real deployment saturates a single instance; it is packaging and
routing work at that point, not a protocol or client change.

## 3. Connection lifecycle

### 3.1 Reaching the server

`fleet.wudelivery.<tld>` resolves (A/CNAME) to the box running `fleet-server` +
`coturn` — the tier-1 docker-compose deployment (D11). TLS terminates at the server;
ordinary WebPKI, nothing pinned. The main `wudelivery` site can live anywhere else;
the fleet subdomain is its own record.

### 3.2 Identity & enrollment

Every robot holds a **per-robot credential**; identity is always derived server-side
from the credential, never claimed by the client. Two-step provisioning, the Tailscale
auth-key pattern:

1. An admin creates a **fleet enrollment key** in the console (scoped, expiring,
   revocable).
2. On first boot the robot presents the enrollment key; the server registers it, mints
   an opaque per-robot token, and returns it. The robot stores the token and uses it
   for every subsequent connection.

Tokens are opaque random strings stored hashed in the platform DB (the robots/tokens
runtime state of D1) — revocable the instant a robot is lost, stolen, or compromised,
which stateless formats like JWT handle badly. A compromised robot therefore
compromises exactly one identity, not the fleet. (Signed stateless credentials become
worth revisiting at tier 3, when verification may happen on more than one server.)

### 3.3 Handshake

```
robot                          control server
  │ ── WSS connect ──────────────►│
  │ ── hello {token, agent ver} ─►│  auth: token lookup → robot_id + fleet
  │ ◄─ welcome {robot_id, time} ──│  identity confirmed by server, never claimed
  │ ── manifest {...} ───────────►│  registry: capabilities → console UI
  │ ◄─ ack ───────────────────────│  presence: ONLINE (event emitted on the bus)
  │ ⇄  heartbeat ⇄                │  silence ⇒ presence lost
```

The manifest declares the drive contract (`{drive: {type: "twist", max_v, max_w}}`),
camera streams, battery, and the **domain channels** the robot's club node speaks
(e.g. `assignment`, `edge_report`). The console renders only what's declared.

Presence is heartbeat-based with expiry, not socket-lifecycle — and this applies to
**every** client class: robots, services, and operator consoles alike. Reconnection is
the fleet_agent's job: exponential backoff, same token. A lease outlives a network blip
shorter than its TTL; the robot-side deadman covers the gap.

## 4. Availability → matching

On connect/disconnect the platform emits typed presence events (`robot.online`,
`robot.offline`) on the bus, and every intervention transition is likewise a typed
lifecycle event (`robot.help_requested`, `robot.lease_granted`,
`robot.lease_released`). **Every SDK subscription opens with a state snapshot** —
current presence, FSM state, and lease holders for the fleet — followed by the live
event stream. Snapshot-then-stream is what makes the brain genuinely restartable: a
freshly deployed brain reconstructs its world model from the snapshot plus its own
club-side state (orders in flight), rather than depending on having witnessed every
event since the beginning of time.

From platform presence + live telemetry + club state (orders in flight, battery,
current FSM state) the brain derives which robots are **dispatchable** and feeds that
to the matcher.

The world model branches off from the club's state manager
(`apps/authoritative/internal/state/`) — the robot + order registry
with the delivery lifecycle (`IDLE → ASSIGNED → MOVING_TO_PICKUP → AT_PICKUP →
MOVING_TO_DROPOFF → AT_DROPOFF → RETURNING`), whose `GetAvailableRobots` query is
exactly this dispatchability derivation. Today that code is a state *store* — raw
setters, no transition rules; inside the brain it becomes a state *machine*: enforced
legal transitions, plus the dispatch loop around it (order created + robot available
→ matcher → path service → `send(robot, "assignment", …)`). Its inputs swap from the
old gRPC/WS hub to SDK subscriptions, and its own `IsOnline` tracking retires in
favor of platform presence — one liveness truth. Note the two vocabularies stay
disjoint: the delivery statuses are the club's FSM, the platform's is
`AUTONOMOUS | HELP_REQUESTED | TELEOP`; a robot can be `MOVING_TO_DROPOFF` and
`AUTONOMOUS` at once, and neither leaks into the other.

This is the integration seam: club services consume platform *events*, not platform
internals. The platform's database describes the platform's world (fleets, tokens,
robots, leases); the club's database describes the club's world (users, orders,
deliveries).

## 5. Dispatch: delivering the match + path

When the matcher pairs an order with a robot and the path service produces a waypoint
list (server-side A\* over the campus graph with live travel-time weights):

```
command brain ── send(robot_42, "assignment", {order, waypoints[], deadline}) ──► bus
bus           ── routes bytes; size/rate limits ──► robot_42's socket
club node     ── parses assignment; sequences Nav2 goals leg by leg
```

- The assignment payload rides an opaque domain channel; its schema is club property,
  versioned in `delivery-gdg-platform`. Nothing order- or path-shaped enters
  `protocol/`.
- The bus is **at-most-once by design**: that keeps the platform dumb, and reliable
  delivery is the domain's contract to provide where it needs it. Here, the brain
  re-sends until the club node confirms on the same channel, and assignments are
  idempotent club-side. The TypeScript SDK ships a small request/ack helper so each
  service doesn't reinvent this pattern.
- On the robot: generic `fleet_agent` (platform SDK) ↔ thin club node (~50 lines:
  assignment parsing, leg sequencing, escalation policy) ↔ Nav2/base. Navigation and
  SLAM are entirely the robot stack's concern.

## 6. Telemetry upstream

Over the same socket, continuously:

- **Typed platform telemetry** — frame-relative pose (geographic frame on campus,
  local Cartesian indoors), velocity, battery, FSM state
  (`AUTONOMOUS | HELP_REQUESTED | TELEOP | …`), health flags (including teleop media
  link health during a session). Consumed by the console map, fleet list, replay
  recorder, and the brain's world model.
- **Typed help** — `request_help {reason, context}` → intervention queue (§7).
- **Domain events on channels** — `edge_report {edge_id, seconds, blocked?}` emitted
  by the club node as each leg completes. The path service subscribes and updates edge
  weights, so the campus graph learns from fleet history.

Per-robot steady load is ~1 KB/s — chat-user class. Bus-enforced size/rate limits keep
the control plane light; video never rides this pipe.

## 7. Teleop & intervention

1. The robot's club node (escalation policy is club code) decides it needs help →
   `request_help` → server FSM: `AUTONOMOUS → HELP_REQUESTED` → queue entry on every
   ops console. Day-one metric: help raised → first operator eyeball.
2. An operator claims → the server issues an exclusive, expiring, renewable **lease**
   → `TELEOP`. Steal = revoke + reissue, never share. Every state transition is a
   server-side action; the P2P channel never carries authority.
3. The server does **WebRTC signaling only**; video + twist flow P2P (operator browser
   ↔ robot, both dialing out; ICE races LAN / hole-punched / TURN paths). Target
   <200 ms glass-to-glass.
4. **Robot-side arbitration is layered.** The fleet_agent accepts twist only bearing
   the current lease id and publishes it to the highest-priority input of the robot's
   twist mux — so even a buggy club node cannot fight the operator at the actuator.
   On entering `TELEOP`, the club node additionally cancels its active Nav2 goal:
   autonomy must not keep planning against a robot that isn't obeying it, and — the
   real hazard — a stale goal left alive would reclaim the mux the instant the
   operator releases, lurching the robot toward wherever it was headed before the
   takeover. Autonomy re-arms only when a fresh goal is issued after handback (step
   6). Throughout, local obstacle avoidance stays active (safeguarded velocity
   control) and a ~300 ms **deadman** runs: no valid twist → zero velocity. Physics
   never waits on the network.
5. **Operator loss is the server's transition, not the robot's.** Operator consoles
   are heartbeated presence clients like everything else; if the operator's presence
   lapses or the lease goes unrenewed, the server revokes the lease and returns the
   robot to `HELP_REQUESTED` — back in the queue for the next operator. The robot's
   role during the gap is to fail safe (the deadman has already stopped it) and to
   report media-link loss as telemetry; it never re-queues itself off P2P state. The
   asymmetry matters because the WebRTC path can die while the operator's WSS is
   healthy (a TURN hiccup): if the robot decided for itself, one robot could hold a
   live lease *and* sit in the queue — exactly the two-drivers ambiguity leases exist
   to kill.
6. **Handback always replans — no "did the operator go too far?" threshold.** On
   resolve, the server transitions to `AUTONOMOUS`; the club node reports its leg
   invalidated along with current pose; the brain has the path service route fresh
   from the nearest graph node and dispatches a new assignment. If the operator barely
   moved the robot, the replan degenerates to "continue to the next waypoint" — same
   code path, no special cases. This also keeps the operator out of graph surgery: no
   console UI for picking waypoints, which would be domain knowledge in the platform.
   Every intervention is black-box recorded for scrubbable replay.

Operators can also open teleop **proactively** on any robot (claim from `AUTONOMOUS`,
same lease mechanics, same arbitration in step 4) — e.g. when the brain or an anomaly
highlight in the console ("pose unchanged for 60 s while autonomous") suggests a robot
is silently stuck. The *judgment* that a delivery is off-track (no progress along its
leg, blown ETA) is the brain's: it watches pose against assignments and either directs
its club node to raise help or flags an operator.

## 8. Cross-service communication: the Kafka bridge

Club services keep speaking Kafka to each other (`apps/authoritative` already has the
plumbing). A ~30-line club-side **bridge service**, built on the TypeScript SDK, is the
single point where the two messaging worlds touch — **one direction in v1**:

```
platform bus ── TS SDK bridge ──► club Kafka
   robot.online / pose / edge_report  →  topics for robot_manager & friends
```

The reverse path (Kafka topics re-published as bus sends) is deliberately deferred: a
durable, replayable log feeding an at-most-once command bus means a bridge restart can
replay stale commands at robots. If a club service needs to command a robot, it uses
the SDK directly. Rule of thumb: club↔club traffic rides Kafka; anything robot-facing
or operator-visible rides the platform.

## 9. End-to-end sequence

```
robot boots ─ DNS ─► WSS connect ─► hello/auth ─► manifest ─► ONLINE
                                                        │
             command brain (SDK) ◄── robot.online ──────┘
user orders ─► club web/API ─► matcher pairs order + robot
path service ─► A* over campus graph ─► waypoint list
brain ─► send(robot, "assignment", {...}) ─► bus ─► club node ─► Nav2 legs
robot ─► pose/state telemetry ─► console map · replay · brain world model
robot ─► edge_report per leg ─► path service ─► graph weights update
        ⋯ a leg fails ⋯
robot ─► request_help ─► queue ─► operator claims ─► lease ─► P2P teleop
operator drives it clear ─► handback ─► AUTONOMOUS ─► leg invalidated
brain ─► replan from nearest node ─► fresh assignment ─► Nav2 legs resume
delivery completes ─► club-side order close-out
```

The platform half of this sequence is roadmap steps 1–2 against the sim. The club half
(matcher beyond FIFO, path service, brain) is club-side work built during the semester
on top of it — the D5 audit is the honest baseline for how much of it exists today.

## 10. Build order

1. **Protocol v0** (`protocol/`): envelope + hello (per-robot token) + enrollment
   exchange, manifest, twist + lease, frame-relative pose/telemetry, help/lease
   lifecycle messages, **snapshot-then-stream subscribe semantics**, layer
   declaration. Domain channels come free with the envelope.
2. **Vertical slice** (roadmap 1–2): server skeleton + sim robot + console — connect,
   see pose, claim, WASD, handback — zero hardware, before the club semester
   (~2026-08-21).
3. **Club integration** (roadmap 4): bridge service + brain migration; matcher and
   path service consume presence via SDK/bridge; `apps/command` retired.
4. **Real hardware** (roadmap 3): encoder feedback + `/cmd_vel` path + twist mux +
   Nav2 bring-up on delivery-robo, then `fleet_agent` + thin club node.
