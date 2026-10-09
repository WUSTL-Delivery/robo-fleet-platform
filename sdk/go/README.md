# fleet SDK (Go)

The Go client for `fleet-server`, for backend services: one outbound WebSocket that
enrolls once, heartbeats, and reconnects by itself with the same token. On top of that
connection it gives a service the fleet's state (a snapshot, then events), domain
channels, and acked sends. `docs/INTEGRATION.md` section 4 is the worked example.

Two packages:

| Package | What |
|---|---|
| `fleetplatform/sdk/go/fleet` | the client: `Connect`, `Subscribe` and the `On*` callbacks, `Channel` |
| `fleetplatform/sdk/go/protocol` | the wire types and constants, shared with the server |

Go 1.24 or newer. One runtime dependency (`github.com/coder/websocket`).

## Install

`fleetplatform/sdk/go` is a **placeholder** module path until the project is named. It
does not resolve on the Go proxy, so use it from a checkout of this repo with a `replace`
in your service's `go.mod`:

```
require fleetplatform/sdk/go v0.0.0

replace fleetplatform/sdk/go => ../fleet-platform/sdk/go
```

Then `go mod tidy`. At the rename, the import changes by one search-and-replace of the
`fleetplatform` prefix.

## The shape

```go
ctx := context.Background()
client, err := fleet.Connect(ctx, fleet.Config{
	URL:       "ws://localhost:8080/ws",
	Kind:      fleet.Service,
	Name:      "my-service",
	EnrollKey: os.Getenv("FLEET_ENROLL_KEY"), // used once, on the first run
	TokenFile: "my-service.token.json",       // reused after that: same client id
})
if err != nil {
	log.Fatal(err)
}
defer client.Close()

// Register callbacks, then subscribe. Every snapshot is the whole truth (the answer
// to Subscribe, and a fresh one after each reconnect); events keep it current.
client.OnSnapshot(func(s fleet.Snapshot) { log.Printf("%d robots", len(s.Robots)) })
client.OnPresence(func(robotID string, online bool) { log.Println(robotID, "online:", online) })
if _, err := client.Subscribe(ctx, fleet.TopicPresence, fleet.TopicEvents); err != nil {
	log.Fatal(err)
}

// Channels carry your own data; the platform never looks inside it. Publish is
// at-most-once. SendAcked re-sends until the receiver's application accepts it.
err = client.Channel("jobs").SendAcked(ctx, robotID, map[string]string{"id": "job-1"}, 10*time.Second)
switch {
case err == nil: // accepted by the robot
case errors.Is(err, fleet.ErrNotFound): // the robot is not connected
default: // fleet.ErrTimeout and the rest: unknown, it may have arrived
}
```

The package documentation in `fleet/client.go`, `subscribe.go`, `channel.go` and
`acked.go` is the reference: reconnect policy, handler ordering, what each error means.
The acked-send convention itself is in `protocol/README.md`, so a receiver in another
language (the Python SDK's `on_acked`) interoperates.

## Example

[`examples/dispatcher`](examples/dispatcher/main.go) keeps a world model from the
callbacks, picks a robot that is online and `AUTONOMOUS`, sends it a job with `SendAcked`,
and requeues the job when that fails. With a server running (`docs/INTEGRATION.md`
section 1) and a robot that takes jobs (`sdk/python/examples/fake_robot.py`):

```bash
cd sdk/go
FLEET_URL=ws://localhost:8080/ws FLEET_ENROLL_KEY=... go run ./examples/dispatcher
#   snapshot: 1 robots
#   acked job-1 by r_6901d0470260c380
#   acked job-2 by r_6901d0470260c380
# kill the robot:
#   requeue job-3 (1 waiting): no robot is online and AUTONOMOUS
```

The token is saved to `~/.fleet/dispatcher.json` (or `FLEET_TOKEN_FILE`), so later runs
need no enrollment key.

## Tests

```bash
cd sdk/go
go test ./...
```

The tests talk to a real `fleet-server`, never a mock: each test package builds the
server from `../../server` once and each test starts its own instance on a free port.
The server needs a newer Go than the SDK does (see `server/go.mod`); when the `go`
running the tests is older, the build lets it download the toolchain it needs.
`protocol/contract_test.go` validates the Go wire types against `protocol/schemas` and
the fixtures.

One test crosses languages: `TestSendAckedToThePythonRobot` sends acked jobs to
`sdk/python/examples/fake_robot.py`. It needs a Python that can import the Python SDK's
dependencies, and skips when `python3` cannot. Point it at one with `FLEET_TEST_PYTHON`:

```bash
python3 -m venv /tmp/fleet-venv && /tmp/fleet-venv/bin/pip install -e ../python
FLEET_TEST_PYTHON=/tmp/fleet-venv/bin/python go test ./fleet -run TestSendAckedToThePythonRobot -v
```
