# fleet-platform

Open-source, self-hosted **multiplayer ops platform for robot fleets**. Robots and
services dial out to a command server; a web console renders whatever they declare;
when autonomy fails, a human takes over in one click.

Full illustrated brief (pitch, diagrams, roadmap):
https://claude.ai/code/artifact/65c4a0dd-600d-4ef0-8c6e-0ad88974f5bf

**READ `docs/DESIGN.md` before doing any design or implementation work** — it records all
settled architecture decisions (D1–D12: topology, trust model, scope boundary, club
integration, competitive landscape, server shape, deployment) so they don't get re-litigated,
plus the sibling-repo audit findings and the next work item (protocol v0).

`docs/INTEGRATION.md` is the guide for building an app on the platform (wire protocol
essentials, the four client roles, runnable TS service + Python robot examples, the
delivery-gdg migration map, deployment, and the honest list of v0 gaps).

## Context

Built by the software lead of the WashU GDG delivery robot project. Two sibling repos
(same parent folder, `../`):

- `../delivery-gdg-platform` — club monorepo (order client, matching, path service).
  Becomes the **reference deployment** built ON this platform, via the SDK.
- `../delivery-robo` — the physical robot: ROS2 (Nav2, RPLidar, RealSense, RTK GPS
  via ublox_dgnss, IMU, wheel encoders). Will run this platform's `fleet_agent` node.

The club's dispatch architecture (context, lives in the club repo, NOT here): campus
is a waypoint graph, edges weighted by live travel-time estimates; server-side A*
produces a waypoint list; the robot just runs Nav2 leg by leg. Robots report actual
traversal times back → weights learn from fleet history. When a leg fails, the robot
escalates → that escalation lands in THIS platform's intervention queue.

## Core concepts (the product)

1. **Intervention queue** — a robot's autonomy hits low confidence, it raises its
   hand; an operator claims it, teleops it out, hands control back. State machine:
   AUTONOMOUS → HELP_REQUESTED → (operator claims) → TELEOP → (resolved) → AUTONOMOUS.
   Exactly-one-driver semantics enforced by the server; deadman/watchdog stop on the
   robot if the operator link drops.
2. **Multiplayer** — presence (who's online, who's watching what, who has the wheel),
   claim/steal/handback authority, read-only spectating of a live takeover,
   black-box recording of every intervention (scrubbable replay).
3. **Capability manifest** — on connect, a robot self-describes:
   `{drive: {type: "twist", max_v}, camera: {streams}, battery: {}, behaviors: []}`.
   The console renders ONLY what the manifest declares. No capability → no UI.
4. **Layer streams** — services are clients too. They declare map *layers*
   (e.g. `campus-graph`, type `geojson`, per-edge weight/eta/blocked + styling rules)
   and can subscribe to platform events (e.g. `robot.edge_traversed`). The platform
   is the message bus of the whole system, not just a viewer.
5. **Dial-out connectivity** — robots AND services initiate the connection outward
   (wss) like Tailscale nodes; works on campus Wi-Fi / home Wi-Fi / LTE, no port
   forwarding. Teleop video+control rides a direct WebRTC path (<200ms target);
   the server does signaling.
6. **Fleets as tenants** — multiple fleets (club fleet, personal home robots) on one
   server, separated by auth. Designed in from day one, never forked.

## Control model / terminology

- Console sends **intent, not actuation**: body-frame velocity setpoints
  (**Twist**, à la `geometry_msgs/Twist` / `cmd_vel`). W = +linear.x, not "spin motors".
- Each robot's onboard controller realizes the twist for its morphology: diff-drive
  wheel kinematics, hexapod gait generator, drone attitude controller. This boundary
  is the hardware abstraction layer.
- Teleop is **safeguarded velocity control** (robot keeps local obstacle avoidance
  active during takeover); normal ops is **supervisory control** (goals, autonomy
  executes). Sheridan's levels of teleoperation.
- The manifest declares the control **contract type** (`twist`, later maybe
  `joint_jog`, `altitude_hold`, …), and the console renders the matching input
  widget. Small standardizable set of control types over infinite robot bodies.

## Planned repo layout

```
protocol/     # SOURCE OF TRUTH: JSON Schemas — manifest, control msgs, layer types.
              # Server, console, SDKs validate against / generate from these.
server/       # command server: auth, fleets, presence, intervention queue,
              # pub/sub + layer streams, replay storage, WebRTC signaling
console/      # web app: map, fleet list, queue, teleop UI, replay scrubber
sdk/
  typescript/ # for services (club path service is first consumer)
  python/     # for hobby robots / quick agents
  ros2/       # fleet_agent package: manifest, twist bridge, camera, watchdog
sim/          # fake fleet speaking the REAL protocol; dev environment + demo
docs/         # quickstart good enough for a club member in 10 minutes
```

## Design disciplines (non-negotiable)

- **No domain code in the platform.** If console code ever says
  `if (layer.name === "campus-graph")`, that logic belongs in the club's path
  service or in styling config. Delivery/graph/order logic lives in
  `delivery-gdg-platform`, full stop.
- **Contract-first.** Protocol schemas change before code does.
- **Sim-first.** Every feature must work against the sim fleet with zero hardware.
- **Never build a platform feature the reference deployment doesn't immediately use.**

## Roadmap

1. **Vertical slice** — protocol v0 (manifest + twist + pose), server skeleton, one
   sim robot, console: live pose on a map, click take-over, WASD drive, handback.
2. **Queue + presence** — 20 sim robots, intervention queue, operator presence.
   Two people triaging a misbehaving sim fleet = the demo.
3. **Real hardware** — `fleet_agent` on delivery-robo: RealSense over WebRTC,
   twist in, deadman watchdog.
4. **Manifests & layer streams** — client-agnostic protocol; path service publishes
   the campus graph; console renders it generically (GeoJSON layer renderer).
5. **Personal fleet & OSS release** — multi-tenant fleets, publish server+console+SDK.

Club starts ~2026-08-21 (two weeks from doc creation). Goal for the gap: roadmap
steps 1–2 done, plus a quickstart, so members onboard onto a working platform.

## Open decisions

- Project name (repo is placeholder-named `fleet-platform`; rename before SDK
  imports spread).
- License: Apache-2.0 is the working default (patent grant matters for infra).
- Server/console tech stack: not yet chosen — pick during vertical slice. Whatever
  is chosen, the protocol stays language-neutral (JSON Schema).
- What (if anything) to port from `../delivery-gdg-platform/apps/authoritative` and
  `apps/command` — they are conceptually the command server's ancestors.
