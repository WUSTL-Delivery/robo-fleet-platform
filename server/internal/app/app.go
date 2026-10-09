// Package app wires the modules together: it implements gateway.Auth over the
// store and gateway.Handler over registry/ops/bus/signaling. Message routing
// policy lives here; the modules stay single-purpose.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"
	"unicode/utf8"

	"fleetplatform/sdk/go/protocol"
	"fleetplatform/server/internal/bus"
	"fleetplatform/server/internal/gateway"
	"fleetplatform/server/internal/layers"
	"fleetplatform/server/internal/ops"
	"fleetplatform/server/internal/registry"
	"fleetplatform/server/internal/signaling"
	"fleetplatform/server/internal/store"
)

type Config struct {
	HeartbeatInterval time.Duration
	LeaseTTL          time.Duration
	SweepEvery        time.Duration
	// LayerTTL is how long a service's retained layers outlive its connection.
	// Zero means DefaultLayerTTL.
	LayerTTL time.Duration
}

// DefaultLayerTTL keeps a service's layers across a restart or a Wi-Fi blip.
const DefaultLayerTTL = 5 * time.Minute

// maxHelpReasonLen mirrors help.request's reason maxLength in the schema.
const maxHelpReasonLen = 256

type App struct {
	cfg Config
	st  store.Store
	reg *registry.Registry
	ops *ops.Ops
	bus *bus.Bus
	lay *layers.Store

	// layerMu orders layer fan-out against late-subscriber replay, so a live
	// update can never be followed by an older retained copy.
	layerMu sync.Mutex

	// opMu orders every change to an operator's entry (online, watching)
	// with the event that announces it, and against the snapshot a subscriber
	// is sent, so a subscriber never ends up holding an older entry than the
	// server. It is taken before mu, never while holding it.
	opMu sync.Mutex

	mu    sync.Mutex
	conns map[string]*gateway.Conn // clientID → live conn
	// watching is operator id → the robot that operator's live connection
	// said it is looking at. An operator with no connection has no entry.
	watching map[string]string
}

func New(cfg Config, st store.Store) *App {
	return &App{
		cfg:      cfg,
		st:       st,
		reg:      registry.New(),
		ops:      ops.New(time.Now, cfg.LeaseTTL),
		bus:      bus.New(),
		lay:      layers.New(),
		conns:    make(map[string]*gateway.Conn),
		watching: make(map[string]string),
	}
}

// Gateway returns the WS edge bound to this app.
func (a *App) Gateway() *gateway.Gateway {
	return &gateway.Gateway{
		Auth:                a,
		Handler:             a,
		HeartbeatIntervalMs: int(a.cfg.HeartbeatInterval.Milliseconds()),
	}
}

// Run drives the presence and lease sweeps until ctx is done.
func (a *App) Run(ctx context.Context) {
	t := time.NewTicker(a.cfg.SweepEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			// Presence: heartbeat lapsed → close the socket; OnDisconnect does the
			// rest (offline event, lease revocation) so both paths converge.
			for _, e := range a.reg.SweepStale(now, a.cfg.HeartbeatInterval*5/2) {
				if c := a.conn(e.Client.ID); c != nil {
					// One final error only: a second (conflict) would override
					// rate_limited in the SDKs and turn a retryable lapse terminal.
					a.closeWith(c, protocol.ErrRateLimited, "heartbeat lapsed")
				}
			}
			// Leases: expiry is the server's transition (§7.5), robot → HELP_REQUESTED.
			for _, rv := range a.ops.SweepExpired() {
				a.notifyRevoked(rv)
			}
			ttl := a.cfg.LayerTTL
			if ttl <= 0 {
				ttl = DefaultLayerTTL
			}
			a.lay.Sweep(now, ttl)
		}
	}
}

// --- gateway.Auth ---

func (a *App) Enroll(req protocol.EnrollRequest) (protocol.EnrollResponse, *protocol.ErrorMsg) {
	switch req.Kind {
	case string(store.KindOperator):
		return a.enrollOperator(req)
	case string(store.KindRobot), string(store.KindService):
	default:
		return protocol.EnrollResponse{}, &protocol.ErrorMsg{Code: protocol.ErrInvalidMessage, Message: "kind must be robot, service, or operator"}
	}
	fleetID, ok, err := a.st.AuthEnroll(req.EnrollmentKey)
	if err != nil || !ok {
		return protocol.EnrollResponse{}, &protocol.ErrorMsg{Code: protocol.ErrAuthFailed, Message: "invalid enrollment key"}
	}
	token, client, err := a.st.CreateToken(fleetID, store.Kind(req.Kind), req.Name)
	if err != nil {
		return protocol.EnrollResponse{}, &protocol.ErrorMsg{Code: protocol.ErrInvalidMessage, Message: "enrollment failed"}
	}
	return protocol.EnrollResponse{Token: token, ClientID: client.ID, FleetID: client.FleetID}, nil
}

// enrollOperator redeems a single-use operator invite (DESIGN.md D14). The
// invite rides in enrollment_key; the fleet comes from the invite, not the
// request. A used invite is a conflict (the key was real once); unknown and
// expired invites are plain auth failures.
func (a *App) enrollOperator(req protocol.EnrollRequest) (protocol.EnrollResponse, *protocol.ErrorMsg) {
	token, client, err := a.st.RedeemOperatorInvite(req.EnrollmentKey, req.Name)
	switch {
	case err == nil:
		return protocol.EnrollResponse{Token: token, ClientID: client.ID, FleetID: client.FleetID}, nil
	case errors.Is(err, store.ErrInviteInvalid):
		return protocol.EnrollResponse{}, &protocol.ErrorMsg{Code: protocol.ErrAuthFailed, Message: "invalid invite key"}
	case errors.Is(err, store.ErrInviteExpired):
		return protocol.EnrollResponse{}, &protocol.ErrorMsg{Code: protocol.ErrAuthFailed, Message: "invite key expired"}
	case errors.Is(err, store.ErrInviteUsed):
		return protocol.EnrollResponse{}, &protocol.ErrorMsg{Code: protocol.ErrConflict, Message: "invite key already redeemed"}
	default:
		slog.Error("app: operator invite redemption failed", "err", err)
		return protocol.EnrollResponse{}, &protocol.ErrorMsg{Code: protocol.ErrInvalidMessage, Message: "enrollment failed"}
	}
}

func (a *App) Hello(h protocol.Hello) (store.Client, *protocol.ErrorMsg) {
	client, ok, err := a.st.AuthToken(h.Token)
	if err != nil || !ok {
		return store.Client{}, &protocol.ErrorMsg{Code: protocol.ErrAuthFailed, Message: "invalid token"}
	}
	return client, nil
}

// --- gateway.Handler ---

func (a *App) OnConnect(c *gateway.Conn) {
	if c.Client.Kind == store.KindOperator {
		a.opMu.Lock()
		defer a.opMu.Unlock()
	}
	a.mu.Lock()
	prev := a.conns[c.Client.ID]
	a.conns[c.Client.ID] = c
	// What an operator watches belongs to the connection that said so.
	wasWatching := a.watching[c.Client.ID] != ""
	delete(a.watching, c.Client.ID)
	a.mu.Unlock()
	if prev != nil {
		a.dropConn(prev) // one live conn per identity; newest wins
	}
	a.reg.Up(c.Client, time.Now())
	if c.Client.Kind == store.KindService {
		a.lay.OwnerUp(c.Client.ID)
	}
	switch c.Client.Kind {
	case store.KindRobot:
		a.emit(c.Client.FleetID, bus.TopicPresence, protocol.Event{Event: protocol.EventRobotOnline, RobotID: c.Client.ID})
	case store.KindOperator:
		// A connection that replaces a live one is the same operator still
		// online: no online event. (The replaced conn's OnDisconnect is a no-op
		// too.) The new connection has not said what it watches, so if the old
		// one was watching a robot the fleet is told that it stopped.
		if prev == nil {
			a.emitOperator(protocol.EventOperatorOnline, c.Client, true, "")
		} else if wasWatching {
			a.emitOperator(protocol.EventOperatorWatching, c.Client, true, "")
		}
	}
}

func (a *App) OnDisconnect(c *gateway.Conn) {
	if c.Client.Kind == store.KindOperator {
		a.opMu.Lock()
		defer a.opMu.Unlock()
	}
	a.mu.Lock()
	current := a.conns[c.Client.ID] == c
	if current {
		delete(a.conns, c.Client.ID)
		delete(a.watching, c.Client.ID)
	}
	a.mu.Unlock()
	if !current {
		return // replaced by a newer conn; nothing to tear down
	}
	a.reg.Down(c.Client.ID)
	a.bus.Drop(c.Client.ID)
	switch c.Client.Kind {
	case store.KindRobot:
		a.emit(c.Client.FleetID, bus.TopicPresence, protocol.Event{Event: protocol.EventRobotOffline, RobotID: c.Client.ID})
	case store.KindService:
		a.lay.OwnerDown(c.Client.ID, time.Now())
	case store.KindOperator:
		// Operator loss is the server's transition, never the robot's (§7.5).
		for _, rv := range a.ops.DropOperator(c.Client.ID) {
			a.notifyRevoked(rv)
		}
		// The offline entry carries no watching: this one event clears it.
		a.emitOperator(protocol.EventOperatorOffline, c.Client, false, "")
	}
}

func (a *App) OnMessage(c *gateway.Conn, env protocol.Envelope) {
	if len(env.Payload) > bus.MaxPayloadBytes {
		c.Send(errMsg(protocol.ErrRateLimited, "payload exceeds control-plane limit", env.ID))
		return
	}
	kind := c.Client.Kind
	switch env.Type {
	case protocol.TypeHeartbeat:
		a.reg.Heartbeat(c.Client.ID, time.Now())

	case protocol.TypeManifest:
		if !require(c, env, kind == store.KindRobot) {
			return
		}
		var m protocol.Manifest
		if !parse(c, env, &m) {
			return
		}
		a.reg.SetManifest(c.Client.ID, &m)

	case protocol.TypeTelemetry:
		if !require(c, env, kind == store.KindRobot) {
			return
		}
		a.emit(c.Client.FleetID, bus.TopicTelemetry, protocol.Event{
			Event: protocol.EventRobotTelemetry, RobotID: c.Client.ID, Data: env.Payload,
		})

	case protocol.TypeHelpRequest:
		if !require(c, env, kind == store.KindRobot) {
			return
		}
		var req protocol.HelpRequest
		if !parse(c, env, &req) {
			return
		}
		// The reason is replayed in snapshots, so hold it to the schema's bounds.
		if n := utf8.RuneCountInString(req.Reason); n < 1 || n > maxHelpReasonLen {
			c.Send(errMsg(protocol.ErrInvalidMessage, "help.request reason must be 1 to 256 characters", env.ID))
			return
		}
		// The event carries the stored entry (the request plus requested_at_ms),
		// so a live subscriber can order the queue without a fresh snapshot.
		if help := a.ops.RequestHelp(c.Client.ID, req.Reason, req.Context); help != nil {
			a.emit(c.Client.FleetID, bus.TopicEvents, protocol.Event{
				Event: protocol.EventRobotHelpRequested, RobotID: c.Client.ID, Data: mustJSON(help.Proto()),
			})
		}

	case protocol.TypeSubscribe:
		var sub protocol.Subscribe
		if !parse(c, env, &sub) {
			return
		}
		a.subscribe(c, sub.Topics)

	case protocol.TypeLeaseClaim:
		if !require(c, env, kind == store.KindOperator) {
			return
		}
		var claim protocol.LeaseClaim
		if !parse(c, env, &claim) {
			return
		}
		a.handleClaim(c, env, claim)

	case protocol.TypeLeaseRenew:
		if !require(c, env, kind == store.KindOperator) {
			return
		}
		var renew protocol.LeaseRenew
		if !parse(c, env, &renew) {
			return
		}
		lease, err := a.ops.Renew(renew.LeaseID, c.Client.ID)
		if err != nil {
			c.Send(errMsg(protocol.ErrNotFound, "no such lease held", env.ID))
			return
		}
		c.Send(protocol.Msg(protocol.TypeLeaseGranted, lease.Proto()))

	case protocol.TypeLeaseRelease:
		if !require(c, env, kind == store.KindOperator) {
			return
		}
		var rel protocol.LeaseRelease
		if !parse(c, env, &rel) {
			return
		}
		lease, err := a.ops.Release(rel.LeaseID, c.Client.ID)
		if err != nil {
			c.Send(errMsg(protocol.ErrNotFound, "no such lease held", env.ID))
			return
		}
		revoked := protocol.LeaseRevoked{LeaseID: lease.ID, RobotID: lease.RobotID, Reason: protocol.RevokeReleased}
		if rc := a.conn(lease.RobotID); rc != nil {
			rc.Send(protocol.Msg(protocol.TypeLeaseRevoked, revoked))
		}
		a.emit(c.Client.FleetID, bus.TopicEvents, protocol.Event{
			Event: protocol.EventRobotLeaseReleased, RobotID: lease.RobotID, Data: mustJSON(revoked),
		})

	case protocol.TypeWatch:
		if !require(c, env, kind == store.KindOperator) {
			return
		}
		a.handleWatch(c, env)

	case protocol.TypeTwist:
		if !require(c, env, kind == store.KindOperator) {
			return
		}
		var tw protocol.Twist
		if !parse(c, env, &tw) {
			return
		}
		robotID, ok := a.ops.RobotForLease(tw.LeaseID, c.Client.ID)
		if !ok {
			c.Send(errMsg(protocol.ErrNotAuthorized, "no live lease for twist", env.ID))
			return
		}
		// Leases are only granted inside a fleet (handleClaim); the fleet check
		// here keeps twist inside it even if that ever stops being true.
		if rc := a.conn(robotID); rc != nil && rc.Client.FleetID == c.Client.FleetID {
			rc.Send(env) // control-plane fallback path; the real one is the P2P datachannel
		}

	case protocol.TypeSignal:
		var sig protocol.Signal
		if !parse(c, env, &sig) {
			return
		}
		target, out, err := signaling.Route(sig, c.Client.ID)
		if err != nil {
			c.Send(errMsg(protocol.ErrInvalidMessage, err.Error(), env.ID))
			return
		}
		tc := a.conn(target)
		if tc == nil || tc.Client.FleetID != c.Client.FleetID {
			c.Send(errMsg(protocol.ErrNotFound, "signal target not connected", env.ID))
			return
		}
		tc.Send(protocol.Msg(protocol.TypeSignal, out))

	case protocol.TypeChannelPublish:
		var pub protocol.ChannelPublish
		if !parse(c, env, &pub) {
			return
		}
		a.handlePublish(c, env, pub)

	case protocol.TypeLayerDeclare, protocol.TypeLayerUpdate:
		if !require(c, env, kind == store.KindService) {
			return
		}
		a.handleLayer(c, env)

	default:
		c.Send(errMsg(protocol.ErrInvalidMessage, "unknown message type "+env.Type, env.ID))
	}
}

// subscribe is snapshot-then-stream; a `layers` subscriber also gets every
// retained layer (declare, then latest update) right after the snapshot.
func (a *App) subscribe(c *gateway.Conn, topics []string) {
	wantsLayers := false
	for _, t := range topics {
		if t == bus.TopicLayers {
			wantsLayers = true
		}
	}
	if wantsLayers {
		a.layerMu.Lock()
		defer a.layerMu.Unlock()
	}
	a.opMu.Lock()
	a.bus.Subscribe(c.Client.ID, topics)
	c.Send(protocol.Msg(protocol.TypeSnapshot, a.snapshot(c.Client.FleetID)))
	a.opMu.Unlock()
	if wantsLayers {
		for _, env := range a.lay.Replay(c.Client.FleetID) {
			c.Send(env)
		}
	}
}

// handleLayer retains a layer message and fans it out live. The payload stays
// opaque beyond its layer_id (D4).
func (a *App) handleLayer(c *gateway.Conn, env protocol.Envelope) {
	var ref struct {
		LayerID string `json:"layer_id"`
	}
	if !parse(c, env, &ref) {
		return
	}
	if ref.LayerID == "" {
		c.Send(errMsg(protocol.ErrInvalidMessage, env.Type+" needs layer_id", env.ID))
		return
	}
	a.layerMu.Lock()
	defer a.layerMu.Unlock()
	if env.Type == protocol.TypeLayerDeclare {
		a.lay.Declare(c.Client.FleetID, c.Client.ID, ref.LayerID, env)
	} else {
		a.lay.Update(c.Client.FleetID, c.Client.ID, ref.LayerID, env)
	}
	a.fanout(c.Client.FleetID, bus.TopicLayers, env)
}

func (a *App) handleClaim(c *gateway.Conn, env protocol.Envelope, claim protocol.LeaseClaim) {
	// The target must be an online robot in the caller's fleet. Another fleet's
	// robot, or a client that is not a robot, gets the same answer as an id
	// that does not exist, so ids do not leak across fleets.
	e, online := a.reg.Get(claim.RobotID)
	if !online || e.Client.Kind != store.KindRobot || e.Client.FleetID != c.Client.FleetID {
		c.Send(errMsg(protocol.ErrNotFound, "robot not online", env.ID))
		return
	}
	// Whether the robot is free, and who wins two claims that cross, is decided
	// inside ops under its lock. A refusal changes nothing and tells nobody but
	// the caller. It names the lease in the way, which the caller could already
	// read from its own fleet's snapshot, so a console can say who is driving.
	lease, stolen, err := a.ops.Claim(claim.RobotID, c.Client.ID, claim.Steal)
	if err != nil {
		held := lease.Proto()
		c.Send(protocol.Msg(protocol.TypeError, protocol.ErrorMsg{
			Code: protocol.ErrConflict, Message: "robot is leased to another operator", Ref: env.ID, Lease: &held,
		}))
		return
	}
	granted := protocol.Msg(protocol.TypeLeaseGranted, lease.Proto())
	c.Send(granted)
	if rc := a.conn(claim.RobotID); rc != nil {
		rc.Send(granted)
	}
	if stolen != nil {
		a.notifyOperator(stolen.Lease.OperatorID, *stolen)
		a.emit(c.Client.FleetID, bus.TopicEvents, protocol.Event{
			Event: protocol.EventRobotLeaseRevoked, RobotID: claim.RobotID,
			Data: mustJSON(protocol.LeaseRevoked{LeaseID: stolen.Lease.ID, RobotID: claim.RobotID, Reason: stolen.Reason}),
		})
	}
	a.emit(c.Client.FleetID, bus.TopicEvents, protocol.Event{
		Event: protocol.EventRobotLeaseGranted, RobotID: claim.RobotID, Data: mustJSON(lease.Proto()),
	})
}

// handleWatch records which robot an operator is looking at and tells the
// fleet's presence subscribers when that changed. It never replies on success.
func (a *App) handleWatch(c *gateway.Conn, env protocol.Envelope) {
	// robot_id must be present: a string names a robot, null stops watching.
	var w struct {
		RobotID json.RawMessage `json:"robot_id"`
	}
	if !parse(c, env, &w) {
		return
	}
	robotID := ""
	if string(w.RobotID) != "null" {
		if json.Unmarshal(w.RobotID, &robotID) != nil || robotID == "" {
			c.Send(errMsg(protocol.ErrInvalidMessage, "watch needs robot_id: a robot id or null", env.ID))
			return
		}
	}
	a.opMu.Lock()
	defer a.opMu.Unlock()
	// The target must be a robot of the caller's fleet, online or not. Another
	// fleet's robot, a client that is not a robot and a revoked robot all get
	// the same answer as an id that does not exist, and nobody else hears of it.
	if robotID != "" && !a.robotInFleet(c.Client.FleetID, robotID) {
		c.Send(errMsg(protocol.ErrNotFound, "no such robot", env.ID))
		return
	}
	a.mu.Lock()
	// A connection that has been replaced no longer speaks for the operator.
	changed := a.conns[c.Client.ID] == c && a.watching[c.Client.ID] != robotID
	if changed && robotID != "" {
		a.watching[c.Client.ID] = robotID
	} else if changed {
		delete(a.watching, c.Client.ID)
	}
	a.mu.Unlock()
	// Saying again what the server already has is not news.
	if changed {
		a.emitOperator(protocol.EventOperatorWatching, c.Client, true, robotID)
	}
}

// robotInFleet reports whether id is a robot of the fleet that is not revoked:
// exactly the robots the fleet's snapshot lists.
func (a *App) robotInFleet(fleetID, id string) bool {
	robots, err := a.st.RobotsInFleet(fleetID)
	if err != nil {
		slog.Error("app: watch robot lookup failed", "err", err)
		return false
	}
	for _, r := range robots {
		if r.ID == id {
			return true
		}
	}
	return false
}

// unwatchRobot stops every operator watching the robot and announces each.
func (a *App) unwatchRobot(robotID string) {
	a.opMu.Lock()
	defer a.opMu.Unlock()
	var watchers []store.Client
	a.mu.Lock()
	for opID, watched := range a.watching {
		if watched != robotID {
			continue
		}
		delete(a.watching, opID)
		if oc := a.conns[opID]; oc != nil {
			watchers = append(watchers, oc.Client)
		}
	}
	a.mu.Unlock()
	for _, op := range watchers {
		a.emitOperator(protocol.EventOperatorWatching, op, true, "")
	}
}

func (a *App) handlePublish(c *gateway.Conn, env protocol.Envelope, pub protocol.ChannelPublish) {
	if pub.Channel == "" || pub.Data == nil || (pub.To == "" && !pub.Broadcast) {
		c.Send(errMsg(protocol.ErrInvalidMessage, "channel.publish needs channel, data, and to|broadcast", env.ID))
		return
	}
	msg := protocol.Msg(protocol.TypeChannelMessage, protocol.ChannelMessage{
		Channel: pub.Channel, From: c.Client.ID, Data: pub.Data,
	})
	if pub.To != "" {
		tc := a.conn(pub.To)
		if tc == nil || tc.Client.FleetID != c.Client.FleetID {
			c.Send(errMsg(protocol.ErrNotFound, "channel target not connected", env.ID))
			return
		}
		tc.Send(msg)
		return
	}
	// Broadcast: robots that declared the channel in their manifest + explicit
	// topic subscribers. At-most-once by design (D8) — no queues, no retries.
	delivered := map[string]bool{c.Client.ID: true}
	for _, id := range a.bus.SubscribersOf(bus.ChannelPrefix + pub.Channel) {
		if tc := a.conn(id); tc != nil && tc.Client.FleetID == c.Client.FleetID && !delivered[id] {
			tc.Send(msg)
			delivered[id] = true
		}
	}
	a.mu.Lock()
	conns := make([]*gateway.Conn, 0, len(a.conns))
	for _, tc := range a.conns {
		conns = append(conns, tc)
	}
	a.mu.Unlock()
	for _, tc := range conns {
		if tc.Client.Kind != store.KindRobot || tc.Client.FleetID != c.Client.FleetID || delivered[tc.Client.ID] {
			continue
		}
		if e, ok := a.reg.Get(tc.Client.ID); ok && e.Manifest != nil {
			for _, ch := range e.Manifest.Channels {
				if ch == pub.Channel {
					tc.Send(msg)
					delivered[tc.Client.ID] = true
					break
				}
			}
		}
	}
}

// snapshot composes fleet state for snapshot-then-stream: store (known robots) ×
// registry (presence, manifests) × ops (FSM, leases).
func (a *App) snapshot(fleetID string) protocol.Snapshot {
	robots, err := a.st.RobotsInFleet(fleetID)
	if err != nil {
		slog.Error("app: snapshot query failed", "err", err)
	}
	snap := protocol.Snapshot{Robots: []protocol.RobotSummary{}, Operators: []protocol.OperatorSummary{}}
	operators, err := a.st.OperatorsInFleet(fleetID)
	if err != nil {
		slog.Error("app: snapshot operator query failed", "err", err)
	}
	a.mu.Lock()
	watching := make(map[string]string, len(a.watching))
	for opID, robotID := range a.watching {
		watching[opID] = robotID
	}
	a.mu.Unlock()
	for _, o := range operators {
		_, online := a.reg.Get(o.ID)
		snap.Operators = append(snap.Operators, operatorSummary(o, online, watching[o.ID]))
	}
	for _, r := range robots {
		sum := protocol.RobotSummary{RobotID: r.ID, Name: r.Name, Presence: "offline"}
		if e, ok := a.reg.Get(r.ID); ok {
			sum.Presence = "online"
			sum.Manifest = e.Manifest
		}
		state, lease, help := a.ops.StateOf(r.ID)
		sum.State = state
		if lease != nil {
			lp := lease.Proto()
			sum.Lease = &lp
		}
		if help != nil {
			hp := help.Proto()
			sum.Help = &hp
		}
		snap.Robots = append(snap.Robots, sum)
	}
	return snap
}

// operatorSummary is an operator's entry in the snapshot and the data of its
// presence events: one shape in both places, so a subscriber can upsert it.
// An offline operator watches nothing.
func operatorSummary(c store.Client, online bool, watching string) protocol.OperatorSummary {
	sum := protocol.OperatorSummary{OperatorID: c.ID, Name: c.Name, Online: online}
	if online {
		sum.Watching = watching
	}
	return sum
}

// emitOperator announces a change to an operator's entry (operator.online,
// operator.offline, operator.watching) to the presence subscribers of its own
// fleet. The caller holds opMu.
func (a *App) emitOperator(event string, c store.Client, online bool, watching string) {
	a.emit(c.FleetID, bus.TopicPresence, protocol.Event{
		Event: event, OperatorID: c.ID, Data: mustJSON(operatorSummary(c, online, watching)),
	})
}

// revokedMsg is the wire form of a revocation; it carries the queue entry when
// the robot went back to HELP_REQUESTED.
func revokedMsg(rv ops.Revoked) protocol.LeaseRevoked {
	m := protocol.LeaseRevoked{LeaseID: rv.Lease.ID, RobotID: rv.Lease.RobotID, Reason: rv.Reason}
	if rv.Help != nil {
		hp := rv.Help.Proto()
		m.Help = &hp
	}
	return m
}

func (a *App) notifyRevoked(rv ops.Revoked) {
	revoked := revokedMsg(rv)
	if rc := a.conn(rv.Lease.RobotID); rc != nil {
		rc.Send(protocol.Msg(protocol.TypeLeaseRevoked, revoked))
		a.emit(rc.Client.FleetID, bus.TopicEvents, protocol.Event{
			Event: protocol.EventRobotLeaseRevoked, RobotID: rv.Lease.RobotID, Data: mustJSON(revoked),
		})
	}
	a.notifyOperator(rv.Lease.OperatorID, rv)
}

func (a *App) notifyOperator(operatorID string, rv ops.Revoked) {
	if oc := a.conn(operatorID); oc != nil {
		oc.Send(protocol.Msg(protocol.TypeLeaseRevoked, revokedMsg(rv)))
	}
}

// emit fans an event out to a topic's subscribers within one fleet.
func (a *App) emit(fleetID, topic string, ev protocol.Event) {
	a.fanout(fleetID, topic, protocol.Msg(protocol.TypeEvent, ev))
}

func (a *App) fanout(fleetID, topic string, env protocol.Envelope) {
	for _, id := range a.bus.SubscribersOf(topic) {
		if c := a.conn(id); c != nil && c.Client.FleetID == fleetID {
			c.Send(env)
		}
	}
}

func (a *App) conn(clientID string) *gateway.Conn {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conns[clientID]
}

// DisconnectClient closes the client's live connection, if it has one, after
// telling it why with error{code: auth_failed}. The admin API calls it right
// after revoking the client's token: auth_failed is the same code the next
// hello will get, so the SDKs stop instead of reconnecting. Teardown runs
// through OnDisconnect like any other drop (robot.offline, operator leases
// revoked). Reports whether a live connection was closed.
//
// A revoked robot is gone from the fleet, so nobody is watching it any more.
func (a *App) DisconnectClient(clientID string) bool {
	a.unwatchRobot(clientID)
	c := a.conn(clientID)
	if c == nil {
		return false
	}
	a.closeWith(c, protocol.ErrAuthFailed, "token revoked")
	return true
}

func (a *App) dropConn(c *gateway.Conn) {
	a.closeWith(c, protocol.ErrConflict, "connection superseded or expired")
}

// closeWith sends a final error, then closes the socket. Closing makes the
// read loop return, which runs OnDisconnect.
func (a *App) closeWith(c *gateway.Conn, code, message string) {
	c.Send(protocol.Msg(protocol.TypeError, protocol.ErrorMsg{Code: code, Message: message}))
	go func() {
		time.Sleep(100 * time.Millisecond) // let the notice flush
		c.Close()
	}()
}

func require(c *gateway.Conn, env protocol.Envelope, ok bool) bool {
	if !ok {
		c.Send(errMsg(protocol.ErrNotAuthorized, "message not allowed for client kind", env.ID))
	}
	return ok
}

func parse[T any](c *gateway.Conn, env protocol.Envelope, out *T) bool {
	if err := json.Unmarshal(env.Payload, out); err != nil {
		c.Send(errMsg(protocol.ErrInvalidMessage, "malformed "+env.Type+" payload", env.ID))
		return false
	}
	return true
}

func errMsg(code, message, ref string) protocol.Envelope {
	return protocol.Msg(protocol.TypeError, protocol.ErrorMsg{Code: code, Message: message, Ref: ref})
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
