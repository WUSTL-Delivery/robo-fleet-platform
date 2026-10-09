package fleet_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"fleetplatform/sdk/go/fleet"
	"fleetplatform/sdk/go/internal/fleettest"
	"fleetplatform/sdk/go/protocol"
)

func TestMain(m *testing.M) { fleettest.Main(m) }

// fastBackoff keeps reconnect tests quick.
var fastBackoff = fleet.Backoff{Initial: 20 * time.Millisecond, Max: 200 * time.Millisecond}

// states records every StateChange a client reports, from the first attempt on.
type states struct {
	mu   sync.Mutex
	all  []fleet.StateChange
	wake chan struct{}
}

func newStates() *states { return &states{wake: make(chan struct{}, 1)} }

func (s *states) record(ch fleet.StateChange) {
	s.mu.Lock()
	s.all = append(s.all, ch)
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *states) list() []fleet.StateChange {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.all)
}

func (s *states) names() []fleet.State {
	var out []fleet.State
	for _, ch := range s.list() {
		out = append(out, ch.State)
	}
	return out
}

// waitFor returns the first recorded change at index >= from that matches.
func (s *states) waitFor(t *testing.T, from int, what string, match func(fleet.StateChange) bool) (fleet.StateChange, int) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		for i, ch := range s.list() {
			if i >= from && match(ch) {
				return ch, i
			}
		}
		select {
		case <-s.wake:
		case <-deadline:
			t.Fatalf("timed out waiting for %s; states so far: %v", what, s.names())
		}
	}
}

func isState(want fleet.State) func(fleet.StateChange) bool {
	return func(ch fleet.StateChange) bool { return ch.State == want }
}

// connect connects with test defaults and closes the client when the test ends.
func connect(t *testing.T, cfg fleet.Config) (*fleet.Client, error) {
	t.Helper()
	if cfg.Backoff == (fleet.Backoff{}) {
		cfg.Backoff = fastBackoff
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := fleet.Connect(ctx, cfg)
	if c != nil {
		t.Cleanup(func() {
			c.Close()
			<-c.Done()
		})
	}
	return c, err
}

func mustConnect(t *testing.T, cfg fleet.Config) *fleet.Client {
	t.Helper()
	c, err := connect(t, cfg)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return c
}

func waitDone(t *testing.T, c *fleet.Client) {
	t.Helper()
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatalf("client did not close; state %s", c.State())
	}
}

// The ticket's done condition: enroll once, persist the token, heartbeat at the
// welcome interval, and after the connection is killed come back as the same
// client.
func TestEnrollsOnceHeartbeatsAndReconnectsAsTheSameClient(t *testing.T) {
	srv := fleettest.Start(t)
	proxy := srv.Proxy(t)
	tokenFile := filepath.Join(t.TempDir(), "state", "token.json")
	st := newStates()

	c := mustConnect(t, fleet.Config{
		URL:       proxy.WSURL,
		Kind:      fleet.Service,
		Name:      "go-client-test",
		EnrollKey: srv.EnrollKey,
		TokenFile: tokenFile,
		Agent:     &protocol.AgentInfo{Name: "sdk-go-test", Version: "0.0.0"},
		OnState:   st.record,
	})

	w, ok := c.Welcome()
	if !ok || w.Kind != "service" || w.ClientID == "" || w.ClientID[:2] != "s_" {
		t.Fatalf("welcome = %+v, ok %v", w, ok)
	}
	if got := time.Duration(w.HeartbeatIntervalMs) * time.Millisecond; got != fleettest.HeartbeatInterval {
		t.Fatalf("heartbeat interval = %v, want %v", got, fleettest.HeartbeatInterval)
	}
	id := c.ClientID()
	if id != w.ClientID || c.FleetID() != w.FleetID {
		t.Fatalf("ClientID/FleetID = %q/%q, welcome %+v", id, c.FleetID(), w)
	}

	// The token is on disk, private, in the format the other SDKs read.
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]string
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["token"] == "" || stored["client_id"] != id || stored["fleet_id"] != w.FleetID {
		t.Fatalf("token file = %s", raw)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(tokenFile)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("token file mode = %v, want 0600", info.Mode().Perm())
		}
	}

	// Outlast the server's ~2.5 missed intervals several times over: only
	// heartbeats keep the client online.
	time.Sleep(5 * fleettest.HeartbeatInterval)
	if got := st.names(); !slices.Equal(got, []fleet.State{fleet.StateEnrolling, fleet.StateConnecting, fleet.StateOpen}) {
		t.Fatalf("states after 5 heartbeat intervals = %v", got)
	}
	if c.State() != fleet.StateOpen {
		t.Fatalf("state = %s", c.State())
	}

	// Kill the connection out from under the client.
	proxy.Drop()
	lost, i := st.waitFor(t, 3, "reconnecting", isState(fleet.StateReconnecting))
	if !errors.Is(lost.Err, fleet.ErrNetwork) || lost.Attempt != 1 || lost.RetryIn <= 0 {
		t.Fatalf("reconnecting change = %+v", lost)
	}
	again, _ := st.waitFor(t, i, "open again", isState(fleet.StateOpen))
	if again.Welcome == nil || again.Welcome.ClientID != id {
		t.Fatalf("reconnected as %+v, want client_id %s", again.Welcome, id)
	}
	if c.ClientID() != id {
		t.Fatalf("ClientID after reconnect = %q, want %q", c.ClientID(), id)
	}
	if slices.Contains(st.names()[3:], fleet.StateEnrolling) {
		t.Fatalf("enrolled again on reconnect: %v", st.names())
	}

	// Exactly one identity was ever minted for this fleet.
	clients := srv.Clients(t)
	if len(clients) != 1 || clients[0].ID != id || clients[0].Name != "go-client-test" || clients[0].Kind != "service" {
		t.Fatalf("server's clients = %+v, want only %s", clients, id)
	}
}

func TestReusesTheTokenFileWithoutAnEnrollKey(t *testing.T) {
	srv := fleettest.Start(t)
	tokenFile := filepath.Join(t.TempDir(), "token.json")

	first := mustConnect(t, fleet.Config{URL: srv.WSURL, Kind: fleet.Robot, EnrollKey: srv.EnrollKey, TokenFile: tokenFile})
	id := first.ClientID()
	if id[:2] != "r_" {
		t.Fatalf("client id = %q", id)
	}
	first.Close()
	waitDone(t, first)
	if first.State() != fleet.StateClosed || !errors.Is(first.Err(), fleet.ErrClosed) {
		t.Fatalf("after Close: state %s, err %v", first.State(), first.Err())
	}

	// A later process run: no key at all, same identity.
	st := newStates()
	second := mustConnect(t, fleet.Config{URL: srv.WSURL, Kind: fleet.Robot, TokenFile: tokenFile, OnState: st.record})
	if second.ClientID() != id {
		t.Fatalf("second run is %q, want %q", second.ClientID(), id)
	}
	if slices.Contains(st.names(), fleet.StateEnrolling) {
		t.Fatalf("second run enrolled: %v", st.names())
	}
	if n := len(srv.Clients(t)); n != 1 {
		t.Fatalf("server has %d clients, want 1", n)
	}
}

func TestSendAndHandlers(t *testing.T) {
	srv := fleettest.Start(t)
	c := mustConnect(t, fleet.Config{URL: srv.WSURL, Kind: fleet.Service, EnrollKey: srv.EnrollKey})

	snapshots := make(chan protocol.Envelope, 4)
	every := make(chan string, 16)
	removeSnapshot := c.On(protocol.TypeSnapshot, func(env protocol.Envelope) { snapshots <- env })
	c.OnMessage(func(env protocol.Envelope) { every <- env.Type })

	ctx := context.Background()
	if err := c.Send(ctx, protocol.TypeSubscribe, protocol.Subscribe{Topics: []string{"presence"}}); err != nil {
		t.Fatal(err)
	}
	select {
	case env := <-snapshots:
		var snap protocol.Snapshot
		if err := json.Unmarshal(env.Payload, &snap); err != nil {
			t.Fatalf("snapshot payload: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no snapshot")
	}
	if got := <-every; got != protocol.TypeSnapshot {
		t.Fatalf("OnMessage saw %q first, want snapshot", got)
	}

	// A refused send comes back as an error envelope carrying our id as ref,
	// and does not close the connection.
	errs := make(chan protocol.ErrorMsg, 1)
	c.On(protocol.TypeError, func(env protocol.Envelope) {
		var msg protocol.ErrorMsg
		json.Unmarshal(env.Payload, &msg)
		errs <- msg
	})
	publish := protocol.ChannelPublish{Channel: "test", To: "r_nobody", Data: json.RawMessage(`{"n":1}`)}
	if err := c.Send(ctx, protocol.TypeChannelPublish, publish, fleet.WithID("pub-1")); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-errs:
		if msg.Code != protocol.ErrNotFound || msg.Ref != "pub-1" {
			t.Fatalf("error reply = %+v", msg)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no error reply")
	}

	// A removed handler stops receiving.
	removeSnapshot()
	if err := c.Send(ctx, protocol.TypeSubscribe, protocol.Subscribe{Topics: []string{"events"}}); err != nil {
		t.Fatal(err)
	}
	for got := ""; got != protocol.TypeSnapshot; got = <-every {
	}
	select {
	case <-snapshots:
		t.Fatal("removed handler was called")
	case <-time.After(100 * time.Millisecond):
	}
	if c.State() != fleet.StateOpen {
		t.Fatalf("state = %s", c.State())
	}

	c.Close()
	waitDone(t, c)
	if err := c.Send(ctx, protocol.TypeHeartbeat, nil); !errors.Is(err, fleet.ErrClosed) {
		t.Fatalf("Send after Close = %v, want ErrClosed", err)
	}
}

// A handler runs off the connection goroutine, so it may Send and Close.
func TestHandlersMaySendAndClose(t *testing.T) {
	srv := fleettest.Start(t)
	st := newStates()
	c := mustConnect(t, fleet.Config{URL: srv.WSURL, Kind: fleet.Service, EnrollKey: srv.EnrollKey, OnState: st.record})

	sendErr := make(chan error, 1)
	c.On(protocol.TypeSnapshot, func(protocol.Envelope) {
		sendErr <- c.Send(context.Background(), protocol.TypeHeartbeat, protocol.Heartbeat{})
		c.Close()
	})
	if err := c.Send(context.Background(), protocol.TypeSubscribe, protocol.Subscribe{Topics: []string{"presence"}}); err != nil {
		t.Fatal(err)
	}
	waitDone(t, c)
	if err := <-sendErr; err != nil {
		t.Fatalf("Send from a handler: %v", err)
	}
	all := st.list()
	last := all[len(all)-1]
	if last.State != fleet.StateClosed || !errors.Is(last.Err, fleet.ErrClosed) {
		t.Fatalf("last state change = %+v", last)
	}
}
