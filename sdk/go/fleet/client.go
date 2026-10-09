// Package fleet is the Go client for fleet-server: the one outbound websocket a
// Go service (or robot, or operator tool) keeps to the control server.
//
// This file is the connection core. It owns exactly the identity and liveness
// part of the wire protocol (docs/INTEGRATION.md sections 2.2 and 2.3):
//
//  1. Enroll ONCE: if the TokenStore is empty, open a one-shot socket, send
//     enroll.request {enrollment_key, kind, name} and persist the
//     enroll.response credentials.
//  2. Hello on EVERY connect: hello {token, agent} -> welcome.
//  3. Heartbeat every welcome.heartbeat_interval_ms while open.
//  4. Reconnect with exponential backoff and jitter using the SAME token. It
//     never enrolls again on reconnect (that would mint a new identity).
//
// Everything else is layered on this surface:
//
//	client.Send(ctx, type, payload, opts...)  one outbound envelope; fails unless open
//	client.On(type, handler)                  inbound envelopes of one type
//	client.OnMessage(handler)                 every inbound envelope
//	client.OnState(handler)                   connection-state changes; StateOpen carries
//	                                          the welcome and fires on EVERY (re)connect
//	client.State / Welcome / ClientID / FleetID / Done / Err
//
// # Reconnect policy
//
// Only transient failures retry: a network drop, a server restart, a handshake
// timeout, a heartbeat lapse (the server's rate_limited close). Any other
// refusal is terminal and closes the client: auth_failed (a bad or revoked
// token, including one revoked while connected; a bad enrollment key), conflict
// (another connection took over this identity, so reconnecting would just kick
// it back and ping-pong), and the rest. See Error.Retryable. The client never
// clears the TokenStore by itself.
//
// # Goroutines and handlers
//
// A client runs three goroutines. The connection goroutine dials, reads frames
// and decides when to reconnect. The heartbeat goroutine writes heartbeats, so a
// slow handler can never make the server think the client died. The delivery
// goroutine calls every handler, one at a time, in the order things happened on
// the wire, with state changes interleaved in that same order.
//
// Because handlers do not run on the connection goroutine, a handler may call
// Send, register or remove handlers, or Close. A handler that panics crashes the
// program: there is no recover. A handler that blocks
// stalls later deliveries, and once the delivery queue is full it stalls reading
// too, so do long work elsewhere.
package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"

	"fleetplatform/sdk/go/protocol"
)

// Kind is what a client enrolls as.
type Kind string

const (
	Robot    Kind = "robot"
	Service  Kind = "service"
	Operator Kind = "operator"
)

// State is where the client is in its lifecycle.
type State string

const (
	// StateEnrolling: exchanging the enrollment key for a token (first run only).
	StateEnrolling State = "enrolling"
	// StateConnecting: socket opening or hello sent, waiting for welcome.
	StateConnecting State = "connecting"
	// StateOpen: welcome received, heartbeating; Send works.
	StateOpen State = "open"
	// StateReconnecting: connection lost, waiting out the backoff delay.
	StateReconnecting State = "reconnecting"
	// StateClosed: terminal; Close was called or the server refused the client.
	StateClosed State = "closed"
)

// StateChange is one connection-state transition, as handed to OnState handlers.
type StateChange struct {
	State State
	// Welcome is set when State is StateOpen: the welcome for this connection.
	Welcome *protocol.Welcome
	// RetryIn and Attempt are set when State is StateReconnecting: the delay
	// before the next attempt and its 1-based number since the last welcome.
	RetryIn time.Duration
	Attempt int
	// Err says why the previous connection ended (StateReconnecting) or why the
	// client closed (StateClosed). It is always an *Error.
	Err error
}

// Backoff tunes reconnect delays: Initial * Factor^(n-1), capped at Max, then
// jittered into [d/2, d]. Zero fields take the defaults (250ms, 10s, 2).
type Backoff struct {
	Initial time.Duration
	Max     time.Duration
	Factor  float64
}

func (b Backoff) withDefaults() Backoff {
	if b.Initial <= 0 {
		b.Initial = 250 * time.Millisecond
	}
	if b.Max <= 0 {
		b.Max = 10 * time.Second
	}
	if b.Max < b.Initial {
		b.Max = b.Initial
	}
	if b.Factor < 1 {
		b.Factor = 2
	}
	return b
}

// delay is the wait before the attempt-th retry (1-based).
func (b Backoff) delay(attempt int) time.Duration {
	base := float64(b.Initial) * math.Pow(b.Factor, float64(attempt-1))
	if base > float64(b.Max) || math.IsInf(base, 0) || math.IsNaN(base) {
		base = float64(b.Max)
	}
	return time.Duration(base/2 + rand.Float64()*base/2)
}

// Config describes one client.
type Config struct {
	// URL is the server's websocket endpoint, e.g. "wss://fleet.example.org/ws".
	URL string
	// Kind is what to enroll as. Required.
	Kind Kind
	// Name is the display name sent with enroll.request.
	Name string
	// EnrollKey is the enrollment key (robot, service) or single-use invite key
	// (operator). Used once, and only when no token is stored.
	EnrollKey string
	// TokenFile is where the token persists (a FileTokenStore). With neither
	// TokenFile nor TokenStore the token lives in memory and the next process
	// run enrolls again as a new client.
	TokenFile string
	// TokenStore replaces TokenFile with any other persistence. Set at most one.
	TokenStore TokenStore
	// Agent is sent with enroll.request and every hello.
	Agent *protocol.AgentInfo
	// Backoff tunes the reconnect delays.
	Backoff Backoff
	// NoReconnect makes the first failure terminal, retryable or not.
	NoReconnect bool
	// HandshakeTimeout bounds dialing plus the wait for enroll.response or
	// welcome on one attempt. Default 10s.
	HandshakeTimeout time.Duration
	// OnState, when set, is registered before the first attempt, so it sees
	// every transition from the start. A handler added with Client.OnState
	// after Connect returns has already missed the first StateOpen.
	OnState func(StateChange)
}

const (
	defaultHandshakeTimeout = 10 * time.Second
	// The websocket library's default frame cap is 32 KiB; a snapshot of a large
	// fleet is one frame and can be far bigger.
	maxFrameBytes = 16 << 20
	// Deliveries buffered for the delivery goroutine before reading stalls.
	deliveryQueueDepth = 1024
)

// Client is one connection to fleet-server that keeps itself alive. Create it
// with Connect. All methods are safe for concurrent use.
type Client struct {
	cfg     Config
	store   TokenStore
	backoff Backoff

	// ctx ends when the client closes, for any reason.
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	state   State
	conn    *websocket.Conn // set only while open
	welcome *protocol.Welcome
	creds   *Credentials
	err     error // why the client closed; nil until then

	nextHandler int
	onState     []entry[StateChange]
	onType      map[string][]entry[protocol.Envelope]
	onAny       []entry[protocol.Envelope]

	// queue feeds the delivery goroutine. Only the connection goroutine sends on
	// it (through deliver) and only that goroutine closes it.
	queue chan func()

	opened    chan struct{} // closed at the first welcome
	openedOne sync.Once
	done      chan struct{} // closed after the last handler has run
}

type entry[T any] struct {
	id int
	fn func(T)
}

// Connect enrolls if needed, connects, and returns once the server has
// welcomed the client. From then on the client reconnects by itself until
// Close.
//
// Network failures before the first welcome are retried with backoff, so
// Connect blocks while the server is unreachable; bound that with ctx. ctx
// governs only this call: once Connect has returned a client, cancelling ctx
// does nothing to it. A terminal failure (see Error.Retryable) is returned as an
// *Error.
func Connect(ctx context.Context, cfg Config) (*Client, error) {
	c, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	go c.deliverLoop()
	go c.run()

	select {
	case <-c.opened:
		return c, nil
	case <-c.done:
		return nil, c.Err()
	case <-ctx.Done():
		c.terminate(&Error{Code: CodeClosed, Message: "connect: " + ctx.Err().Error()})
		<-c.done
		return nil, ctx.Err()
	}
}

func newClient(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("fleet: Config.URL %q is not a websocket URL", cfg.URL)
	}
	switch u.Scheme {
	case "ws", "wss", "http", "https":
	default:
		return nil, fmt.Errorf("fleet: Config.URL %q: scheme must be ws or wss", cfg.URL)
	}
	switch cfg.Kind {
	case Robot, Service, Operator:
	default:
		return nil, fmt.Errorf("fleet: Config.Kind %q: want fleet.Robot, fleet.Service or fleet.Operator", cfg.Kind)
	}
	if cfg.TokenFile != "" && cfg.TokenStore != nil {
		return nil, errors.New("fleet: set Config.TokenFile or Config.TokenStore, not both")
	}
	if cfg.HandshakeTimeout <= 0 {
		cfg.HandshakeTimeout = defaultHandshakeTimeout
	}

	store := cfg.TokenStore
	switch {
	case store != nil:
	case cfg.TokenFile != "":
		store = FileTokenStore{Path: cfg.TokenFile}
	default:
		store = &MemoryTokenStore{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		cfg:     cfg,
		store:   store,
		backoff: cfg.Backoff.withDefaults(),
		ctx:     ctx,
		cancel:  cancel,
		state:   StateConnecting,
		onType:  map[string][]entry[protocol.Envelope]{},
		queue:   make(chan func(), deliveryQueueDepth),
		opened:  make(chan struct{}),
		done:    make(chan struct{}),
	}
	if cfg.OnState != nil {
		c.OnState(cfg.OnState)
	}
	return c, nil
}

// Close closes the connection and stops reconnecting. It is terminal and
// idempotent, returns at once, and may be called from a handler. Wait on Done to
// know that no handler is running any more.
func (c *Client) Close() error {
	c.terminate(&Error{Code: CodeClosed, Message: "client closed"})
	return nil
}

// Done is closed once the client is closed and its last handler has returned
// (the StateClosed notification is the last thing delivered).
func (c *Client) Done() <-chan struct{} { return c.done }

// Err is why the client closed: an *Error with CodeClosed after Close, or the
// terminal failure otherwise. It is nil while the client is alive.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// State is the current connection state. It can be ahead of the last state an
// OnState handler was told about, because handlers are called in order from the
// delivery goroutine.
func (c *Client) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Welcome is the welcome of the latest connection.
func (c *Client) Welcome() (protocol.Welcome, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.welcome == nil {
		return protocol.Welcome{}, false
	}
	return *c.welcome, true
}

// ClientID is this client's id (r_..., s_..., o_...). It never changes over the
// life of a client, whatever the number of reconnects.
func (c *Client) ClientID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.welcome != nil {
		return c.welcome.ClientID
	}
	if c.creds != nil {
		return c.creds.ClientID
	}
	return ""
}

// FleetID is the fleet this client belongs to.
func (c *Client) FleetID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.welcome != nil {
		return c.welcome.FleetID
	}
	if c.creds != nil {
		return c.creds.FleetID
	}
	return ""
}

// SendOption adjusts one Send.
type SendOption func(*protocol.Envelope)

// WithID sets the envelope's correlation id. The server echoes it back as ref on
// an error reply.
func WithID(id string) SendOption {
	return func(env *protocol.Envelope) { env.ID = id }
}

// Send writes one envelope of the given type (protocol.Type*) with payload
// marshalled as its payload; a nil payload is sent as {}. It fails with an
// *Error matching ErrClosed unless the connection is open right now: nothing is
// queued across a reconnect. A nil error means the frame was written, not that
// the server accepted it; refusals come back as error envelopes.
//
// ctx bounds the write. Cancelling it mid-write tears the connection down (the
// client then reconnects), so use it as a deadline, not as routine control flow.
func (c *Client) Send(ctx context.Context, typ string, payload any, opts ...SendOption) error {
	raw := json.RawMessage("{}")
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("fleet: marshal %s payload: %w", typ, err)
		}
		raw = b
	}
	env := protocol.Envelope{V: protocol.Version, Type: typ, TsMs: time.Now().UnixMilli(), Payload: raw}
	for _, opt := range opts {
		opt(&env)
	}

	c.mu.Lock()
	conn, state := c.conn, c.state
	c.mu.Unlock()
	if conn == nil || state != StateOpen {
		return &Error{Code: CodeClosed, Message: fmt.Sprintf("cannot send %s: connection is %s", typ, state)}
	}
	if err := writeEnvelope(ctx, conn, env); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Error{Code: CodeNetwork, Message: fmt.Sprintf("send %s: %v", typ, err)}
	}
	return nil
}

// On registers a handler for inbound envelopes of one type (protocol.Type*).
// The returned function removes it.
func (c *Client) On(typ string, handler func(protocol.Envelope)) (remove func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.handlerID()
	c.onType[typ] = append(c.onType[typ], entry[protocol.Envelope]{id, handler})
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.onType[typ] = without(c.onType[typ], id)
	}
}

// OnMessage registers a handler for every inbound envelope. The returned
// function removes it.
func (c *Client) OnMessage(handler func(protocol.Envelope)) (remove func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.handlerID()
	c.onAny = append(c.onAny, entry[protocol.Envelope]{id, handler})
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.onAny = without(c.onAny, id)
	}
}

// OnState registers a handler for connection-state changes. The returned
// function removes it. See Config.OnState for a handler that must not miss the
// first StateOpen.
func (c *Client) OnState(handler func(StateChange)) (remove func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.handlerID()
	c.onState = append(c.onState, entry[StateChange]{id, handler})
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.onState = without(c.onState, id)
	}
}

// handlerID needs c.mu held.
func (c *Client) handlerID() int {
	c.nextHandler++
	return c.nextHandler
}

// without returns a new slice, so a dispatch that copied the old one under the
// lock keeps iterating it safely.
func without[T any](list []entry[T], id int) []entry[T] {
	out := make([]entry[T], 0, len(list))
	for _, e := range list {
		if e.id != id {
			out = append(out, e)
		}
	}
	return out
}

// ---------------------------------------------------------------- lifecycle

// run is the connection goroutine: one attempt after another until the client
// closes.
func (c *Client) run() {
	defer close(c.queue)
	attempt := 0
	for {
		welcomed, err := c.attempt()
		if c.ctx.Err() != nil {
			return
		}
		if welcomed {
			attempt = 0
		}
		if !err.Retryable() || c.cfg.NoReconnect {
			c.terminate(err)
			return
		}
		attempt++
		delay := c.backoff.delay(attempt)
		c.setState(StateChange{State: StateReconnecting, RetryIn: delay, Attempt: attempt, Err: err})
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-c.ctx.Done():
			timer.Stop()
			return
		}
	}
}

// attempt gets credentials (enrolling on the very first run) and runs one
// session to its end. welcomed reports whether the session got as far as open.
func (c *Client) attempt() (welcomed bool, _ *Error) {
	c.mu.Lock()
	creds := c.creds
	c.mu.Unlock()

	if creds == nil {
		loaded, err := c.store.Load()
		if err != nil {
			return false, &Error{Code: CodeTokenStore, Message: "load: " + err.Error()}
		}
		if loaded == nil {
			c.setState(StateChange{State: StateEnrolling})
			enrolled, eerr := c.enroll()
			if eerr != nil {
				return false, eerr
			}
			// A token that cannot be persisted is terminal, not retried: every
			// retry would enroll again and mint one more identity.
			if err := c.store.Save(*enrolled); err != nil {
				return false, &Error{Code: CodeTokenStore, Message: "save: " + err.Error()}
			}
			loaded = enrolled
		}
		creds = loaded
		c.mu.Lock()
		c.creds = creds
		c.mu.Unlock()
	}
	return c.session(creds.Token)
}

// enroll is the one-shot exchange on its own socket: enroll.request ->
// enroll.response, after which the server closes it.
func (c *Client) enroll() (*Credentials, *Error) {
	if c.cfg.EnrollKey == "" {
		return nil, &Error{Code: protocol.ErrAuthFailed, Message: "no stored token and no EnrollKey to enroll with"}
	}
	ctx, cancel := context.WithTimeout(c.ctx, c.cfg.HandshakeTimeout)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, c.cfg.URL, nil)
	if err != nil {
		return nil, c.handshakeError(ctx, "enroll: dial", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxFrameBytes)

	req := protocol.EnrollRequest{EnrollmentKey: c.cfg.EnrollKey, Kind: string(c.cfg.Kind), Name: c.cfg.Name, Agent: c.cfg.Agent}
	if err := writeEnvelope(ctx, conn, protocol.Msg(protocol.TypeEnrollRequest, req)); err != nil {
		return nil, c.handshakeError(ctx, "enroll: send", err)
	}
	env, ok, err := readEnvelope(ctx, conn)
	if err != nil {
		return nil, c.handshakeError(ctx, "enroll: no response", err)
	}
	switch {
	case ok && env.Type == protocol.TypeEnrollResponse:
		var resp protocol.EnrollResponse
		if json.Unmarshal(env.Payload, &resp) != nil || resp.Token == "" || resp.ClientID == "" {
			return nil, &Error{Code: CodeProtocol, Message: "enroll: malformed enroll.response"}
		}
		return &Credentials{Token: resp.Token, ClientID: resp.ClientID, FleetID: resp.FleetID}, nil
	case ok && env.Type == protocol.TypeError:
		return nil, serverError("enroll: ", env)
	case ok:
		return nil, &Error{Code: CodeProtocol, Message: "enroll: unexpected " + env.Type}
	default:
		return nil, &Error{Code: CodeProtocol, Message: "enroll: unexpected frame"}
	}
}

// session opens the session socket and stays on it until it ends: hello ->
// welcome -> heartbeat and read. It always returns the reason the session ended.
func (c *Client) session(token string) (welcomed bool, _ *Error) {
	c.setState(StateChange{State: StateConnecting})

	hctx, cancelHandshake := context.WithTimeout(c.ctx, c.cfg.HandshakeTimeout)
	defer cancelHandshake()

	conn, _, err := websocket.Dial(hctx, c.cfg.URL, nil)
	if err != nil {
		return false, c.handshakeError(hctx, "dial", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(maxFrameBytes)

	hello := protocol.Hello{Token: token, Agent: c.cfg.Agent}
	if err := writeEnvelope(hctx, conn, protocol.Msg(protocol.TypeHello, hello)); err != nil {
		return false, c.handshakeError(hctx, "hello", err)
	}

	// Before welcome, any error envelope is the handshake's answer.
	var welcome protocol.Welcome
	for {
		env, ok, err := readEnvelope(hctx, conn)
		if err != nil {
			return false, c.handshakeError(hctx, "no welcome after hello", err)
		}
		if !ok {
			continue
		}
		if env.Type == protocol.TypeError {
			return false, serverError("", env)
		}
		if env.Type != protocol.TypeWelcome {
			continue
		}
		if json.Unmarshal(env.Payload, &welcome) != nil || welcome.ClientID == "" || welcome.HeartbeatIntervalMs <= 0 {
			return false, &Error{Code: CodeProtocol, Message: "malformed welcome"}
		}
		break
	}
	cancelHandshake()

	if !c.sessionOpened(conn, &welcome) {
		return false, &Error{Code: CodeClosed, Message: "client closed"}
	}
	defer c.sessionLost()

	hbCtx, stopHeartbeat := context.WithCancel(c.ctx)
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		heartbeat(hbCtx, conn, time.Duration(welcome.HeartbeatIntervalMs)*time.Millisecond)
	}()
	defer func() {
		stopHeartbeat()
		conn.CloseNow() // unblocks a heartbeat write in flight
		<-hbDone
	}()

	// After welcome, error envelopes are replies to our sends and do not close
	// the socket, with three exceptions the server sends as the LAST frame
	// before it closes: conflict (taken over), rate_limited (heartbeat lapsed),
	// auth_failed (token revoked). The same codes also arrive as ordinary
	// replies (a refused lease.claim, a throttled publish), so one counts as the
	// reason only if nothing came after it.
	var closing *Error
	for {
		env, ok, err := readEnvelope(c.ctx, conn)
		if err != nil {
			switch {
			case c.ctx.Err() != nil:
				return true, &Error{Code: CodeClosed, Message: "client closed"}
			case closing != nil:
				return true, closing
			default:
				return true, &Error{Code: CodeNetwork, Message: "socket closed: " + err.Error()}
			}
		}
		if !ok {
			continue
		}
		closing = nil
		if env.Type == protocol.TypeError {
			if e := serverError("", env); e.Code == protocol.ErrConflict || e.Code == protocol.ErrRateLimited || e.Code == protocol.ErrAuthFailed {
				closing = e
			}
		}
		c.route(env)
	}
}

// sessionOpened runs on the connection goroutine the moment a welcome arrives,
// before anything else is read from the new socket and before any handler hears
// about StateOpen. Work that must happen first on every (re)connect belongs
// here, ahead of the setState call: whatever it writes to conn is on the wire
// before a handler can Send.
func (c *Client) sessionOpened(conn *websocket.Conn, welcome *protocol.Welcome) bool {
	c.mu.Lock()
	if c.state == StateClosed {
		c.mu.Unlock()
		return false
	}
	c.conn = conn
	c.welcome = welcome
	c.mu.Unlock()

	w := *welcome
	c.setState(StateChange{State: StateOpen, Welcome: &w})
	c.openedOne.Do(func() { close(c.opened) })
	return true
}

// sessionLost runs on the connection goroutine when an open session's socket is
// gone, whether it dropped or the client is closing. Per-connection state is
// discarded here.
func (c *Client) sessionLost() {
	c.mu.Lock()
	c.conn = nil
	c.mu.Unlock()
}

// route takes one inbound envelope of an open session, on the connection
// goroutine, in wire order. Connection-level consumers that must see a frame
// before (or instead of) the handlers act here; what handlers should see is
// passed on with deliver.
func (c *Client) route(env protocol.Envelope) {
	c.deliver(func() { c.dispatch(env) })
}

// deliver queues fn for the delivery goroutine, behind everything queued before
// it. Call it from the connection goroutine only: that single sender is what
// keeps handler order equal to wire order, and what makes closing the queue
// safe. It blocks while the queue is full and drops fn if the client closes.
func (c *Client) deliver(fn func()) {
	select {
	case c.queue <- fn:
	case <-c.ctx.Done():
	}
}

// deliverLoop is the delivery goroutine. Whatever is still queued when the
// client closes is dropped; StateClosed is the last thing any handler sees.
func (c *Client) deliverLoop() {
	for fn := range c.queue {
		if c.ctx.Err() == nil {
			fn()
		}
	}
	c.notifyState(StateChange{State: StateClosed, Err: c.Err()})
	close(c.done)
}

// dispatch calls the handlers for one envelope; delivery goroutine only.
func (c *Client) dispatch(env protocol.Envelope) {
	c.mu.Lock()
	typed, all := c.onType[env.Type], c.onAny
	c.mu.Unlock()
	for _, h := range typed {
		h.fn(env)
	}
	for _, h := range all {
		h.fn(env)
	}
}

// notifyState calls the state handlers; delivery goroutine only.
func (c *Client) notifyState(ch StateChange) {
	c.mu.Lock()
	handlers := c.onState
	c.mu.Unlock()
	for _, h := range handlers {
		h.fn(ch)
	}
}

// setState records a non-terminal state and queues its notification. Connection
// goroutine only. The terminal state goes through terminate.
func (c *Client) setState(ch StateChange) {
	c.mu.Lock()
	if c.state == StateClosed {
		c.mu.Unlock()
		return
	}
	c.state = ch.State
	c.mu.Unlock()
	c.deliver(func() { c.notifyState(ch) })
}

// terminate closes the client for good, from any goroutine. The connection
// goroutine sees ctx end, tears the socket down and closes the queue; the
// delivery goroutine then reports StateClosed and closes done.
func (c *Client) terminate(err *Error) {
	c.mu.Lock()
	if c.state == StateClosed {
		c.mu.Unlock()
		return
	}
	c.state = StateClosed
	c.err = err
	c.mu.Unlock()
	c.cancel()
}

// handshakeError names a failure before welcome: the attempt's deadline, the
// client closing, or the network.
func (c *Client) handshakeError(hctx context.Context, what string, err error) *Error {
	switch {
	case c.ctx.Err() != nil:
		return &Error{Code: CodeClosed, Message: "client closed"}
	case errors.Is(hctx.Err(), context.DeadlineExceeded):
		return &Error{Code: CodeTimeout, Message: what + ": timed out"}
	default:
		return &Error{Code: CodeNetwork, Message: what + ": " + err.Error()}
	}
}

// heartbeat writes one heartbeat per interval until ctx ends or a write fails;
// the read loop is what notices a dead socket.
func heartbeat(ctx context.Context, conn *websocket.Conn, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if writeEnvelope(ctx, conn, protocol.Msg(protocol.TypeHeartbeat, protocol.Heartbeat{})) != nil {
				return
			}
		}
	}
}

func writeEnvelope(ctx context.Context, conn *websocket.Conn, env protocol.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

// readEnvelope reads one frame. ok is false for a frame that is not a v0
// envelope with an object payload; such frames are skipped, not fatal.
func readEnvelope(ctx context.Context, conn *websocket.Conn) (env protocol.Envelope, ok bool, err error) {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return env, false, err
	}
	if json.Unmarshal(data, &env) != nil || env.V != protocol.Version || env.Type == "" {
		return env, false, nil
	}
	if len(env.Payload) == 0 || env.Payload[0] != '{' {
		return env, false, nil
	}
	return env, true, nil
}

// serverError turns an error envelope into an *Error.
func serverError(prefix string, env protocol.Envelope) *Error {
	var msg protocol.ErrorMsg
	if json.Unmarshal(env.Payload, &msg) != nil || msg.Code == "" {
		return &Error{Code: CodeProtocol, Message: prefix + "malformed error envelope"}
	}
	return &Error{Code: msg.Code, Message: prefix + msg.Message}
}
