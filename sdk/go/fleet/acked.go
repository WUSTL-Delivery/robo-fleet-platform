package fleet

// The sender half of the acked-send convention (protocol/README.md, "Acked
// send (convention on channel data)", Sender). The receiver half and the
// Channel type are in channel.go.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"fleetplatform/sdk/go/protocol"
)

// seqCounter allocates seqs: one counter for the whole client, shared by every
// channel and target, seeded with the Unix time in milliseconds because it is
// not persisted. That keeps a seq unique per (sender, channel) across a
// restart, as the convention requires.
type seqCounter struct{ last atomic.Int64 }

func (s *seqCounter) seed(now time.Time) { s.last.Store(now.UnixMilli()) }
func (s *seqCounter) next() int64        { return s.last.Add(1) }

// ackedSend is one SendAcked call waiting for its outcome.
type ackedSend struct {
	channel, to string
	// done receives the outcome exactly once: nil for an ack, else the failure.
	// Buffered, so the connection goroutine never waits for the caller.
	done chan error
}

// ackedRef is the envelope id of one transmission: the seq and the 1-based
// number of the transmission, so an error's ref says which send it answers and
// whether that was the first copy.
func ackedRef(seq int64, n int) string { return fmt.Sprintf("acked-%d.%d", seq, n) }

func parseAckedRef(ref string) (seq int64, n int, ok bool) {
	rest, found := strings.CutPrefix(ref, "acked-")
	if !found {
		return 0, 0, false
	}
	a, b, found := strings.Cut(rest, ".")
	if !found {
		return 0, 0, false
	}
	seq, err1 := strconv.ParseInt(a, 10, 64)
	n, err2 := strconv.Atoi(b)
	return seq, n, err1 == nil && err2 == nil
}

// SendAcked sends data to one client and returns nil once that client's
// application has accepted it (its OnAcked handler, or on_acked in the Python
// SDK, returned without error). Until then it re-sends the same message every
// Config.AckedResend.
//
// timeout is the deadline for the whole send, measured from this call; 0 means
// DefaultAckedTimeout (10 s), and more than MaxAckedTimeout (5 minutes) is an
// error. It keeps running while this client is disconnected: re-sending pauses
// and resumes, as the same message, once the client is connected again.
//
// SendAcked blocks until one of these, and reports it as:
//
//   - nil: acked. The receiving application accepted the data.
//   - ErrNotFound: the server says the target is not connected. Returned at
//     once, without waiting out the deadline. If it answers the first copy
//     the data was not delivered; if it answers a re-send, an earlier copy may
//     have been delivered and its ack lost (the error's message says which).
//   - ErrInvalidMessage or ErrNotAuthorized: the server refused the publish.
//   - ErrTimeout: the deadline passed. Unknown: the data may or may not have
//     been delivered.
//   - ctx.Err() if ctx ends first, and c.Err() if the client closes first;
//     both unknown in the same way.
//
// Match them with errors.Is. A caller that tries again after a failure is
// making a new send: because the earlier outcome may be unknown, data whose
// effect must not happen twice needs its own identity inside data.
//
// It may be called from any goroutine, including from inside a handler: acks
// are matched on the connection goroutine, not the delivery goroutine, so a
// handler blocked here does not hold up its own answer. It does hold up every
// later delivery, so prefer calling it from a goroutine of your own.
func (ch *Channel) SendAcked(ctx context.Context, to string, data any, timeout time.Duration) error {
	c := ch.c
	switch {
	case to == "":
		return errors.New("fleet: SendAcked needs a target client id; an acked send is never a broadcast")
	case timeout < 0 || timeout > MaxAckedTimeout:
		return fmt.Errorf("fleet: SendAcked timeout %v: want 0 (default %v) to %v", timeout, DefaultAckedTimeout, MaxAckedTimeout)
	case timeout == 0:
		timeout = DefaultAckedTimeout
	}
	inner, err := marshalData(data)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	a := &c.channels
	seq := a.seq.next()
	payload := protocol.ChannelPublish{Channel: ch.name, To: to, Data: ackedData(seq, inner)}
	p := &ackedSend{channel: ch.name, to: to, done: make(chan error, 1)}

	a.mu.Lock()
	if a.pending == nil {
		a.pending = map[int64]*ackedSend{}
	}
	a.pending[seq] = p
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pending, seq)
		a.mu.Unlock()
	}()

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	resend := time.NewTimer(time.Hour)
	defer resend.Stop()

	written := 0 // transmissions on the wire so far
	var last time.Time
	for {
		// Taken before the write, so a session that opens after a failed write
		// is never missed.
		opened := a.openSignal()

		wctx, cancel := context.WithTimeout(c.ctx, c.cfg.HandshakeTimeout)
		err := c.Send(wctx, protocol.TypeChannelPublish, payload, WithID(ackedRef(seq, written+1)))
		cancel()
		if err == nil {
			written++
			last = time.Now()
			resend.Reset(c.ackedResend)
		} else {
			// Not connected, or the socket died under the write: paused until
			// the next session opens. The deadline keeps running.
			resend.Stop()
		}

		select {
		case err := <-p.done:
			return err
		case <-resend.C:
		case <-opened:
			// A new session: whatever was sent on the old one may be lost, so
			// send again now, but never sooner than the shortest legal interval
			// after the last copy.
			if wait := minAckedResend - time.Since(last); wait > 0 {
				resend.Reset(wait)
				select {
				case err := <-p.done:
					return err
				case <-resend.C:
				case <-deadline.C:
					return ackedTimeout(ch.name, to, timeout, written)
				case <-ctx.Done():
					return ctx.Err()
				case <-c.ctx.Done():
					return c.Err()
				}
			}
		case <-deadline.C:
			return ackedTimeout(ch.name, to, timeout, written)
		case <-ctx.Done():
			return ctx.Err()
		case <-c.ctx.Done():
			return c.Err()
		}
	}
}

func ackedTimeout(channel, to string, timeout time.Duration, written int) *Error {
	return &Error{Code: CodeTimeout, Message: fmt.Sprintf(
		"acked send on channel %q to %s: no ack within %v (%d sent); it may or may not have been delivered", channel, to, timeout, written)}
}

// ackedData is the acked message as it goes on the wire.
func ackedData(seq int64, inner json.RawMessage) json.RawMessage {
	raw, _ := json.Marshal(struct {
		Seq  int64           `json:"seq"`
		Data json.RawMessage `json:"data"`
	}{seq, inner})
	return raw
}

// openSignal returns a channel that is closed the next time a session opens.
func (a *channels) openSignal() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.open == nil {
		a.open = make(chan struct{})
	}
	return a.open
}

// opened runs from sessionOpened, once Send works on the new session: every
// pending acked send resumes.
func (a *channels) opened() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.open != nil {
		close(a.open)
		a.open = nil
	}
}

// observeAcked runs from route, on the connection goroutine, for every inbound
// envelope in wire order, and resolves the pending acked sends an envelope
// settles. It runs there, ahead of the stream gate and the delivery queue,
// because a SendAcked may be blocking the delivery goroutine (it may be called
// from a handler): its answer must not wait in a queue behind it.
func (c *Client) observeAcked(env protocol.Envelope) {
	a := &c.channels
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.pending) == 0 {
		return
	}
	switch env.Type {
	case protocol.TypeChannelMessage:
		// Acked: an ack on the same channel, from the client the send went to.
		// One that matches no pending send (late, duplicate, from another
		// client, unknown seq) is ignored.
		var msg protocol.ChannelMessage
		if json.Unmarshal(env.Payload, &msg) != nil {
			return
		}
		seq, ok := ackMessage(msg.Data)
		if !ok {
			return
		}
		if p := a.pending[seq]; p != nil && p.channel == msg.Channel && p.to == msg.From {
			a.resolve(seq, nil)
		}

	case protocol.TypeError:
		var msg protocol.ErrorMsg
		if json.Unmarshal(env.Payload, &msg) != nil {
			return
		}
		seq, n, ok := parseAckedRef(msg.Ref)
		p := a.pending[seq]
		if !ok || p == nil {
			return
		}
		what := fmt.Sprintf("acked send on channel %q to %s: ", p.channel, p.to)
		switch msg.Code {
		case protocol.ErrNotFound:
			if n <= 1 {
				what += "target not connected; not delivered"
			} else {
				what += "target not connected; an earlier copy may have been delivered"
			}
			a.resolve(seq, &Error{Code: msg.Code, Message: what})
		case protocol.ErrInvalidMessage, protocol.ErrNotAuthorized:
			a.resolve(seq, &Error{Code: msg.Code, Message: what + msg.Message})
		}
		// rate_limited: that copy was dropped; the next interval re-sends it.
		// Any other code does not settle a send either.
	}
}

// resolve needs a.mu held.
func (a *channels) resolve(seq int64, err error) {
	p := a.pending[seq]
	delete(a.pending, seq)
	p.done <- err
}
