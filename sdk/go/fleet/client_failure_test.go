package fleet_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"fleetplatform/sdk/go/fleet"
	"fleetplatform/sdk/go/internal/fleettest"
)

// Terminal vs retryable failures (docs/INTEGRATION.md 2.2, 2.3, 2.8).

func TestBadTokenIsTerminal(t *testing.T) {
	srv := fleettest.Start(t)
	store := &fleet.MemoryTokenStore{}
	store.Save(fleet.Credentials{Token: "fp-tk-not-a-real-token", ClientID: "s_x", FleetID: "f_x"})
	st := newStates()

	c, err := connect(t, fleet.Config{URL: srv.WSURL, Kind: fleet.Service, TokenStore: store, OnState: st.record})
	if c != nil || !errors.Is(err, fleet.ErrAuthFailed) {
		t.Fatalf("Connect = %v, %v; want auth_failed", c, err)
	}
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Retryable() {
		t.Fatalf("err = %#v, want a terminal *fleet.Error", err)
	}
	// One attempt, no retry, and the bad token was not swapped for a new identity.
	for _, s := range st.names() {
		if s == fleet.StateReconnecting || s == fleet.StateEnrolling {
			t.Fatalf("states = %v", st.names())
		}
	}
	if n := len(srv.Clients(t)); n != 0 {
		t.Fatalf("server has %d clients, want 0", n)
	}
}

func TestBadEnrollKeyIsTerminal(t *testing.T) {
	srv := fleettest.Start(t)
	_, err := connect(t, fleet.Config{URL: srv.WSURL, Kind: fleet.Robot, EnrollKey: "not-the-enrollment-key-0123456789"})
	if !errors.Is(err, fleet.ErrAuthFailed) {
		t.Fatalf("Connect = %v, want auth_failed", err)
	}
}

func TestNoTokenAndNoEnrollKey(t *testing.T) {
	// Nothing listens here: the failure must come before any dial.
	_, err := connect(t, fleet.Config{URL: "ws://127.0.0.1:1/ws", Kind: fleet.Service})
	if !errors.Is(err, fleet.ErrAuthFailed) {
		t.Fatalf("Connect = %v, want auth_failed", err)
	}
}

func TestConfigIsValidated(t *testing.T) {
	for name, cfg := range map[string]fleet.Config{
		"no url":      {Kind: fleet.Service},
		"bad scheme":  {URL: "ftp://example.org/ws", Kind: fleet.Service},
		"no kind":     {URL: "ws://127.0.0.1:1/ws"},
		"both stores": {URL: "ws://127.0.0.1:1/ws", Kind: fleet.Service, TokenFile: "x", TokenStore: &fleet.MemoryTokenStore{}},
	} {
		if c, err := fleet.Connect(context.Background(), cfg); err == nil {
			c.Close()
			t.Errorf("%s: Connect succeeded", name)
		}
	}
}

func TestOperatorInviteIsSingleUse(t *testing.T) {
	srv := fleettest.Start(t)
	key := srv.OperatorInvite(t)

	op := mustConnect(t, fleet.Config{URL: srv.WSURL, Kind: fleet.Operator, Name: "alice", EnrollKey: key})
	if w, _ := op.Welcome(); w.Kind != "operator" || w.ClientID[:2] != "o_" {
		t.Fatalf("welcome = %+v", w)
	}

	// A second redemption is refused, and that is terminal, not retried.
	_, err := connect(t, fleet.Config{URL: srv.WSURL, Kind: fleet.Operator, EnrollKey: key})
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Retryable() {
		t.Fatalf("second redemption = %v, want a terminal *fleet.Error", err)
	}
}

func TestRevokedWhileConnectedStopsForGood(t *testing.T) {
	srv := fleettest.Start(t)
	proxy := srv.Proxy(t)
	st := newStates()
	c := mustConnect(t, fleet.Config{URL: proxy.WSURL, Kind: fleet.Robot, EnrollKey: srv.EnrollKey, OnState: st.record})

	if !srv.RevokeClient(t, c.ClientID()) {
		t.Fatal("revoke did not disconnect the client")
	}
	waitDone(t, c)
	if !errors.Is(c.Err(), fleet.ErrAuthFailed) || c.State() != fleet.StateClosed {
		t.Fatalf("after revoke: state %s, err %v", c.State(), c.Err())
	}
	closed, _ := st.waitFor(t, 0, "closed", isState(fleet.StateClosed))
	if !errors.Is(closed.Err, fleet.ErrAuthFailed) {
		t.Fatalf("closed change = %+v", closed)
	}
	// No reconnect loop.
	time.Sleep(300 * time.Millisecond)
	for _, s := range st.names() {
		if s == fleet.StateReconnecting {
			t.Fatalf("states = %v", st.names())
		}
	}
	if n := proxy.Connections(); n != 0 {
		t.Fatalf("%d connections still open after revoke", n)
	}
}

// One live connection per identity: the newer hello wins, and the older client
// must not reconnect (that would kick the newer one back, forever).
func TestTakeoverIsTerminalForTheOlderConnection(t *testing.T) {
	srv := fleettest.Start(t)
	tokenFile := filepath.Join(t.TempDir(), "token.json")
	cfg := fleet.Config{URL: srv.WSURL, Kind: fleet.Service, EnrollKey: srv.EnrollKey, TokenFile: tokenFile}

	older := mustConnect(t, cfg)
	newer := mustConnect(t, cfg)
	if newer.ClientID() != older.ClientID() {
		t.Fatalf("ids differ: %q vs %q", newer.ClientID(), older.ClientID())
	}
	waitDone(t, older)
	if !errors.Is(older.Err(), fleet.ErrConflict) {
		t.Fatalf("older client's err = %v, want conflict", older.Err())
	}
	time.Sleep(300 * time.Millisecond)
	if newer.State() != fleet.StateOpen {
		t.Fatalf("newer client is %s", newer.State())
	}
}

// A heartbeat lapse is the server's rate_limited close. It is retryable.
func TestHeartbeatLapseReconnects(t *testing.T) {
	srv := fleettest.Start(t)
	proxy := srv.Proxy(t)
	st := newStates()
	c := mustConnect(t, fleet.Config{URL: proxy.WSURL, Kind: fleet.Robot, EnrollKey: srv.EnrollKey, OnState: st.record})
	id := c.ClientID()

	// The server keeps reaching the client but stops hearing from it.
	proxy.StallUpstream()
	lost, i := st.waitFor(t, 0, "reconnecting", isState(fleet.StateReconnecting))
	if !errors.Is(lost.Err, fleet.ErrRateLimited) {
		t.Fatalf("reconnecting after a lapse with %v, want rate_limited", lost.Err)
	}
	again, _ := st.waitFor(t, i, "open again", isState(fleet.StateOpen))
	if again.Welcome.ClientID != id {
		t.Fatalf("reconnected as %q, want %q", again.Welcome.ClientID, id)
	}
}

func TestOutageBacksOffThenRecoversAsTheSameClient(t *testing.T) {
	srv := fleettest.Start(t)
	proxy := srv.Proxy(t)
	st := newStates()
	c := mustConnect(t, fleet.Config{URL: proxy.WSURL, Kind: fleet.Service, EnrollKey: srv.EnrollKey, OnState: st.record})
	id := c.ClientID()

	proxy.SetDown(true)
	third, i := st.waitFor(t, 0, "a third failed attempt", func(ch fleet.StateChange) bool {
		return ch.State == fleet.StateReconnecting && ch.Attempt == 3
	})
	if !errors.Is(third.Err, fleet.ErrNetwork) {
		t.Fatalf("third attempt failed with %v, want network", third.Err)
	}
	// Delays grow from Initial and are jittered into [d/2, d].
	for _, ch := range st.list() {
		if ch.State != fleet.StateReconnecting {
			continue
		}
		d := fastBackoff.Initial << (ch.Attempt - 1)
		if d > fastBackoff.Max {
			d = fastBackoff.Max
		}
		if ch.RetryIn < d/2 || ch.RetryIn > d {
			t.Fatalf("attempt %d waits %v, want within [%v, %v]", ch.Attempt, ch.RetryIn, d/2, d)
		}
	}
	if err := c.Send(context.Background(), "heartbeat", nil); !errors.Is(err, fleet.ErrClosed) {
		t.Fatalf("Send during the outage = %v, want ErrClosed", err)
	}

	proxy.SetDown(false)
	again, _ := st.waitFor(t, i, "open again", isState(fleet.StateOpen))
	if again.Welcome.ClientID != id {
		t.Fatalf("reconnected as %q, want %q", again.Welcome.ClientID, id)
	}
	if n := len(srv.Clients(t)); n != 1 {
		t.Fatalf("server has %d clients after the outage, want 1", n)
	}
}

func TestNoReconnectMakesADropTerminal(t *testing.T) {
	srv := fleettest.Start(t)
	proxy := srv.Proxy(t)
	c := mustConnect(t, fleet.Config{URL: proxy.WSURL, Kind: fleet.Service, EnrollKey: srv.EnrollKey, NoReconnect: true})
	proxy.Drop()
	waitDone(t, c)
	if !errors.Is(c.Err(), fleet.ErrNetwork) {
		t.Fatalf("err = %v, want network", c.Err())
	}
}

// While the server is unreachable Connect keeps retrying; ctx is what bounds it.
func TestConnectHonoursItsContext(t *testing.T) {
	srv := fleettest.Start(t)
	proxy := srv.Proxy(t)
	proxy.SetDown(true)
	st := newStates()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	c, err := fleet.Connect(ctx, fleet.Config{
		URL: proxy.WSURL, Kind: fleet.Service, EnrollKey: srv.EnrollKey, Backoff: fastBackoff, OnState: st.record,
	})
	if c != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Connect = %v, %v; want deadline exceeded", c, err)
	}
	names := st.names()
	if len(names) < 3 || names[len(names)-1] != fleet.StateClosed {
		t.Fatalf("states = %v, want retries then closed", names)
	}
}

type failingStore struct{ saveErr error }

func (failingStore) Load() (*fleet.Credentials, error) { return nil, nil }
func (s failingStore) Save(fleet.Credentials) error    { return s.saveErr }

// A token that cannot be saved must not be retried: every retry would enroll
// again and mint one more identity.
func TestUnsavableTokenIsTerminalAfterOneEnrollment(t *testing.T) {
	srv := fleettest.Start(t)
	_, err := connect(t, fleet.Config{
		URL: srv.WSURL, Kind: fleet.Service, EnrollKey: srv.EnrollKey,
		TokenStore: failingStore{saveErr: errors.New("disk full")},
	})
	var fe *fleet.Error
	if !errors.As(err, &fe) || fe.Code != fleet.CodeTokenStore || fe.Retryable() {
		t.Fatalf("Connect = %v, want a terminal token_store error", err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(srv.Clients(t)); n != 1 {
		t.Fatalf("server has %d clients, want exactly the 1 enrollment", n)
	}
}
