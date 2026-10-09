package fleet_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"fleetplatform/sdk/go/fleet"
	"fleetplatform/sdk/go/internal/fleettest"
	"fleetplatform/sdk/go/protocol"
)

// fastResend is the shortest re-send interval the convention allows.
const fastResend = 250 * time.Millisecond

// copyOf is one acked message as a receiver without the SDK's receiving half
// sees it on the wire.
type copyOf struct {
	from  string
	seq   int64
	inner string
}

// wire records every channel.message one client receives, raw, and can be
// told to answer acked messages by hand. It stands in for a receiver written
// against the convention in another language.
type wire struct {
	mu   sync.Mutex
	all  []protocol.ChannelMessage
	wake chan struct{}
}

func tap(c *fleet.Client) *wire {
	w := &wire{wake: make(chan struct{}, 1)}
	c.On(protocol.TypeChannelMessage, func(env protocol.Envelope) {
		var msg protocol.ChannelMessage
		json.Unmarshal(env.Payload, &msg)
		w.mu.Lock()
		w.all = append(w.all, msg)
		w.mu.Unlock()
		select {
		case w.wake <- struct{}{}:
		default:
		}
	})
	return w
}

// copies are the acked messages received so far, in order.
func (w *wire) copies() []copyOf {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []copyOf
	for _, m := range w.all {
		var d struct {
			Seq  int64           `json:"seq"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(m.Data, &d) == nil && d.Seq > 0 {
			out = append(out, copyOf{m.From, d.Seq, string(d.Data)})
		}
	}
	return out
}

// acks are the seqs of the acks received so far, in order.
func (w *wire) acks() []int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []int64
	for _, m := range w.all {
		var d struct {
			Ack int64 `json:"ack"`
		}
		if json.Unmarshal(m.Data, &d) == nil && d.Ack > 0 {
			out = append(out, d.Ack)
		}
	}
	return out
}

func (w *wire) waitCopies(t *testing.T, n int) []copyOf {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if got := w.copies(); len(got) >= n {
			return got
		}
		select {
		case <-w.wake:
		case <-deadline:
			t.Fatalf("timed out waiting for %d copies; got %v", n, w.copies())
		}
	}
}

func (w *wire) waitAcks(t *testing.T, n int) []int64 {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if got := w.acks(); len(got) >= n {
			return got
		}
		select {
		case <-w.wake:
		case <-deadline:
			t.Fatalf("timed out waiting for %d acks; got %v", n, w.acks())
		}
	}
}

// ackByHand makes c answer acked messages on one channel itself, from the
// skip-th copy on (0 acks every copy).
func ackByHand(c *fleet.Client, channel string, skip int) {
	seen := 0
	c.Channel(channel).OnMessage(func(from string, data json.RawMessage) {
		var d struct {
			Seq int64 `json:"seq"`
		}
		if json.Unmarshal(data, &d) != nil || d.Seq == 0 {
			return
		}
		seen++
		if seen > skip {
			c.Channel(channel).Publish(context.Background(), from, map[string]int64{"ack": d.Seq})
		}
	})
}

func pair(t *testing.T, srv *fleettest.Server, brainURL string, resend time.Duration) (brain, robot *fleet.Client) {
	t.Helper()
	robot = mustConnect(t, fleet.Config{URL: srv.WSURL, Kind: fleet.Robot, Name: "sim-01", EnrollKey: srv.EnrollKey})
	brain = mustConnect(t, fleet.Config{
		URL: brainURL, Kind: fleet.Service, Name: "command-brain", EnrollKey: srv.EnrollKey, AckedResend: resend,
	})
	return brain, robot
}

type job struct {
	Order string `json:"order"`
}

// Ack on the first try, between the two Go halves: the robot's application
// gets the inner data once, and one copy is all that went over the wire.
func TestSendAckedFirstTry(t *testing.T) {
	srv := fleettest.Start(t)
	brain, robot := pair(t, srv, srv.WSURL, fastResend)
	onWire := tap(robot)

	type got struct{ from, data string }
	handled := make(chan got, 4)
	robot.Channel("assignment").OnAcked(func(from string, data json.RawMessage) error {
		handled <- got{from, string(data)}
		return nil
	})
	// An acked message is not ordinary data on a channel with an acked handler.
	robot.Channel("assignment").OnMessage(func(from string, data json.RawMessage) {
		t.Errorf("OnMessage got %s", data)
	})

	if err := brain.Channel("assignment").SendAcked(context.Background(), robot.ClientID(), job{"o-1"}, 5*time.Second); err != nil {
		t.Fatalf("SendAcked: %v", err)
	}
	if g := <-handled; g.from != brain.ClientID() || g.data != `{"order":"o-1"}` {
		t.Fatalf("handler got %+v", g)
	}
	if copies := onWire.copies(); len(copies) != 1 {
		t.Fatalf("copies on the wire: %v, want one", copies)
	}

	// Seqs are never reused: a second send is a new message.
	if err := brain.Channel("assignment").SendAcked(context.Background(), robot.ClientID(), job{"o-2"}, 5*time.Second); err != nil {
		t.Fatalf("second SendAcked: %v", err)
	}
	copies := onWire.copies()
	if len(copies) != 2 || copies[1].seq <= copies[0].seq || copies[0].seq < time.Now().Add(-time.Hour).UnixMilli() {
		t.Fatalf("copies: %v", copies)
	}
	if len(handled) != 1 {
		t.Fatalf("handler calls after two sends: %d more than expected", len(handled)-1)
	}
}

// The first copy gets no answer (as if it was dropped); the re-send is acked.
// The receiver here answers by hand, so this is the sender against the
// convention, not against this SDK's own receiving half.
func TestSendAckedAfterADroppedSend(t *testing.T) {
	srv := fleettest.Start(t)
	brain, robot := pair(t, srv, srv.WSURL, fastResend)
	onWire := tap(robot)
	ackByHand(robot, "assignment", 1)

	start := time.Now()
	if err := brain.Channel("assignment").SendAcked(context.Background(), robot.ClientID(), job{"o-1"}, 5*time.Second); err != nil {
		t.Fatalf("SendAcked: %v", err)
	}
	if took := time.Since(start); took < fastResend {
		t.Fatalf("acked after %v, before the re-send interval", took)
	}
	copies := onWire.copies()
	if len(copies) != 2 || copies[0] != copies[1] || copies[0].inner != `{"order":"o-1"}` {
		t.Fatalf("copies on the wire: %v, want the same message twice", copies)
	}
	// Resolved means it stops re-sending.
	time.Sleep(2 * fastResend)
	if n := len(onWire.copies()); n != 2 {
		t.Fatalf("%d copies after the ack, want 2", n)
	}
}

// The server reports the target offline: ErrNotFound at once, not after the
// deadline.
func TestSendAckedTargetOffline(t *testing.T) {
	srv := fleettest.Start(t)
	brain, robot := pair(t, srv, srv.WSURL, fastResend)
	robotID := robot.ClientID()
	robot.Close()
	waitDone(t, robot)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		snap := subscribe(t, brain, fleet.TopicPresence)
		if len(snap.Robots) == 1 && snap.Robots[0].Presence != "online" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("robot never went offline: %+v", snap)
		}
	}

	start := time.Now()
	err := brain.Channel("assignment").SendAcked(context.Background(), robotID, job{"o-1"}, 30*time.Second)
	if !errors.Is(err, fleet.ErrNotFound) {
		t.Fatalf("SendAcked to an offline robot = %v, want ErrNotFound", err)
	}
	if !strings.Contains(err.Error(), "not delivered") {
		t.Fatalf("a not_found on the first copy should say it was not delivered: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("ErrNotFound took %v; it must not wait out the deadline", took)
	}
	// A client id that never existed is the same answer.
	if err := brain.Channel("assignment").SendAcked(context.Background(), "r_nobody", job{"o-1"}, 30*time.Second); !errors.Is(err, fleet.ErrNotFound) {
		t.Fatalf("SendAcked to an unknown id = %v, want ErrNotFound", err)
	}
}

// The robot is connected and receives every copy but never acks: the same
// message is re-sent until the deadline, then ErrTimeout, then silence.
func TestSendAckedTimesOut(t *testing.T) {
	srv := fleettest.Start(t)
	brain, robot := pair(t, srv, srv.WSURL, fastResend)
	onWire := tap(robot)

	const timeout = 900 * time.Millisecond
	start := time.Now()
	err := brain.Channel("assignment").SendAcked(context.Background(), robot.ClientID(), job{"o-1"}, timeout)
	if !errors.Is(err, fleet.ErrTimeout) {
		t.Fatalf("SendAcked nobody acks = %v, want ErrTimeout", err)
	}
	if took := time.Since(start); took < timeout {
		t.Fatalf("timed out after %v, before the %v deadline", took, timeout)
	}
	copies := onWire.waitCopies(t, 2)
	for _, c := range copies {
		if c != copies[0] {
			t.Fatalf("re-sends differ: %v", copies)
		}
	}
	n := len(onWire.copies())
	time.Sleep(2 * fastResend)
	if after := len(onWire.copies()); after != n {
		t.Fatalf("still re-sending after the timeout: %d then %d copies", n, after)
	}

	// A context that ends first wins, and also stops the re-sends.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := brain.Channel("assignment").SendAcked(ctx, robot.ClientID(), job{"o-2"}, 5*time.Second); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SendAcked with a short ctx = %v, want context.DeadlineExceeded", err)
	}
}

// The sender's own connection is cut while a send is pending. The re-send
// interval is the longest allowed, so the copy that gets acked can only be the
// one sent when the client came back: same seq, deadline still running.
func TestSendAckedResumesAfterReconnect(t *testing.T) {
	srv := fleettest.Start(t)
	proxy := srv.Proxy(t)
	brain, robot := pair(t, srv, proxy.WSURL, 5*time.Second)
	onWire := tap(robot)
	ackByHand(robot, "assignment", 1)

	result := make(chan error, 1)
	go func() {
		result <- brain.Channel("assignment").SendAcked(context.Background(), robot.ClientID(), job{"o-1"}, 30*time.Second)
	}()
	onWire.waitCopies(t, 1)
	proxy.SetDown(true)
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-result:
		t.Fatalf("SendAcked returned %v during the outage", err)
	default:
	}
	proxy.SetDown(false)

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("SendAcked across a reconnect: %v", err)
		}
	case <-time.After(4 * time.Second): // well inside the 5s re-send interval
		t.Fatalf("no re-send when the client reconnected; copies: %v", onWire.copies())
	}
	if copies := onWire.copies(); len(copies) != 2 || copies[0] != copies[1] {
		t.Fatalf("copies: %v, want the same message before and after the reconnect", copies)
	}

	// Started while disconnected: it waits for the connection, then goes out.
	proxy.SetDown(true)
	for brain.State() == fleet.StateOpen {
		time.Sleep(10 * time.Millisecond)
	}
	go func() {
		result <- brain.Channel("assignment").SendAcked(context.Background(), robot.ClientID(), job{"o-2"}, 30*time.Second)
	}()
	time.Sleep(100 * time.Millisecond)
	proxy.SetDown(false)
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("SendAcked started while disconnected: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SendAcked started while disconnected never finished")
	}
}

// SendAcked called from inside a handler: the delivery goroutine is blocked in
// it, and the ack still gets through.
func TestSendAckedFromAHandler(t *testing.T) {
	srv := fleettest.Start(t)
	brain, robot := pair(t, srv, srv.WSURL, 5*time.Second)
	robot.Channel("assignment").OnAcked(func(string, json.RawMessage) error { return nil })

	result := make(chan error, 1)
	brain.Channel("status").OnMessage(func(from string, data json.RawMessage) {
		result <- brain.Channel("assignment").SendAcked(context.Background(), from, job{"o-1"}, 3*time.Second)
	})
	if err := robot.Channel("status").Publish(context.Background(), brain.ClientID(), "idle"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("SendAcked inside a handler: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SendAcked inside a handler never returned")
	}
}

// The Go receiving half: a handler that fails gets the re-send as new; once it
// accepts, later copies are acked again without calling it again.
func TestOnAckedDedupsAndAcksEveryCopy(t *testing.T) {
	srv := fleettest.Start(t)
	brain, robot := pair(t, srv, srv.WSURL, fastResend)
	atRobot, atBrain := tap(robot), tap(brain)

	calls := 0
	robot.Channel("assignment").OnAcked(func(from string, data json.RawMessage) error {
		calls++
		if calls == 1 {
			return errors.New("not ready")
		}
		return nil
	})
	assign := brain.Channel("assignment")
	if err := assign.SendAcked(context.Background(), robot.ClientID(), job{"o-1"}, 5*time.Second); err != nil {
		t.Fatalf("SendAcked: %v", err)
	}
	copies := atRobot.copies()
	if len(copies) != 2 || len(atBrain.waitAcks(t, 1)) != 1 {
		t.Fatalf("copies %v, acks %v: want the failed copy unanswered and the re-send acked", copies, atBrain.acks())
	}

	// The same message twice more, as late re-sends would arrive.
	again := json.RawMessage(fmt.Sprintf(`{"seq":%d,"data":{"order":"o-1"}}`, copies[0].seq))
	for range 2 {
		if err := assign.Publish(context.Background(), robot.ClientID(), again); err != nil {
			t.Fatal(err)
		}
	}
	if acks := atBrain.waitAcks(t, 3); acks[1] != copies[0].seq || acks[2] != copies[0].seq {
		t.Fatalf("acks: %v", acks)
	}
	// Handlers of one client run one at a time, so reading calls from a
	// handler on the same client is race-free.
	done := make(chan int, 1)
	robot.Channel("probe").OnMessage(func(string, json.RawMessage) { done <- calls })
	if err := brain.Channel("probe").Publish(context.Background(), robot.ClientID(), 1); err != nil {
		t.Fatal(err)
	}
	if n := <-done; n != 2 {
		t.Fatalf("acked handler ran %d times, want 2 (one failure, one acceptance)", n)
	}
}

// OnMessage is enough to receive broadcasts: it subscribes to the channel's
// topic, and the subscription is renewed after a reconnect.
func TestChannelBroadcastDirectedAndResubscribe(t *testing.T) {
	srv := fleettest.Start(t)
	proxy := srv.Proxy(t)
	brain, robot := pair(t, srv, proxy.WSURL, fastResend)

	type got struct{ from, data string }
	msgs := make(chan got, 256)
	brain.Channel("edge_report").OnMessage(func(from string, data json.RawMessage) { msgs <- got{from, string(data)} })

	// The subscribe is sent without waiting for it, so repeat the broadcast
	// until one arrives.
	broadcastUntilHeard := func(what string) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			robot.Channel("edge_report").Broadcast(context.Background(), map[string]string{"edge": what})
			select {
			case m := <-msgs:
				if m.from != robot.ClientID() {
					t.Fatalf("from %q, want the robot", m.from)
				}
				if m.data == `{"edge":"`+what+`"}` {
					return
				}
			case <-time.After(50 * time.Millisecond):
			case <-deadline:
				t.Fatalf("broadcast %q never arrived", what)
			}
		}
	}
	broadcastUntilHeard("e1")

	proxy.Drop()
	for brain.State() == fleet.StateOpen {
		time.Sleep(5 * time.Millisecond)
	}
	broadcastUntilHeard("e2")

	// Directed, in both directions, with no subscription on the robot's side.
	heard := make(chan got, 4)
	robot.Channel("note").OnMessage(func(from string, data json.RawMessage) { heard <- got{from, string(data)} })
	if err := brain.Channel("note").Publish(context.Background(), robot.ClientID(), nil); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-heard:
		if m.from != brain.ClientID() || m.data != "null" {
			t.Fatalf("directed message: %+v", m)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("directed message never arrived")
	}
}

func TestSendAckedArgumentsAndClose(t *testing.T) {
	srv := fleettest.Start(t)
	brain, robot := pair(t, srv, srv.WSURL, fastResend)
	assign := brain.Channel("assignment")
	ctx := context.Background()

	if err := assign.SendAcked(ctx, "", job{}, time.Second); err == nil {
		t.Fatal("SendAcked without a target succeeded")
	}
	if err := assign.SendAcked(ctx, robot.ClientID(), job{}, fleet.MaxAckedTimeout+time.Second); err == nil {
		t.Fatal("SendAcked accepted a deadline over five minutes")
	}
	if err := assign.SendAcked(ctx, robot.ClientID(), job{}, -time.Second); err == nil {
		t.Fatal("SendAcked accepted a negative deadline")
	}
	if err := assign.SendAcked(ctx, robot.ClientID(), make(chan int), time.Second); err == nil {
		t.Fatal("SendAcked accepted data that is not JSON")
	}
	if err := assign.Publish(ctx, "", job{}); err == nil {
		t.Fatal("Publish without a target succeeded")
	}
	for _, name := range []string{"", "Jobs", "-x", "a b", strings.Repeat("a", 65)} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("Channel(%q) did not panic", name)
				}
			}()
			brain.Channel(name)
		}()
	}

	// Closing the client ends a pending send with the client's error.
	result := make(chan error, 1)
	go func() { result <- assign.SendAcked(ctx, robot.ClientID(), job{}, 30*time.Second) }()
	time.Sleep(100 * time.Millisecond)
	brain.Close()
	select {
	case err := <-result:
		if !errors.Is(err, fleet.ErrClosed) {
			t.Fatalf("SendAcked on a closed client = %v, want ErrClosed", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SendAcked outlived its client")
	}
}
