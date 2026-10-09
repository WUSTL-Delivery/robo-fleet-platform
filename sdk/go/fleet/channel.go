package fleet

// Channels: the opaque domain bus (docs/INTEGRATION.md section 2.6). The
// platform never looks inside data; anything shaped like an order, a waypoint
// list or an edge report rides on a channel.
//
//	status := client.Channel("delivery_status")
//	status.OnMessage(func(from string, data json.RawMessage) { ... })
//	status.Broadcast(ctx, report)        // everyone subscribed, sender excluded
//	status.Publish(ctx, robotID, note)   // one client; at-most-once, no receipt
//
//	jobs := client.Channel("assignment")
//	err := jobs.SendAcked(ctx, robotID, job, 10*time.Second) // nil: the robot accepted it
//	jobs.OnAcked(func(from string, data json.RawMessage) error { ... }) // the receiving side
//
// # Receiving
//
// Directed messages reach a client whether or not it subscribed, and so do
// broadcasts to a robot whose manifest lists the channel. Everyone else needs
// the channel's topic to receive broadcasts, so OnMessage adds
// ChannelTopic(name) to the client's subscription (see Subscribe: it is
// remembered and renewed on every reconnect).
//
// # Acked send
//
// channel.publish is at-most-once. SendAcked and OnAcked are the two halves of
// the acked-send convention, whose single reference is protocol/README.md,
// "Acked send (convention on channel data)": the sender publishes
// {"seq": n, "data": ...} and re-sends it until the receiver answers
// {"ack": n} on the same channel. Both shapes are reserved on every channel.
// The guarantee is at-least-once while the sender keeps trying, and
// exactly-once to the receiving application within one receiver process
// lifetime; data whose effect must not happen twice needs its own identity
// inside the inner data.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"fleetplatform/sdk/go/protocol"
)

// Acked-send bounds, from protocol/README.md.
const (
	// DefaultAckedTimeout is SendAcked's deadline when the caller passes 0.
	DefaultAckedTimeout = 10 * time.Second
	// MaxAckedTimeout is the longest deadline the convention allows.
	MaxAckedTimeout = 5 * time.Minute
	// DefaultAckedResend is the default interval between re-sends
	// (Config.AckedResend).
	DefaultAckedResend = time.Second

	minAckedResend = 250 * time.Millisecond
	maxAckedResend = 5 * time.Second
	// ackedMemory is how long the receiver remembers an accepted message:
	// twice the longest sender deadline, so no legal re-send outlives its key.
	ackedMemory = 10 * time.Minute
	// maxSeq is the largest valid seq, 2^53 - 1; the smallest is 1.
	maxSeq = 1<<53 - 1
)

// channelNamePattern is the protocol's bound on a channel name
// (protocol/schemas/defs.schema.json, channelName).
var channelNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

// Channel is a view of one channel on one client. It is cheap to create and
// holds no state of its own: two Channels for the same name share handlers.
type Channel struct {
	c    *Client
	name string
}

// Channel returns the view of one channel. It panics if name is not a valid
// channel name (lower-case letters, digits, "_", "." and "-", at most 64
// characters, not starting with punctuation): names are constants of the
// program, so a bad one is a bug, not a runtime condition.
func (c *Client) Channel(name string) *Channel {
	if !channelNamePattern.MatchString(name) {
		panic(fmt.Sprintf("fleet: Channel(%q): not a valid channel name", name))
	}
	return &Channel{c: c, name: name}
}

// Name is the channel's name.
func (ch *Channel) Name() string { return ch.name }

// Topic is the Subscribe topic that carries this channel's broadcasts.
func (ch *Channel) Topic() string { return ChannelTopic(ch.name) }

// Publish sends data to one client of the fleet. data is marshalled as JSON
// (a json.RawMessage is sent as it is; nil is sent as null).
//
// Delivery is at-most-once and a nil error is not a receipt: it means the
// frame was written. If the target is not connected the server answers with an
// error envelope (code not_found), which reaches the client's
// On(protocol.TypeError) handlers and is not returned here. Use SendAcked to
// know that the target got the message.
//
// Like Send, it fails with ErrClosed unless the connection is open right now.
func (ch *Channel) Publish(ctx context.Context, to string, data any) error {
	if to == "" {
		return errors.New("fleet: Publish needs a target client id; use Broadcast to reach every subscriber")
	}
	raw, err := marshalData(data)
	if err != nil {
		return err
	}
	return ch.c.Send(ctx, protocol.TypeChannelPublish, protocol.ChannelPublish{Channel: ch.name, To: to, Data: raw})
}

// Broadcast sends data to every client subscribed to the channel's topic and
// to every robot whose manifest lists the channel, the sender excluded. It is
// at-most-once, like Publish.
func (ch *Channel) Broadcast(ctx context.Context, data any) error {
	raw, err := marshalData(data)
	if err != nil {
		return err
	}
	return ch.c.Send(ctx, protocol.TypeChannelPublish, protocol.ChannelPublish{Channel: ch.name, Broadcast: true, Data: raw})
}

// OnMessage registers a handler for the messages on this channel, directed or
// broadcast. from is the sender's client id, stamped by the server; answer it
// with Publish(ctx, from, ...). The returned function removes the handler.
//
// It also subscribes the client to the channel's topic, now if the connection
// is open and again on every reconnect, so broadcasts arrive without a call to
// Subscribe. That subscribe is answered by a snapshot like any other, which
// OnSnapshot handlers receive. Removing the handler does not unsubscribe (the
// protocol has no unsubscribe); the messages are then dropped client-side.
//
// Two kinds of data are not shown to OnMessage handlers, because they belong
// to the acked-send convention: acks ({"ack": n}), and acked messages
// ({"seq": n, "data": ...}) on a channel that has an OnAcked handler. The raw
// On(protocol.TypeChannelMessage) handlers see everything.
func (ch *Channel) OnMessage(handler func(from string, data json.RawMessage)) (remove func()) {
	c := ch.c
	c.mu.Lock()
	id := c.handlerID()
	if c.channels.onMessage == nil {
		c.channels.onMessage = map[string][]entry[protocol.ChannelMessage]{}
	}
	c.channels.onMessage[ch.name] = append(c.channels.onMessage[ch.name], entry[protocol.ChannelMessage]{
		id, func(m protocol.ChannelMessage) { handler(m.From, m.Data) },
	})
	c.mu.Unlock()

	c.startSubscribe([]string{ch.Topic()}, true)
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.channels.onMessage[ch.name] = without(c.channels.onMessage[ch.name], id)
	}
}

// OnAcked registers the handler that receives acked sends on this channel
// (the other end's SendAcked). It is called once per message with the inner
// data, however many times the sender re-sends it:
//
//   - the handler returns nil: the message is accepted. The client publishes
//     the ack to the sender, and again for every later copy of the same
//     message, without calling the handler again;
//   - the handler returns an error: nothing is sent and the message is
//     forgotten, so the sender's next re-send calls the handler again.
//
// An ack means accepted, not finished: return well inside the sender's re-send
// interval (1 s by default) and do long work elsewhere, reporting its result
// as ordinary channel data. Accepted messages are remembered for 10 minutes,
// in memory only: a receiver that restarts handles a late re-send again.
//
// Acked sends are directed, and directed messages need no subscription, so
// OnAcked does not subscribe. A channel has one acked handler, because one
// handler's outcome decides the ack: OnAcked panics if the channel already has
// one. The returned function removes it.
func (ch *Channel) OnAcked(handler func(from string, data json.RawMessage) error) (remove func()) {
	c := ch.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.channels.onAcked == nil {
		c.channels.onAcked = map[string]ackedHandler{}
	}
	if _, taken := c.channels.onAcked[ch.name]; taken {
		panic(fmt.Sprintf("fleet: channel %q already has an OnAcked handler", ch.name))
	}
	id := c.handlerID()
	c.channels.onAcked[ch.name] = ackedHandler{id, handler}
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.channels.onAcked[ch.name].id == id {
			delete(c.channels.onAcked, ch.name)
		}
	}
}

// ---------------------------------------------------------------- internals

// channels is the channel layer's state on one client.
type channels struct {
	// Handlers, guarded by Client.mu like the core's.
	onMessage map[string][]entry[protocol.ChannelMessage]
	onAcked   map[string]ackedHandler

	// The acked-send receiver's seen set. Delivery goroutine only.
	seen      map[ackedKey]time.Time // accepted messages -> when first received
	seenOrder []ackedKey             // the same keys, oldest first
	now       func() time.Time       // time.Now unless a test replaces it

	// The acked-send sender.
	seq  seqCounter
	mu   sync.Mutex
	open chan struct{} // closed, and replaced, each time a session opens
	// pending: sends not resolved yet, by seq.
	pending map[int64]*ackedSend
}

type ackedHandler struct {
	id int
	fn func(from string, data json.RawMessage) error
}

// ackedKey identifies one acked message at the receiver. from is stamped by
// the server, so the key survives either side reconnecting.
type ackedKey struct {
	channel, from string
	seq           int64
}

func marshalData(data any) (json.RawMessage, error) {
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("fleet: marshal channel data: %w", err)
	}
	return raw, nil
}

// ackedMessage reports whether data is exactly an acked message, an object
// with just the members seq and data where seq is a valid seq, and returns its
// parts. A null inner data is legal; a missing one is not.
func ackedMessage(data json.RawMessage) (seq int64, inner json.RawMessage, ok bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil || len(obj) != 2 {
		return 0, nil, false
	}
	inner, has := obj["data"]
	if !has {
		return 0, nil, false
	}
	seq, ok = validSeq(obj["seq"])
	return seq, inner, ok
}

// ackMessage reports whether data is exactly an ack, an object with just the
// member ack holding a valid seq.
func ackMessage(data json.RawMessage) (seq int64, ok bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(data, &obj) != nil || len(obj) != 1 {
		return 0, false
	}
	return validSeq(obj["ack"])
}

// validSeq accepts a JSON integer in 1..2^53-1 and nothing else: not a string,
// not a fraction or an exponent (1.0, 1e3), which other SDKs read as
// non-integers too.
func validSeq(raw json.RawMessage) (int64, bool) {
	s := string(raw)
	if s == "" || len(s) > 16 || strings.Trim(s, "0123456789") != "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 || n > maxSeq {
		return 0, false
	}
	return n, true
}

// dispatchChannels hands one channel.message to the channel handlers; delivery
// goroutine only. A payload that does not decode is skipped: the raw handlers
// have already seen the envelope.
func (c *Client) dispatchChannels(env protocol.Envelope) {
	if env.Type != protocol.TypeChannelMessage {
		return
	}
	var msg protocol.ChannelMessage
	if json.Unmarshal(env.Payload, &msg) != nil || msg.Channel == "" {
		return
	}
	c.mu.Lock()
	handlers := c.channels.onMessage[msg.Channel]
	acked, hasAcked := c.channels.onAcked[msg.Channel]
	c.mu.Unlock()

	if hasAcked && msg.From != "" {
		if seq, inner, ok := ackedMessage(msg.Data); ok {
			c.receiveAcked(ackedKey{msg.Channel, msg.From, seq}, inner, acked)
			return
		}
	}
	// An ack was for the sender half, which saw it in observeAcked. One that
	// matched no pending send is ignored silently.
	if _, isAck := ackMessage(msg.Data); isAck {
		return
	}
	call(handlers, msg)
}

// receiveAcked is the receiver half of the convention. Handlers run one at a
// time on the delivery goroutine, so a repeat can never arrive while the
// handler for its first copy is still running: the convention's "in progress"
// state is never observable here, and a key is recorded only once accepted.
func (c *Client) receiveAcked(key ackedKey, inner json.RawMessage, h ackedHandler) {
	ch := &c.channels
	now := time.Now()
	if ch.now != nil {
		now = ch.now()
	}
	// Forget what is older than the memory window, oldest first.
	for len(ch.seenOrder) > 0 && now.Sub(ch.seen[ch.seenOrder[0]]) > ackedMemory {
		delete(ch.seen, ch.seenOrder[0])
		ch.seenOrder = ch.seenOrder[1:]
	}
	if _, done := ch.seen[key]; !done {
		if h.fn(key.from, inner) != nil {
			return // failed: no ack, and the next re-send is treated as new
		}
		if ch.seen == nil {
			ch.seen = map[ackedKey]time.Time{}
		}
		ch.seen[key] = now
		ch.seenOrder = append(ch.seenOrder, key)
	}
	// Once per received copy, never retried: if it cannot be sent (link down,
	// sender gone) the sender's next re-send is answered.
	ctx, cancel := context.WithTimeout(c.ctx, c.cfg.HandshakeTimeout)
	defer cancel()
	c.Send(ctx, protocol.TypeChannelPublish, protocol.ChannelPublish{
		Channel: key.channel, To: key.from, Data: json.RawMessage(`{"ack":` + strconv.FormatInt(key.seq, 10) + `}`),
	})
}
