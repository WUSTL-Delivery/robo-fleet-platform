package fleet

// Snapshot-then-stream (docs/INTEGRATION.md section 2.5): Subscribe, and typed
// callbacks for what a subscription delivers.
//
//	client.OnSnapshot(world.Reset)      every snapshot, including after a reconnect
//	client.OnPresence(world.SetOnline)  robot.online / robot.offline
//	client.OnLease(world.ApplyLease)    robot.lease_granted / _released / _revoked
//	client.OnHelp(world.Enqueue)        a robot entered the intervention queue
//	client.OnTelemetry(world.SetPose)   robot.telemetry
//	client.OnEvent(log)                 every event, whatever its name
//	client.Subscribe(ctx, fleet.TopicPresence, fleet.TopicEvents, fleet.TopicTelemetry)
//
// # Reconnects
//
// Topics are remembered. On every reconnect the client subscribes to all of
// them again before anything else happens on the new connection, and the
// server answers with a fresh snapshot. A world model stays correct by
// replacing itself from each snapshot and then applying events.
//
// # Ordering
//
// While a snapshot is outstanding (after Subscribe, and after every
// reconnect), stream envelopes are held back: event, channel.message,
// layer.declare and layer.update. They are delivered, in wire order, right
// after the snapshot. So the first stream delivery of a connection is always
// its snapshot, and no event is applied to a model the next snapshot is about
// to replace. Held envelopes of a connection that dies first are dropped; the
// next snapshot supersedes them. The raw On and OnMessage handlers see the
// same gated order. Envelopes of other types (lease.granted, signal, error,
// ...) are replies or directed messages and are never held.
//
// Events that happened while the client was disconnected are not replayed.
// Their effect is in the next snapshot.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"

	"github.com/coder/websocket"

	"fleetplatform/sdk/go/protocol"
)

// Topics for Subscribe.
const (
	// TopicPresence: robot.online and robot.offline.
	TopicPresence = "presence"
	// TopicEvents: robot.help_requested and the robot.lease_* events.
	TopicEvents = "events"
	// TopicTelemetry: robot.telemetry for every robot in the fleet.
	TopicTelemetry = "telemetry"
	// TopicLayers: layer.declare and layer.update from services in the fleet.
	TopicLayers = "layers"
)

// topicPattern is the subscribe schema's bound on one topic
// (protocol/schemas/ops.schema.json).
var topicPattern = regexp.MustCompile(`^[a-z][a-z0-9_.:-]{0,79}$`)

// ChannelTopic is the topic that carries broadcasts on one domain channel.
func ChannelTopic(channel string) string { return "channel:" + channel }

// The wire types the callbacks hand out, under their SDK names.
type (
	Snapshot     = protocol.Snapshot
	RobotSummary = protocol.RobotSummary
	Event        = protocol.Event
	Telemetry    = protocol.Telemetry
	Lease        = protocol.Lease
	HelpDetails  = protocol.HelpDetails
)

// LeaseChange is one robot.lease_granted, robot.lease_released or
// robot.lease_revoked event.
type LeaseChange struct {
	RobotID string
	// Event is protocol.EventRobotLeaseGranted, EventRobotLeaseReleased or
	// EventRobotLeaseRevoked.
	Event string
	// State is the robot's FSM state after this change: protocol.StateTeleop
	// after a grant and after a steal (reason "stolen": the next grant follows),
	// StateAutonomous after a release, StateHelpRequested after a revocation
	// that put the robot back in the queue. It is "" when the event does not
	// say (a revocation reason this SDK does not know).
	State string
	// Lease is the new lease. Granted only.
	Lease *Lease
	// LeaseID is the lease this change is about.
	LeaseID string
	// Reason is a protocol.Revoke* value. Released and revoked only.
	Reason string
	// Help is the robot's queue entry when the revocation put it back in the
	// queue (reasons "expired" and "operator_lost"). OnHelp handlers get it too.
	Help *HelpDetails
}

// Subscribe adds topics to this client's subscription and returns the snapshot
// that answers it. Subscriptions are additive and remembered: after every
// reconnect the client subscribes to all of them again, and OnSnapshot
// handlers get the fresh snapshot before any further event.
//
// Register the callbacks before calling Subscribe. Subscribe returns as soon as
// the snapshot is read, which can be before the OnSnapshot handlers have run
// for it, so it is safe to call from a handler.
//
// Topics are the Topic* constants and ChannelTopic names; one that the
// protocol's topic syntax rules out is an error before anything is sent.
//
// Called while the client is reconnecting, Subscribe waits and returns the
// next connection's snapshot. It returns an *Error with the server's code if
// the server refuses the subscribe (the topics this call added are then
// forgotten), c.Err() if the client closes first, and ctx.Err() if ctx ends
// first. A ctx that ends does not undo the subscription.
//
// Do not also send subscribe envelopes with Send: their snapshots would be
// taken for the answers to Subscribe calls.
func (c *Client) Subscribe(ctx context.Context, topics ...string) (Snapshot, error) {
	if len(topics) == 0 {
		return Snapshot{}, errors.New("fleet: Subscribe needs at least one topic")
	}
	for _, t := range topics {
		if !topicPattern.MatchString(t) {
			return Snapshot{}, fmt.Errorf("fleet: Subscribe: %q is not a valid topic", t)
		}
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	call := &subCall{done: make(chan struct{})}
	s := &c.stream

	s.send.Lock()
	if c.ctx.Err() != nil {
		s.send.Unlock()
		return Snapshot{}, c.Err()
	}
	s.mu.Lock()
	for _, t := range topics {
		if !slices.Contains(s.topics, t) {
			s.topics = append(s.topics, t)
			call.added = append(call.added, t)
		}
	}
	conn := s.conn
	id := ""
	if conn != nil {
		id = s.nextID()
		s.inflight = append(s.inflight, &subSend{id: id, calls: []*subCall{call}})
	} else {
		s.deferred = append(s.deferred, call)
	}
	s.mu.Unlock()
	if conn != nil {
		c.writeSubscribe(conn, id, topics)
	}
	s.send.Unlock()

	select {
	case <-call.done:
		return call.snap, call.err
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	case <-c.ctx.Done():
		return Snapshot{}, c.Err()
	}
}

// OnSnapshot registers a handler for every snapshot: the answer to each
// Subscribe and the fresh one after each reconnect. The returned function
// removes it.
func (c *Client) OnSnapshot(handler func(Snapshot)) (remove func()) {
	return addHandler(c, &c.stream.onSnapshot, handler)
}

// OnEvent registers a handler for every event envelope, including ones whose
// name this SDK has no typed callback for. It runs after the typed callbacks
// for the same event. The returned function removes it.
func (c *Client) OnEvent(handler func(Event)) (remove func()) {
	return addHandler(c, &c.stream.onEvent, handler)
}

// OnPresence registers a handler for robot.online (online true) and
// robot.offline (topic TopicPresence). The returned function removes it.
func (c *Client) OnPresence(handler func(robotID string, online bool)) (remove func()) {
	return addHandler(c, &c.stream.onPresence, func(a robotArg[bool]) { handler(a.robotID, a.v) })
}

// OnLease registers a handler for the robot.lease_* events (topic
// TopicEvents). The returned function removes it.
func (c *Client) OnLease(handler func(LeaseChange)) (remove func()) {
	return addHandler(c, &c.stream.onLease, handler)
}

// OnHelp registers a handler called whenever a robot enters the intervention
// queue (topic TopicEvents): on robot.help_requested, and on a
// robot.lease_revoked that put the robot back in the queue, after the OnLease
// handlers for that event. A robot that comes back keeps its original
// RequestedAtMs. The returned function removes it.
func (c *Client) OnHelp(handler func(robotID string, help HelpDetails)) (remove func()) {
	return addHandler(c, &c.stream.onHelp, func(a robotArg[HelpDetails]) { handler(a.robotID, a.v) })
}

// OnTelemetry registers a handler for robot.telemetry (topic TopicTelemetry).
// The returned function removes it.
func (c *Client) OnTelemetry(handler func(robotID string, t Telemetry)) (remove func()) {
	return addHandler(c, &c.stream.onTelemetry, func(a robotArg[Telemetry]) { handler(a.robotID, a.v) })
}

// ---------------------------------------------------------------- internals

// stream is the snapshot-then-stream state of one client.
type stream struct {
	// send serializes "record an in-flight subscribe, then write it", so that
	// inflight is in wire order, which is the order the snapshots come back in.
	send sync.Mutex

	mu     sync.Mutex
	conn   *websocket.Conn // the open session's socket; nil between sessions
	topics []string        // everything subscribed so far, re-sent on reconnect
	seq    int
	// inflight: subscribes written on conn and not answered yet, in wire order.
	// Each ends in exactly one way: its snapshot, an error naming its id, or the
	// loss of the session.
	inflight []*subSend
	// deferred: Subscribe calls waiting for the next session's snapshot.
	deferred []*subCall
	// held: stream envelopes read while inflight is not empty.
	held []protocol.Envelope

	// Handler lists, guarded by Client.mu like the core's.
	onSnapshot  []entry[Snapshot]
	onEvent     []entry[Event]
	onPresence  []entry[robotArg[bool]]
	onLease     []entry[LeaseChange]
	onHelp      []entry[robotArg[HelpDetails]]
	onTelemetry []entry[robotArg[Telemetry]]
}

// subSend is one subscribe envelope on the wire and the calls it answers.
type subSend struct {
	id    string
	calls []*subCall
}

// subCall is one Subscribe call waiting for its snapshot.
type subCall struct {
	// added: the topics this call introduced, forgotten again if it is refused.
	added []string
	done  chan struct{} // closed once snap and err are set
	snap  Snapshot
	err   error
}

func (p *subSend) finish(snap Snapshot, err error) {
	for _, call := range p.calls {
		call.snap, call.err = snap, err
		close(call.done)
	}
}

// robotArg carries a (robot id, value) callback's arguments.
type robotArg[T any] struct {
	robotID string
	v       T
}

func addHandler[T any](c *Client, list *[]entry[T], fn func(T)) (remove func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id := c.handlerID()
	*list = append(*list, entry[T]{id, fn})
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		*list = without(*list, id)
	}
}

// nextID needs s.mu held.
func (s *stream) nextID() string {
	s.seq++
	return fmt.Sprintf("sub-%d", s.seq)
}

// writeSubscribe writes one subscribe envelope; the caller holds stream.send
// and has already recorded it in inflight. If the write fails the socket is
// closed, so the session ends and lost() moves the calls to the next one: an
// in-flight subscribe is never left on a live socket without an answer coming.
func (c *Client) writeSubscribe(conn *websocket.Conn, id string, topics []string) {
	ctx, cancel := context.WithTimeout(c.ctx, c.cfg.HandshakeTimeout)
	defer cancel()
	env := protocol.Msg(protocol.TypeSubscribe, protocol.Subscribe{Topics: topics})
	env.ID = id
	if writeEnvelope(ctx, conn, env) != nil {
		conn.CloseNow()
	}
}

// opened runs from sessionOpened: adopt the new socket and subscribe to every
// remembered topic again. Subscribe calls that were waiting for a connection
// are answered by this subscribe's snapshot.
func (s *stream) opened(c *Client, conn *websocket.Conn) {
	s.send.Lock()
	defer s.send.Unlock()
	s.mu.Lock()
	s.conn = conn
	topics := slices.Clone(s.topics)
	id := ""
	if len(topics) > 0 {
		id = s.nextID()
		s.inflight = append(s.inflight, &subSend{id: id, calls: s.deferred})
		s.deferred = nil
	}
	s.mu.Unlock()
	if id != "" {
		c.writeSubscribe(conn, id, topics)
	}
}

// lost runs from sessionLost: unanswered subscribes wait for the next session,
// and held envelopes are dropped because its snapshot supersedes them.
func (s *stream) lost() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conn = nil
	for _, p := range s.inflight {
		s.deferred = append(s.deferred, p.calls...)
	}
	s.inflight = nil
	s.held = nil
}

// gate runs from route, on the connection goroutine, for every inbound
// envelope in wire order. It returns the envelopes to hand to the handlers
// now, in order: none while env is held, env alone, or env followed by
// everything that was held once the last outstanding snapshot is answered.
func (s *stream) gate(env protocol.Envelope) []protocol.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch env.Type {
	case protocol.TypeSnapshot:
		if len(s.inflight) > 0 {
			p := s.inflight[0]
			s.inflight = s.inflight[1:]
			// Decoded here only for waiting calls; the handlers decode their own.
			if len(p.calls) > 0 {
				var snap Snapshot
				if json.Unmarshal(env.Payload, &snap) != nil {
					p.finish(Snapshot{}, &Error{Code: CodeProtocol, Message: "subscribe: malformed snapshot"})
				} else {
					p.finish(snap, nil)
				}
			}
		}
		return s.release(env)

	case protocol.TypeError:
		var msg protocol.ErrorMsg
		if len(s.inflight) == 0 || json.Unmarshal(env.Payload, &msg) != nil || msg.Ref == "" {
			break
		}
		i := slices.IndexFunc(s.inflight, func(p *subSend) bool { return p.id == msg.Ref })
		if i < 0 {
			break
		}
		p := s.inflight[i]
		s.inflight = slices.Delete(s.inflight, i, i+1)
		// Refused: forget the topics these calls introduced, or every
		// reconnect would send them again and be refused again.
		for _, call := range p.calls {
			s.topics = slices.DeleteFunc(s.topics, func(t string) bool { return slices.Contains(call.added, t) })
		}
		p.finish(Snapshot{}, &Error{Code: msg.Code, Message: "subscribe: " + msg.Message})
		return s.release(env)

	case protocol.TypeEvent, protocol.TypeChannelMessage, protocol.TypeLayerDeclare, protocol.TypeLayerUpdate:
		if len(s.inflight) > 0 {
			s.held = append(s.held, env)
			return nil
		}
	}
	return []protocol.Envelope{env}
}

// release needs s.mu held.
func (s *stream) release(env protocol.Envelope) []protocol.Envelope {
	out := []protocol.Envelope{env}
	if len(s.inflight) == 0 {
		out = append(out, s.held...)
		s.held = nil
	}
	return out
}

// dispatchStream calls the typed callbacks for one envelope; delivery
// goroutine only. A payload that does not decode is skipped, never an error:
// the raw handlers have already seen the envelope.
func (c *Client) dispatchStream(env protocol.Envelope) {
	if env.Type != protocol.TypeSnapshot && env.Type != protocol.TypeEvent {
		return
	}
	s := &c.stream
	c.mu.Lock()
	onSnapshot, onEvent := s.onSnapshot, s.onEvent
	onPresence, onLease, onHelp, onTelemetry := s.onPresence, s.onLease, s.onHelp, s.onTelemetry
	c.mu.Unlock()

	if env.Type == protocol.TypeSnapshot {
		var snap Snapshot
		if len(onSnapshot) == 0 || json.Unmarshal(env.Payload, &snap) != nil {
			return
		}
		call(onSnapshot, snap)
		return
	}

	var ev Event
	if json.Unmarshal(env.Payload, &ev) != nil || ev.Event == "" {
		return
	}
	// A typed callback for a new event name is one more case here (plus its
	// handler list and On* method). A name with no case is not an error: it
	// still reaches the OnEvent handlers below.
	switch ev.Event {
	case protocol.EventRobotOnline, protocol.EventRobotOffline:
		call(onPresence, robotArg[bool]{ev.RobotID, ev.Event == protocol.EventRobotOnline})

	case protocol.EventRobotTelemetry:
		var t Telemetry
		if len(onTelemetry) > 0 && json.Unmarshal(ev.Data, &t) == nil {
			call(onTelemetry, robotArg[Telemetry]{ev.RobotID, t})
		}

	case protocol.EventRobotHelpRequested:
		var help HelpDetails
		if json.Unmarshal(ev.Data, &help) == nil {
			call(onHelp, robotArg[HelpDetails]{ev.RobotID, help})
		}

	case protocol.EventRobotLeaseGranted:
		var lease Lease
		if json.Unmarshal(ev.Data, &lease) == nil {
			call(onLease, LeaseChange{
				RobotID: ev.RobotID, Event: ev.Event, State: protocol.StateTeleop,
				Lease: &lease, LeaseID: lease.LeaseID,
			})
		}

	case protocol.EventRobotLeaseReleased, protocol.EventRobotLeaseRevoked:
		var rv protocol.LeaseRevoked
		if json.Unmarshal(ev.Data, &rv) != nil {
			break
		}
		ch := LeaseChange{RobotID: ev.RobotID, Event: ev.Event, LeaseID: rv.LeaseID, Reason: rv.Reason, Help: rv.Help}
		switch {
		case ev.Event == protocol.EventRobotLeaseReleased:
			ch.State = protocol.StateAutonomous
		case rv.Help != nil:
			ch.State = protocol.StateHelpRequested
		case rv.Reason == protocol.RevokeStolen:
			ch.State = protocol.StateTeleop
		}
		call(onLease, ch)
		if rv.Help != nil {
			call(onHelp, robotArg[HelpDetails]{ev.RobotID, *rv.Help})
		}
	}
	call(onEvent, ev)
}

func call[T any](handlers []entry[T], v T) {
	for _, h := range handlers {
		h.fn(v)
	}
}
