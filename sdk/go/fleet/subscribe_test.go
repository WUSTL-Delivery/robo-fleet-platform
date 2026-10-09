package fleet_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"fleetplatform/sdk/go/fleet"
	"fleetplatform/sdk/go/internal/fleettest"
	"fleetplatform/sdk/go/protocol"
)

// rec is one thing a subscriber was told, in the order it was told.
type rec struct {
	kind    string // state | snapshot | presence | telemetry | help | lease | channel
	state   fleet.State
	robotID string
	online  bool
	snap    fleet.Snapshot
	tel     fleet.Telemetry
	help    fleet.HelpDetails
	lease   fleet.LeaseChange
	channel protocol.ChannelMessage
}

func (r rec) String() string {
	switch r.kind {
	case "state":
		return "state:" + string(r.state)
	case "lease":
		return "lease:" + r.lease.Event
	case "presence":
		return fmt.Sprintf("presence:%v", r.online)
	}
	return r.kind
}

// feed records every callback of one client in delivery order. All callbacks
// run on the client's delivery goroutine, so the order here is the order the
// client delivered them in.
type feed struct {
	mu   sync.Mutex
	all  []rec
	wake chan struct{}
}

func newFeed() *feed { return &feed{wake: make(chan struct{}, 1)} }

func (f *feed) add(r rec) {
	f.mu.Lock()
	f.all = append(f.all, r)
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// state is the Config.OnState hook, so the feed sees the first open too.
func (f *feed) state(ch fleet.StateChange) { f.add(rec{kind: "state", state: ch.State}) }

// attach registers every typed callback, as a command brain would.
func (f *feed) attach(c *fleet.Client) {
	c.OnSnapshot(func(s fleet.Snapshot) { f.add(rec{kind: "snapshot", snap: s}) })
	c.OnPresence(func(id string, online bool) { f.add(rec{kind: "presence", robotID: id, online: online}) })
	c.OnTelemetry(func(id string, t fleet.Telemetry) { f.add(rec{kind: "telemetry", robotID: id, tel: t}) })
	c.OnHelp(func(id string, h fleet.HelpDetails) { f.add(rec{kind: "help", robotID: id, help: h}) })
	c.OnLease(func(ch fleet.LeaseChange) { f.add(rec{kind: "lease", robotID: ch.RobotID, lease: ch}) })
	c.On(protocol.TypeChannelMessage, func(env protocol.Envelope) {
		var msg protocol.ChannelMessage
		json.Unmarshal(env.Payload, &msg)
		f.add(rec{kind: "channel", robotID: msg.From, channel: msg})
	})
}

func (f *feed) list() []rec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.all)
}

func (f *feed) len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.all)
}

// waitFor returns the first record at index >= from that matches, and its index.
func (f *feed) waitFor(t *testing.T, from int, what string, match func(rec) bool) (rec, int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		for i, r := range f.list() {
			if i >= from && match(r) {
				return r, i
			}
		}
		select {
		case <-f.wake:
		case <-deadline:
			t.Fatalf("timed out waiting for %s; feed from %d: %v", what, from, f.list()[min(from, f.len()):])
		}
	}
}

func isKind(kind string) func(rec) bool { return func(r rec) bool { return r.kind == kind } }

func isOpen(r rec) bool { return r.kind == "state" && r.state == fleet.StateOpen }

// checkSnapshotFirst asserts the ticket's ordering rule over the whole feed:
// after every (re)connect, the first stream delivery is a snapshot.
func (f *feed) checkSnapshotFirst(t *testing.T) (opens int) {
	t.Helper()
	all := f.list()
	for i, r := range all {
		if !isOpen(r) {
			continue
		}
		opens++
		for _, next := range all[i+1:] {
			if next.kind == "state" {
				if next.state == fleet.StateOpen {
					break
				}
				continue
			}
			if next.kind != "snapshot" {
				t.Fatalf("first stream delivery after open #%d is %v, want a snapshot; feed: %v", opens, next, all)
			}
			break
		}
	}
	return opens
}

func send(t *testing.T, c *fleet.Client, typ string, payload any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Send(ctx, typ, payload); err != nil {
		t.Fatalf("send %s: %v", typ, err)
	}
}

func subscribe(t *testing.T, c *fleet.Client, topics ...string) fleet.Snapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snap, err := c.Subscribe(ctx, topics...)
	if err != nil {
		t.Fatalf("Subscribe(%v): %v", topics, err)
	}
	return snap
}

func f64(v float64) *float64 { return &v }

// The ticket's done condition: the subscriber half of the server's
// TestIntegrationStoryline (server/internal/app/integration_test.go), with
// every client an SDK client and the subscriber a service keeping a world
// model from callbacks, as the command brain will. In the middle of it the
// brain's connection is cut: it must come back, subscribe again by itself and
// be handed a fresh snapshot before any further event.
func TestBrainStorylineWithReconnect(t *testing.T) {
	srv := fleettest.Start(t)
	proxy := srv.Proxy(t) // only the brain goes through it

	// Robot enrolls, connects and declares its manifest.
	robot := mustConnect(t, fleet.Config{URL: srv.WSURL, Kind: fleet.Robot, Name: "sim-01", EnrollKey: srv.EnrollKey})
	robotID := robot.ClientID()
	send(t, robot, protocol.TypeManifest, protocol.Manifest{
		Drive:    &protocol.Drive{Type: "twist", MaxVMps: 1.5, MaxWRadps: 2.0},
		Battery:  &struct{}{},
		Channels: []string{"assignment"},
	})

	operator := mustConnect(t, fleet.Config{URL: srv.WSURL, Kind: fleet.Operator, Name: "op-1", EnrollKey: srv.OperatorInvite(t)})
	granted := make(chan protocol.Lease, 4)
	operator.On(protocol.TypeLeaseGranted, func(env protocol.Envelope) {
		var lease protocol.Lease
		json.Unmarshal(env.Payload, &lease)
		granted <- lease
	})

	// The brain: callbacks first, then Subscribe.
	f := newFeed()
	brain := mustConnect(t, fleet.Config{
		URL: proxy.WSURL, Kind: fleet.Service, Name: "command-brain", EnrollKey: srv.EnrollKey,
		Backoff: fastBackoff, OnState: f.state,
	})
	brainID := brain.ClientID()
	f.attach(brain)
	topics := []string{fleet.TopicPresence, fleet.TopicEvents, fleet.TopicTelemetry, fleet.ChannelTopic("edge_report")}

	// Snapshot-then-stream shows the robot online with its manifest. Repeat
	// until the manifest (sent on another connection) has landed; subscribes
	// are additive and each one returns a fresh snapshot.
	var snap fleet.Snapshot
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		snap = subscribe(t, brain, topics...)
		if len(snap.Robots) == 1 && snap.Robots[0].Presence == "online" && snap.Robots[0].Manifest != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("snapshot never showed the robot online with its manifest: %+v", snap)
		}
	}
	if r := snap.Robots[0]; r.RobotID != robotID || r.State != protocol.StateAutonomous || r.Help != nil || r.Lease != nil {
		t.Fatalf("snapshot: %+v", r)
	}
	if first, _ := f.waitFor(t, 0, "the first snapshot callback", isKind("snapshot")); len(first.snap.Robots) != 1 {
		t.Fatalf("OnSnapshot got %+v", first.snap)
	}

	// Telemetry fans out to OnTelemetry, decoded.
	send(t, robot, protocol.TypeTelemetry, protocol.Telemetry{
		Pose:    &protocol.Pose{Frame: "local", FrameID: "map", XM: f64(1.0), YM: f64(2.0)},
		Battery: &protocol.Battery{Pct: f64(87)},
	})
	tel, _ := f.waitFor(t, 0, "telemetry", isKind("telemetry"))
	if tel.robotID != robotID || tel.tel.Pose == nil || *tel.tel.Pose.XM != 1.0 || *tel.tel.Battery.Pct != 87 {
		t.Fatalf("telemetry: %+v", tel)
	}

	// The robot raises its hand: OnHelp carries the queue entry.
	send(t, robot, protocol.TypeHelpRequest, protocol.HelpRequest{
		Reason: "nav_goal_failed", Context: map[string]any{"attempts": 3},
	})
	help, _ := f.waitFor(t, 0, "help", isKind("help"))
	if help.robotID != robotID || help.help.Reason != "nav_goal_failed" || help.help.Context["attempts"] != float64(3) || help.help.RequestedAtMs <= 0 {
		t.Fatalf("help: %+v", help)
	}

	// An operator claims: OnLease says the robot is under teleop.
	send(t, operator, protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotID})
	var lease protocol.Lease
	select {
	case lease = <-granted:
	case <-time.After(5 * time.Second):
		t.Fatal("operator got no lease.granted")
	}
	claimed, _ := f.waitFor(t, 0, "lease granted", isKind("lease"))
	if ch := claimed.lease; ch.Event != protocol.EventRobotLeaseGranted || ch.RobotID != robotID || ch.State != protocol.StateTeleop ||
		ch.LeaseID != lease.LeaseID || ch.Lease == nil || ch.Lease.OperatorID != operator.ClientID() {
		t.Fatalf("lease granted: %+v", ch)
	}

	// The brain's connection is cut mid-teleop. It reconnects as the same
	// client and the first thing it is told is a fresh snapshot, which shows
	// what the stream said before the cut: TELEOP, under that lease.
	cut := f.len()
	proxy.Drop()
	_, i := f.waitFor(t, cut, "reconnecting", func(r rec) bool { return r.kind == "state" && r.state == fleet.StateReconnecting })
	_, i = f.waitFor(t, i, "open again", isOpen)
	fresh, i := f.waitFor(t, i, "a stream delivery after the reconnect", func(r rec) bool { return r.kind != "state" })
	if fresh.kind != "snapshot" {
		t.Fatalf("first delivery after the reconnect is %v, want a snapshot", fresh)
	}
	if len(fresh.snap.Robots) != 1 {
		t.Fatalf("fresh snapshot: %+v", fresh.snap)
	}
	if r := fresh.snap.Robots[0]; r.RobotID != robotID || r.Presence != "online" || r.State != protocol.StateTeleop ||
		r.Lease == nil || r.Lease.LeaseID != lease.LeaseID || r.Manifest == nil {
		t.Fatalf("fresh snapshot robot: %+v", r)
	}
	if brain.ClientID() != brainID {
		t.Fatalf("reconnected as %q, want %q", brain.ClientID(), brainID)
	}

	// Everything after this point arrives on the second connection, so it
	// proves each topic was subscribed again without the test asking.

	// Handback: lease released, robot back to AUTONOMOUS (topic events).
	send(t, operator, protocol.TypeLeaseRelease, protocol.LeaseRelease{LeaseID: lease.LeaseID, Resolution: "resolved"})
	released, i := f.waitFor(t, i, "lease released", isKind("lease"))
	if ch := released.lease; ch.Event != protocol.EventRobotLeaseReleased || ch.State != protocol.StateAutonomous ||
		ch.Reason != protocol.RevokeReleased || ch.LeaseID != lease.LeaseID || ch.Help != nil {
		t.Fatalf("lease released: %+v", ch)
	}

	// An opaque domain channel broadcast (topic channel:edge_report).
	send(t, robot, protocol.TypeChannelPublish, protocol.ChannelPublish{
		Channel: "edge_report", Broadcast: true, Data: json.RawMessage(`{"edge":"e12","seconds":41.5}`),
	})
	msg, i := f.waitFor(t, i, "channel message", isKind("channel"))
	if msg.channel.From != robotID || msg.channel.Channel != "edge_report" || string(msg.channel.Data) != `{"edge":"e12","seconds":41.5}` {
		t.Fatalf("channel message: %+v", msg.channel)
	}

	// Telemetry (topic telemetry).
	send(t, robot, protocol.TypeTelemetry, protocol.Telemetry{Battery: &protocol.Battery{Pct: f64(86)}})
	if tel, _ := f.waitFor(t, i, "telemetry after the reconnect", isKind("telemetry")); *tel.tel.Battery.Pct != 86 {
		t.Fatalf("telemetry after the reconnect: %+v", tel.tel)
	}

	// The robot goes away (topic presence).
	robot.Close()
	if off, _ := f.waitFor(t, i, "robot offline", isKind("presence")); off.robotID != robotID || off.online {
		t.Fatalf("presence: %+v", off)
	}

	if opens := f.checkSnapshotFirst(t); opens != 2 {
		t.Fatalf("the brain opened %d connections, want 2", opens)
	}
	if n := len(srv.Clients(t)); n != 3 {
		t.Fatalf("server has %d clients, want 3 (robot, operator, brain)", n)
	}
}

// A robot streams telemetry the whole time while the subscriber's connection
// is cut again and again. Whatever the timing of each reconnect, a snapshot is
// delivered before any event of the new connection, and events keep flowing
// afterwards without the test subscribing again.
func TestSnapshotComesFirstAfterEveryReconnect(t *testing.T) {
	srv := fleettest.Start(t)
	proxy := srv.Proxy(t)
	robot := mustConnect(t, fleet.Config{URL: srv.WSURL, Kind: fleet.Robot, EnrollKey: srv.EnrollKey})

	f := newFeed()
	brain := mustConnect(t, fleet.Config{URL: proxy.WSURL, Kind: fleet.Service, EnrollKey: srv.EnrollKey, OnState: f.state})
	f.attach(brain)
	subscribe(t, brain, fleet.TopicPresence, fleet.TopicTelemetry)

	// Under the server's default 50 telemetry messages a second per connection.
	stop := make(chan struct{})
	flooded := make(chan struct{})
	go func() {
		defer close(flooded)
		tick := time.NewTicker(25 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				robot.Send(context.Background(), protocol.TypeTelemetry, protocol.Telemetry{Battery: &protocol.Battery{Pct: f64(50)}})
			}
		}
	}()
	defer func() {
		close(stop)
		<-flooded
	}()

	const cuts = 5
	_, i := f.waitFor(t, 0, "telemetry", isKind("telemetry"))
	for n := 1; n <= cuts; n++ {
		proxy.Drop()
		_, i = f.waitFor(t, i, fmt.Sprintf("open after cut %d", n), isOpen)
		_, i = f.waitFor(t, i, fmt.Sprintf("snapshot after cut %d", n), isKind("snapshot"))
		_, i = f.waitFor(t, i, fmt.Sprintf("telemetry after cut %d", n), isKind("telemetry"))
	}
	if opens := f.checkSnapshotFirst(t); opens != cuts+1 {
		t.Fatalf("opened %d connections, want %d", opens, cuts+1)
	}
	snapshots := 0
	for _, r := range f.list() {
		if r.kind == "snapshot" {
			snapshots++
		}
	}
	if snapshots != cuts+1 {
		t.Fatalf("%d snapshots delivered, want one per connection (%d)", snapshots, cuts+1)
	}
}

// Help details reach the callbacks on every path the server sends them: the
// robot.help_requested event, the snapshot entry, and a lease revocation that
// puts the robot back in the queue with its original request time.
func TestHelpDetailsOnEventSnapshotAndRevocation(t *testing.T) {
	srv := fleettest.Start(t)
	robot := mustConnect(t, fleet.Config{URL: srv.WSURL, Kind: fleet.Robot, EnrollKey: srv.EnrollKey})
	robotID := robot.ClientID()
	operator := mustConnect(t, fleet.Config{URL: srv.WSURL, Kind: fleet.Operator, EnrollKey: srv.OperatorInvite(t)})

	f := newFeed()
	brain := mustConnect(t, fleet.Config{URL: srv.WSURL, Kind: fleet.Service, EnrollKey: srv.EnrollKey})
	f.attach(brain)
	subscribe(t, brain, fleet.TopicEvents)

	send(t, robot, protocol.TypeHelpRequest, protocol.HelpRequest{Reason: "stuck", Context: map[string]any{"leg": "e12"}})
	asked, i := f.waitFor(t, 0, "help", isKind("help"))
	if asked.robotID != robotID || asked.help.Reason != "stuck" || asked.help.Context["leg"] != "e12" {
		t.Fatalf("help event: %+v", asked)
	}

	// A second subscriber's snapshot carries the same queue entry.
	late := mustConnect(t, fleet.Config{URL: srv.WSURL, Kind: fleet.Service, EnrollKey: srv.EnrollKey})
	snap := subscribe(t, late, fleet.TopicEvents)
	if len(snap.Robots) != 1 || snap.Robots[0].State != protocol.StateHelpRequested || snap.Robots[0].Help == nil {
		t.Fatalf("snapshot: %+v", snap)
	}
	if h := *snap.Robots[0].Help; h.Reason != "stuck" || h.Context["leg"] != "e12" || h.RequestedAtMs != asked.help.RequestedAtMs {
		t.Fatalf("snapshot help = %+v, want the entry from the event %+v", h, asked.help)
	}

	// The operator claims and then vanishes: the server revokes the lease and
	// puts the robot back in the queue.
	send(t, operator, protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotID})
	_, i = f.waitFor(t, i, "lease granted", isKind("lease"))
	operator.Close()
	revoked, i := f.waitFor(t, i+1, "lease revoked", isKind("lease"))
	if ch := revoked.lease; ch.Event != protocol.EventRobotLeaseRevoked || ch.Reason != protocol.RevokeOperatorLost ||
		ch.State != protocol.StateHelpRequested || ch.Help == nil || ch.Help.Reason != "stuck" {
		t.Fatalf("lease revoked: %+v", ch)
	}
	again, _ := f.waitFor(t, i+1, "help after the revocation", isKind("help"))
	if again.robotID != robotID || again.help.Reason != "stuck" || again.help.RequestedAtMs != asked.help.RequestedAtMs {
		t.Fatalf("requeued help = %+v, want the original entry %+v", again.help, asked.help)
	}
}

// Subscribe called during an outage waits for the next connection and returns
// its snapshot.
func TestSubscribeWhileDisconnected(t *testing.T) {
	srv := fleettest.Start(t)
	proxy := srv.Proxy(t)
	st := newStates()
	c := mustConnect(t, fleet.Config{URL: proxy.WSURL, Kind: fleet.Service, EnrollKey: srv.EnrollKey, OnState: st.record})
	snapshots := make(chan fleet.Snapshot, 4)
	c.OnSnapshot(func(s fleet.Snapshot) { snapshots <- s })

	proxy.SetDown(true)
	st.waitFor(t, 0, "reconnecting", isState(fleet.StateReconnecting))

	type result struct {
		snap fleet.Snapshot
		err  error
	}
	done := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		snap, err := c.Subscribe(ctx, fleet.TopicPresence)
		done <- result{snap, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("Subscribe returned during the outage: %+v", r)
	case <-time.After(150 * time.Millisecond):
	}

	// A context that ends gives up the wait, not the subscription.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Subscribe(ctx, fleet.TopicEvents); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Subscribe with an expiring ctx = %v, want deadline exceeded", err)
	}

	proxy.SetDown(false)
	select {
	case r := <-done:
		if r.err != nil || r.snap.Robots == nil {
			t.Fatalf("Subscribe = %+v, %v", r.snap, r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Subscribe did not return after the outage")
	}
	select {
	case <-snapshots:
	case <-time.After(5 * time.Second):
		t.Fatal("no OnSnapshot delivery")
	}
	// One subscribe carried both calls' topics: exactly one snapshot.
	select {
	case <-snapshots:
		t.Fatal("a second snapshot for one reconnect")
	case <-time.After(200 * time.Millisecond):
	}
}

// A subscribe the server refuses fails the call with the server's code, and
// its topics are forgotten so that reconnects do not send them again.
func TestRefusedSubscribeIsForgotten(t *testing.T) {
	srv := fleettest.Start(t)
	proxy := srv.Proxy(t)
	st := newStates()
	c := mustConnect(t, fleet.Config{URL: proxy.WSURL, Kind: fleet.Service, EnrollKey: srv.EnrollKey, OnState: st.record})
	snapshots := make(chan fleet.Snapshot, 4)
	c.OnSnapshot(func(s fleet.Snapshot) { snapshots <- s })
	subscribe(t, c, fleet.TopicPresence)
	<-snapshots

	// Each topic is valid, but together they exceed the server's 64 KiB
	// payload limit, which it answers with rate_limited and our id as ref.
	var tooMany []string
	for n := 0; n < 1000; n++ {
		tooMany = append(tooMany, fmt.Sprintf("channel:%071d", n))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Subscribe(ctx, tooMany...); !errors.Is(err, fleet.ErrRateLimited) {
		t.Fatalf("oversized Subscribe = %v, want rate_limited", err)
	}
	if c.State() != fleet.StateOpen {
		t.Fatalf("state after a refused subscribe = %s", c.State())
	}

	// The reconnect subscribes to what is left and gets its snapshot. Had the
	// refused topics been kept, it would be refused again and deliver nothing.
	before := len(st.list())
	proxy.Drop()
	st.waitFor(t, before, "open again", isState(fleet.StateOpen))
	select {
	case <-snapshots:
	case <-time.After(10 * time.Second):
		t.Fatal("no snapshot after the reconnect")
	}
}

func TestSubscribeArgumentsAndClose(t *testing.T) {
	srv := fleettest.Start(t)
	c := mustConnect(t, fleet.Config{URL: srv.WSURL, Kind: fleet.Service, EnrollKey: srv.EnrollKey})
	ctx := context.Background()

	if _, err := c.Subscribe(ctx); err == nil {
		t.Fatal("Subscribe with no topics succeeded")
	}
	for _, bad := range []string{"", "Presence", "channel:has space"} {
		if _, err := c.Subscribe(ctx, fleet.TopicPresence, bad); err == nil {
			t.Fatalf("Subscribe(%q) succeeded", bad)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Subscribe(cancelled, fleet.TopicPresence); !errors.Is(err, context.Canceled) {
		t.Fatalf("Subscribe with a cancelled ctx = %v", err)
	}

	// A removed handler stops receiving.
	kept, removed := make(chan struct{}, 2), make(chan struct{}, 2)
	c.OnSnapshot(func(fleet.Snapshot) { kept <- struct{}{} })
	remove := c.OnSnapshot(func(fleet.Snapshot) { removed <- struct{}{} })
	remove()
	subscribe(t, c, fleet.TopicPresence)
	select {
	case <-kept:
	case <-time.After(5 * time.Second):
		t.Fatal("no OnSnapshot delivery")
	}
	if len(removed) != 0 {
		t.Fatal("removed handler was called")
	}

	c.Close()
	waitDone(t, c)
	if _, err := c.Subscribe(ctx, fleet.TopicPresence); !errors.Is(err, fleet.ErrClosed) {
		t.Fatalf("Subscribe after Close = %v, want ErrClosed", err)
	}
}

// Operator presence, typed: who is at a console arrives as the operator's
// entry, online and then offline, and the snapshot lists the same entry.
func TestOperatorPresenceCallbacks(t *testing.T) {
	srv := fleettest.Start(t)
	brain := mustConnect(t, fleet.Config{URL: srv.WSURL, Kind: fleet.Service, EnrollKey: srv.EnrollKey})
	seen := make(chan fleet.OperatorSummary, 8)
	brain.OnOperatorPresence(func(op fleet.OperatorSummary) { seen <- op })
	var names []string
	brain.OnEvent(func(ev fleet.Event) { names = append(names, ev.Event+" "+ev.OperatorID+ev.RobotID) })
	if snap := subscribe(t, brain, fleet.TopicPresence); len(snap.Operators) != 0 {
		t.Fatalf("operators before anyone enrolled: %+v", snap.Operators)
	}

	next := func(what string) fleet.OperatorSummary {
		t.Helper()
		select {
		case op := <-seen:
			return op
		case <-time.After(10 * time.Second):
			t.Fatalf("no operator presence callback for %s", what)
			return fleet.OperatorSummary{}
		}
	}

	operator := mustConnect(t, fleet.Config{URL: srv.WSURL, Kind: fleet.Operator, Name: "ada", EnrollKey: srv.OperatorInvite(t)})
	want := fleet.OperatorSummary{OperatorID: operator.ClientID(), Name: "ada", Online: true}
	if op := next("operator.online"); op != want {
		t.Fatalf("online: got %+v, want %+v", op, want)
	}
	if snap := subscribe(t, brain, fleet.TopicPresence); len(snap.Operators) != 1 || snap.Operators[0] != want {
		t.Fatalf("snapshot operators: %+v, want [%+v]", snap.Operators, want)
	}

	operator.Close()
	want.Online = false
	if op := next("operator.offline"); op != want {
		t.Fatalf("offline: got %+v, want %+v", op, want)
	}
	// Handlers run one at a time, so a handler is a safe place to read names.
	done := make(chan []string, 1)
	brain.OnSnapshot(func(fleet.Snapshot) { done <- slices.Clone(names) })
	subscribe(t, brain, fleet.TopicPresence)
	id := operator.ClientID()
	if got := <-done; !slices.Equal(got, []string{"operator.online " + id, "operator.offline " + id}) {
		t.Fatalf("OnEvent saw %q", got)
	}
}
