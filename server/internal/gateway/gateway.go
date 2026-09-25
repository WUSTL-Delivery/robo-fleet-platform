// Package gateway owns the WebSocket edge: accept, the enroll/hello handshake,
// per-client send queues and inbound rate limits. BACKPRESSURE LIVES HERE
// (DESIGN.md D10): a slow consumer overflows its queue and is disconnected; a
// chatty producer is throttled (ratelimit.go) and keeps its connection.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"fleetplatform/server/internal/bus"
	"fleetplatform/server/internal/protocol"
	"fleetplatform/server/internal/store"
)

const (
	handshakeTimeout = 10 * time.Second
	writeTimeout     = 5 * time.Second
	sendQueueDepth   = 64
)

// Auth is implemented by the app over the store.
type Auth interface {
	Enroll(req protocol.EnrollRequest) (protocol.EnrollResponse, *protocol.ErrorMsg)
	Hello(h protocol.Hello) (store.Client, *protocol.ErrorMsg)
}

// Handler receives lifecycle + messages for authenticated connections.
type Handler interface {
	OnConnect(c *Conn)
	OnMessage(c *Conn, env protocol.Envelope)
	OnDisconnect(c *Conn)
}

// Conn is one authenticated client connection.
type Conn struct {
	Client store.Client

	ws     *websocket.Conn
	sendCh chan protocol.Envelope
	done   chan struct{}
}

// Send enqueues without blocking. On overflow the connection is torn down:
// dropping a slow consumer beats letting it backpressure the whole server.
func (c *Conn) Send(env protocol.Envelope) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.sendCh <- env:
		return true
	default:
		slog.Warn("gateway: send queue overflow, dropping client", "client", c.Client.ID)
		c.ws.Close(websocket.StatusPolicyViolation, "send queue overflow")
		return false
	}
}

// Close tears the connection down; the read loop returns and the handler's
// OnDisconnect runs. It does not wait for the peer's close handshake: the
// callers drop peers that have lapsed or been superseded, and a frozen peer
// would otherwise hold off robot.offline for the handshake timeout (5 s).
func (c *Conn) Close() {
	c.ws.CloseNow()
}

type Gateway struct {
	Auth                Auth
	Handler             Handler
	HeartbeatIntervalMs int
	// RateLimit meters telemetry and channel.publish per connection; the zero
	// value disables it.
	RateLimit RateLimit
}

func (g *Gateway) ServeWS(w http.ResponseWriter, r *http.Request) {
	// v0 accepts any origin: robots/services have no origin, and console origin
	// enforcement arrives with console auth.
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		return
	}
	ws.SetReadLimit(2 * bus.MaxPayloadBytes)

	ctx := r.Context()
	first, err := readEnvelope(ctx, ws, handshakeTimeout)
	if err != nil {
		ws.Close(websocket.StatusPolicyViolation, "expected hello or enroll.request")
		return
	}

	switch first.Type {
	case protocol.TypeEnrollRequest:
		g.serveEnroll(ctx, ws, first)
	case protocol.TypeHello:
		g.serveSession(ctx, ws, first)
	default:
		writeEnvelope(ctx, ws, protocol.Msg(protocol.TypeError, protocol.ErrorMsg{
			Code: protocol.ErrInvalidMessage, Message: "first message must be hello or enroll.request", Ref: first.ID,
		}))
		ws.Close(websocket.StatusPolicyViolation, "bad handshake")
	}
}

func (g *Gateway) serveEnroll(ctx context.Context, ws *websocket.Conn, env protocol.Envelope) {
	var req protocol.EnrollRequest
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		writeEnvelope(ctx, ws, errEnvelope(protocol.ErrInvalidMessage, "malformed enroll.request", env.ID))
		ws.Close(websocket.StatusPolicyViolation, "bad enroll")
		return
	}
	resp, errMsg := g.Auth.Enroll(req)
	if errMsg != nil {
		errMsg.Ref = env.ID
		writeEnvelope(ctx, ws, protocol.Msg(protocol.TypeError, *errMsg))
		ws.Close(websocket.StatusPolicyViolation, "enroll rejected")
		return
	}
	writeEnvelope(ctx, ws, protocol.Msg(protocol.TypeEnrollResponse, resp))
	// Enrollment is a one-shot exchange; the client reconnects with its token.
	ws.Close(websocket.StatusNormalClosure, "enrolled")
}

func (g *Gateway) serveSession(ctx context.Context, ws *websocket.Conn, env protocol.Envelope) {
	var hello protocol.Hello
	if err := json.Unmarshal(env.Payload, &hello); err != nil {
		writeEnvelope(ctx, ws, errEnvelope(protocol.ErrInvalidMessage, "malformed hello", env.ID))
		ws.Close(websocket.StatusPolicyViolation, "bad hello")
		return
	}
	client, errMsg := g.Auth.Hello(hello)
	if errMsg != nil {
		errMsg.Ref = env.ID
		writeEnvelope(ctx, ws, protocol.Msg(protocol.TypeError, *errMsg))
		ws.Close(websocket.StatusPolicyViolation, "auth failed")
		return
	}

	c := &Conn{
		Client: client,
		ws:     ws,
		sendCh: make(chan protocol.Envelope, sendQueueDepth),
		done:   make(chan struct{}),
	}
	go c.writeLoop(ctx)
	// Register before welcoming: once a client sees welcome, it is present.
	g.Handler.OnConnect(c)
	c.Send(protocol.Msg(protocol.TypeWelcome, protocol.Welcome{
		ClientID:            client.ID,
		FleetID:             client.FleetID,
		Kind:                string(client.Kind),
		ServerTimeMs:        time.Now().UnixMilli(),
		HeartbeatIntervalMs: g.HeartbeatIntervalMs,
	}))
	defer func() {
		close(c.done)
		g.Handler.OnDisconnect(c)
		ws.Close(websocket.StatusNormalClosure, "")
	}()

	lim := newLimiter(g.RateLimit)
	for {
		env, err := readEnvelope(ctx, ws, 0)
		if err != nil {
			return
		}
		if env.V != protocol.Version {
			c.Send(errEnvelope(protocol.ErrInvalidMessage, "unsupported protocol version", env.ID))
			continue
		}
		if now := time.Now(); !lim.allow(env.Type, now) {
			// Throttle, never disconnect: drop it, and say so at most once per
			// notice window so the replies cannot overflow the send queue.
			if lim.notify(now) {
				c.Send(errEnvelope(protocol.ErrRateLimited, env.Type+" rate limit exceeded; dropping until the client slows down", env.ID))
			}
			continue
		}
		g.Handler.OnMessage(c, env)
	}
}

func (c *Conn) writeLoop(ctx context.Context) {
	for {
		select {
		case env := <-c.sendCh:
			if err := writeEnvelope(ctx, c.ws, env); err != nil {
				c.ws.Close(websocket.StatusNormalClosure, "write failed")
				return
			}
		case <-c.done:
			return
		}
	}
}

func readEnvelope(ctx context.Context, ws *websocket.Conn, timeout time.Duration) (protocol.Envelope, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	_, data, err := ws.Read(ctx)
	if err != nil {
		return protocol.Envelope{}, err
	}
	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return protocol.Envelope{}, err
	}
	if env.Type == "" || env.Payload == nil {
		return protocol.Envelope{}, errors.New("gateway: malformed envelope")
	}
	return env, nil
}

func writeEnvelope(ctx context.Context, ws *websocket.Conn, env protocol.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return ws.Write(wctx, websocket.MessageText, data)
}

func errEnvelope(code, msg, ref string) protocol.Envelope {
	return protocol.Msg(protocol.TypeError, protocol.ErrorMsg{Code: code, Message: msg, Ref: ref})
}
