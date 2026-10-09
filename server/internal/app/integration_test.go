// Layer-3 integration tests (docs/TESTING.md): the real server — gateway, app,
// sqlite store — on an ephemeral port, with fake clients speaking raw protocol
// JSON over real WebSockets. Once sdk/typescript exists, the same storyline runs
// there through the public SDK surface.
package app_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"fleetplatform/sdk/go/protocol"
	"fleetplatform/server/internal/app"
	"fleetplatform/server/internal/store"
	"fleetplatform/server/internal/web"
)

type harness struct {
	t     *testing.T
	url   string
	store store.Store
	fleet store.Fleet
}

func newHarness(t *testing.T, cfg app.Config) *harness {
	t.Helper()
	st, err := store.OpenSqlite(filepath.Join(t.TempDir(), "fleet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	fleet, err := st.CreateFleet("test-fleet")
	if err != nil {
		t.Fatal(err)
	}

	a := app.New(cfg, st)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go a.Run(ctx)

	srv := httptest.NewServer(web.Handler(a.Gateway()))
	t.Cleanup(srv.Close)

	return &harness{
		t:     t,
		url:   "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws",
		store: st,
		fleet: fleet,
	}
}

func defaultConfig() app.Config {
	return app.Config{
		// Heartbeats deliberately lax: these tests exercise flows, not expiry.
		HeartbeatInterval: time.Minute,
		LeaseTTL:          30 * time.Second,
		SweepEvery:        25 * time.Millisecond,
	}
}

// client is a fake wire client — the proto-SDK.
type client struct {
	t  *testing.T
	ws *websocket.Conn
}

func (h *harness) dial() *client {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, h.url, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { ws.CloseNow() })
	return &client{t: h.t, ws: ws}
}

// connect dials + hellos and consumes the welcome.
func (h *harness) connect(token string) *client {
	h.t.Helper()
	c := h.dial()
	c.send(protocol.TypeHello, protocol.Hello{Token: token})
	c.expect(protocol.TypeWelcome)
	return c
}

func (c *client) send(typ string, payload any) {
	c.t.Helper()
	data, err := json.Marshal(protocol.Msg(typ, payload))
	if err != nil {
		c.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, data); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) recv() (protocol.Envelope, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.ws.Read(ctx)
	if err != nil {
		return protocol.Envelope{}, err
	}
	var env protocol.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return protocol.Envelope{}, err
	}
	return env, nil
}

// expect reads until an envelope of the wanted type arrives, skipping others
// (event streams interleave); an error envelope fails the test unless expected.
func (c *client) expect(typ string) protocol.Envelope {
	c.t.Helper()
	for i := 0; i < 50; i++ {
		env, err := c.recv()
		if err != nil {
			c.t.Fatalf("waiting for %q: %v", typ, err)
		}
		if env.Type == typ {
			return env
		}
		if env.Type == protocol.TypeError {
			c.t.Fatalf("waiting for %q, got error: %s", typ, env.Payload)
		}
	}
	c.t.Fatalf("gave up waiting for %q", typ)
	return protocol.Envelope{}
}

// expectEvent reads events until the named one arrives.
func (c *client) expectEvent(name string) protocol.Event {
	c.t.Helper()
	for i := 0; i < 50; i++ {
		env := c.expect(protocol.TypeEvent)
		var ev protocol.Event
		mustUnmarshal(c.t, env.Payload, &ev)
		if ev.Event == name {
			return ev
		}
	}
	c.t.Fatalf("gave up waiting for event %q", name)
	return protocol.Event{}
}

func mustUnmarshal[T any](t *testing.T, raw json.RawMessage, out *T) {
	t.Helper()
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
}

// TestIntegrationStoryline is the canonical end-to-end flow: enroll → connect →
// manifest → snapshot → help → claim → twist + signaling → handback → channels
// → offline.
func TestIntegrationStoryline(t *testing.T) {
	h := newHarness(t, defaultConfig())

	enrollKey, err := h.store.CreateEnrollKey(h.fleet.ID)
	if err != nil {
		t.Fatal(err)
	}
	opToken, _, err := h.store.CreateToken(h.fleet.ID, store.KindOperator, "op-1")
	if err != nil {
		t.Fatal(err)
	}
	svcToken, _, err := h.store.CreateToken(h.fleet.ID, store.KindService, "brain")
	if err != nil {
		t.Fatal(err)
	}

	// 1. Robot enrolls with the fleet key and receives its per-robot token.
	ec := h.dial()
	ec.send(protocol.TypeEnrollRequest, protocol.EnrollRequest{
		EnrollmentKey: enrollKey, Kind: "robot", Name: "sim-01",
	})
	var enrolled protocol.EnrollResponse
	mustUnmarshal(t, ec.expect(protocol.TypeEnrollResponse).Payload, &enrolled)
	if enrolled.FleetID != h.fleet.ID || !strings.HasPrefix(enrolled.ClientID, "r_") {
		t.Fatalf("enroll: %+v", enrolled)
	}
	robotID := enrolled.ClientID

	// 2. Robot connects with the token and declares its manifest.
	robot := h.connect(enrolled.Token)
	robot.send(protocol.TypeManifest, protocol.Manifest{
		Drive:    &protocol.Drive{Type: "twist", MaxVMps: 1.5, MaxWRadps: 2.0},
		Battery:  &struct{}{},
		Channels: []string{"assignment"},
	})

	// 3. Operator subscribes; snapshot-then-stream shows the robot online with
	// its manifest (retry: manifest processing on another conn may still land).
	operator := h.connect(opToken)
	var snap protocol.Snapshot
	deadline := time.Now().Add(3 * time.Second)
	for {
		operator.send(protocol.TypeSubscribe, protocol.Subscribe{
			Topics: []string{"presence", "events", "telemetry"},
		})
		mustUnmarshal(t, operator.expect(protocol.TypeSnapshot).Payload, &snap)
		if len(snap.Robots) == 1 && snap.Robots[0].Presence == "online" && snap.Robots[0].Manifest != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("snapshot never showed online robot with manifest: %+v", snap)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if snap.Robots[0].RobotID != robotID || snap.Robots[0].State != protocol.StateAutonomous {
		t.Fatalf("snapshot: %+v", snap.Robots[0])
	}

	// 4. Telemetry fans out to telemetry subscribers.
	robot.send(protocol.TypeTelemetry, protocol.Telemetry{
		Pose: &protocol.Pose{Frame: "local", FrameID: "map", XM: f(1.0), YM: f(2.0)},
	})
	if ev := operator.expectEvent(protocol.EventRobotTelemetry); ev.RobotID != robotID {
		t.Fatalf("telemetry event: %+v", ev)
	}

	// 5. Robot raises its hand → intervention queue event.
	robot.send(protocol.TypeHelpRequest, protocol.HelpRequest{Reason: "nav_goal_failed"})
	if ev := operator.expectEvent(protocol.EventRobotHelpRequested); ev.RobotID != robotID {
		t.Fatalf("help event: %+v", ev)
	}

	// 6. Operator claims → both sides hold the same lease.
	operator.send(protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotID})
	var opLease, robotLease protocol.Lease
	mustUnmarshal(t, operator.expect(protocol.TypeLeaseGranted).Payload, &opLease)
	mustUnmarshal(t, robot.expect(protocol.TypeLeaseGranted).Payload, &robotLease)
	if opLease.LeaseID != robotLease.LeaseID || opLease.OperatorID == "" || opLease.RobotID != robotID {
		t.Fatalf("lease mismatch: op=%+v robot=%+v", opLease, robotLease)
	}

	// 7. Twist bearing the lease id reaches the robot (control-plane fallback).
	operator.send(protocol.TypeTwist, protocol.Twist{
		LeaseID: opLease.LeaseID,
		Linear:  protocol.TwistLinear{XMps: 0.5},
		Angular: protocol.TwistAngular{ZRadps: -0.2},
	})
	var tw protocol.Twist
	mustUnmarshal(t, robot.expect(protocol.TypeTwist).Payload, &tw)
	if tw.LeaseID != opLease.LeaseID || tw.Linear.XMps != 0.5 {
		t.Fatalf("twist: %+v", tw)
	}

	// 8. Signaling relay, both directions, `from` stamped by the server.
	operator.send(protocol.TypeSignal, protocol.Signal{
		To: robotID, Kind: "offer", Data: json.RawMessage(`{"sdp":"fake-offer"}`),
	})
	var sigToRobot protocol.Signal
	mustUnmarshal(t, robot.expect(protocol.TypeSignal).Payload, &sigToRobot)
	if sigToRobot.Kind != "offer" || sigToRobot.From != opLease.OperatorID {
		t.Fatalf("signal to robot: %+v", sigToRobot)
	}
	robot.send(protocol.TypeSignal, protocol.Signal{
		To: sigToRobot.From, Kind: "answer", Data: json.RawMessage(`{"sdp":"fake-answer"}`),
	})
	var sigToOp protocol.Signal
	mustUnmarshal(t, operator.expect(protocol.TypeSignal).Payload, &sigToOp)
	if sigToOp.Kind != "answer" || sigToOp.From != robotID {
		t.Fatalf("signal to operator: %+v", sigToOp)
	}

	// 9. Handback: release → robot told, event emitted, FSM back to AUTONOMOUS.
	operator.send(protocol.TypeLeaseRelease, protocol.LeaseRelease{
		LeaseID: opLease.LeaseID, Resolution: "resolved",
	})
	var revoked protocol.LeaseRevoked
	mustUnmarshal(t, robot.expect(protocol.TypeLeaseRevoked).Payload, &revoked)
	if revoked.Reason != protocol.RevokeReleased {
		t.Fatalf("handback revocation: %+v", revoked)
	}
	operator.expectEvent(protocol.EventRobotLeaseReleased)

	// 10. Opaque domain channels: service subscribes, robot broadcasts.
	service := h.connect(svcToken)
	service.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: []string{"channel:edge_report"}})
	service.expect(protocol.TypeSnapshot)
	robot.send(protocol.TypeChannelPublish, protocol.ChannelPublish{
		Channel: "edge_report", Broadcast: true, Data: json.RawMessage(`{"edge":"e12","seconds":41.5}`),
	})
	var chMsg protocol.ChannelMessage
	mustUnmarshal(t, service.expect(protocol.TypeChannelMessage).Payload, &chMsg)
	if chMsg.From != robotID || chMsg.Channel != "edge_report" {
		t.Fatalf("channel message: %+v", chMsg)
	}

	// 11. Robot drops → presence subscribers see robot.offline.
	robot.ws.Close(websocket.StatusNormalClosure, "shutdown")
	if ev := operator.expectEvent(protocol.EventRobotOffline); ev.RobotID != robotID {
		t.Fatalf("offline event: %+v", ev)
	}
}

// TestIntegrationOperatorLoss: the operator's socket dies mid-teleop → the
// SERVER revokes the lease (reason operator_lost) and the robot goes back to
// HELP_REQUESTED. §7.5: the robot never re-queues itself.
func TestIntegrationOperatorLoss(t *testing.T) {
	h := newHarness(t, defaultConfig())
	robotToken, robotClient, err := h.store.CreateToken(h.fleet.ID, store.KindRobot, "sim-01")
	if err != nil {
		t.Fatal(err)
	}
	opToken, _, err := h.store.CreateToken(h.fleet.ID, store.KindOperator, "op-1")
	if err != nil {
		t.Fatal(err)
	}

	robot := h.connect(robotToken)
	robot.send(protocol.TypeHelpRequest, protocol.HelpRequest{Reason: "stuck"})

	operator := h.connect(opToken)
	operator.send(protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotClient.ID})
	operator.expect(protocol.TypeLeaseGranted)
	robot.expect(protocol.TypeLeaseGranted)

	operator.ws.Close(websocket.StatusNormalClosure, "browser gone")

	var revoked protocol.LeaseRevoked
	mustUnmarshal(t, robot.expect(protocol.TypeLeaseRevoked).Payload, &revoked)
	if revoked.Reason != protocol.RevokeOperatorLost {
		t.Fatalf("reason = %s, want operator_lost", revoked.Reason)
	}

	// The robot is back in the queue: a second operator can claim it.
	op2Token, _, err := h.store.CreateToken(h.fleet.ID, store.KindOperator, "op-2")
	if err != nil {
		t.Fatal(err)
	}
	op2 := h.connect(op2Token)
	op2.send(protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotClient.ID})
	op2.expect(protocol.TypeLeaseGranted)
}

// TestIntegrationLeaseExpiry: an unrenewed lease dies on its TTL — the server's
// transition, driven by the sweep loop with a real (tiny) TTL.
func TestIntegrationLeaseExpiry(t *testing.T) {
	cfg := defaultConfig()
	cfg.LeaseTTL = 150 * time.Millisecond
	h := newHarness(t, cfg)

	robotToken, robotClient, err := h.store.CreateToken(h.fleet.ID, store.KindRobot, "sim-01")
	if err != nil {
		t.Fatal(err)
	}
	opToken, _, err := h.store.CreateToken(h.fleet.ID, store.KindOperator, "op-1")
	if err != nil {
		t.Fatal(err)
	}

	robot := h.connect(robotToken)
	operator := h.connect(opToken)
	operator.send(protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotClient.ID})
	operator.expect(protocol.TypeLeaseGranted)
	robot.expect(protocol.TypeLeaseGranted)

	// No renewals: both sides must hear the expiry from the server.
	var robotRevoked protocol.LeaseRevoked
	mustUnmarshal(t, robot.expect(protocol.TypeLeaseRevoked).Payload, &robotRevoked)
	if robotRevoked.Reason != protocol.RevokeExpired {
		t.Fatalf("robot revocation reason = %s", robotRevoked.Reason)
	}
	var opRevoked protocol.LeaseRevoked
	mustUnmarshal(t, operator.expect(protocol.TypeLeaseRevoked).Payload, &opRevoked)
	if opRevoked.Reason != protocol.RevokeExpired {
		t.Fatalf("operator revocation reason = %s", opRevoked.Reason)
	}
}

// enrollOperatorInvite sends an operator enroll.request carrying an invite
// key over a fresh websocket and returns the first reply envelope.
func (h *harness) enrollOperatorInvite(inviteKey, name string) protocol.Envelope {
	h.t.Helper()
	c := h.dial()
	c.send(protocol.TypeEnrollRequest, protocol.EnrollRequest{
		EnrollmentKey: inviteKey, Kind: "operator", Name: name,
	})
	env, err := c.recv()
	if err != nil {
		h.t.Fatalf("operator enroll reply: %v", err)
	}
	return env
}

func expectInviteEnrollError(t *testing.T, env protocol.Envelope, code string) {
	t.Helper()
	if env.Type != protocol.TypeError {
		t.Fatalf("want error %q, got %s: %s", code, env.Type, env.Payload)
	}
	var e protocol.ErrorMsg
	mustUnmarshal(t, env.Payload, &e)
	if e.Code != code {
		t.Fatalf("error code = %q (%s), want %q", e.Code, e.Message, code)
	}
}

// TestIntegrationOperatorInviteEnroll: an operator redeems a one-time invite
// over the real enroll flow (DESIGN.md D14), connects with the minted token and
// takes the wheel. The invite is single use.
func TestIntegrationOperatorInviteEnroll(t *testing.T) {
	h := newHarness(t, defaultConfig())
	robotToken, robotClient, err := h.store.CreateToken(h.fleet.ID, store.KindRobot, "sim-01")
	if err != nil {
		t.Fatal(err)
	}
	invite, _, err := h.store.CreateOperatorInvite(h.fleet.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	env := h.enrollOperatorInvite(invite, "op-invited")
	if env.Type != protocol.TypeEnrollResponse {
		t.Fatalf("operator enroll: got %s: %s", env.Type, env.Payload)
	}
	var enrolled protocol.EnrollResponse
	mustUnmarshal(t, env.Payload, &enrolled)
	if enrolled.FleetID != h.fleet.ID || enrolled.Token == "" || !strings.HasPrefix(enrolled.ClientID, "o_") {
		t.Fatalf("operator enroll response: %+v", enrolled)
	}

	robot := h.connect(robotToken)
	robot.send(protocol.TypeHelpRequest, protocol.HelpRequest{Reason: "stuck"})

	operator := h.dial()
	operator.send(protocol.TypeHello, protocol.Hello{Token: enrolled.Token})
	var welcome protocol.Welcome
	mustUnmarshal(t, operator.expect(protocol.TypeWelcome).Payload, &welcome)
	if welcome.Kind != "operator" || welcome.ClientID != enrolled.ClientID {
		t.Fatalf("welcome: %+v", welcome)
	}

	operator.send(protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: robotClient.ID})
	var opLease, robotLease protocol.Lease
	mustUnmarshal(t, operator.expect(protocol.TypeLeaseGranted).Payload, &opLease)
	mustUnmarshal(t, robot.expect(protocol.TypeLeaseGranted).Payload, &robotLease)
	if opLease.LeaseID != robotLease.LeaseID || opLease.OperatorID != enrolled.ClientID {
		t.Fatalf("lease: op=%+v robot=%+v", opLease, robotLease)
	}

	// Single use: the same invite cannot mint a second operator.
	expectInviteEnrollError(t, h.enrollOperatorInvite(invite, "op-again"), protocol.ErrConflict)
}

// TestIntegrationOperatorInviteRejected: unknown and expired invites are auth
// failures, and a fleet enrollment key cannot mint an operator.
func TestIntegrationOperatorInviteRejected(t *testing.T) {
	h := newHarness(t, defaultConfig())

	expectInviteEnrollError(t, h.enrollOperatorInvite("fp-oi-not-a-real-invite", "op"), protocol.ErrAuthFailed)

	expired, _, err := h.store.CreateOperatorInvite(h.fleet.ID, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	expectInviteEnrollError(t, h.enrollOperatorInvite(expired, "op"), protocol.ErrAuthFailed)

	enrollKey, err := h.store.CreateEnrollKey(h.fleet.ID)
	if err != nil {
		t.Fatal(err)
	}
	expectInviteEnrollError(t, h.enrollOperatorInvite(enrollKey, "op"), protocol.ErrAuthFailed)
}

func f(v float64) *float64 { return &v }

// A robot that stops heartbeating but keeps its socket open (frozen process,
// dead Wi-Fi with no FIN) must go offline within about 2.5 heartbeat intervals,
// not after an extra wait for a close handshake the peer will never answer.
func TestIntegrationHeartbeatLapseGoesOfflinePromptly(t *testing.T) {
	cfg := defaultConfig()
	cfg.HeartbeatInterval = 200 * time.Millisecond
	h := newHarness(t, cfg)

	svcToken, _, err := h.store.CreateToken(h.fleet.ID, store.KindService, "watcher")
	if err != nil {
		t.Fatal(err)
	}
	robotToken, _, err := h.store.CreateToken(h.fleet.ID, store.KindRobot, "frozen-01")
	if err != nil {
		t.Fatal(err)
	}

	service := h.connect(svcToken)
	service.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: []string{"presence"}})
	service.expect(protocol.TypeSnapshot)

	// Hello, then never heartbeat and never read again.
	_ = h.connect(robotToken)
	service.expectEvent(protocol.EventRobotOnline)
	start := time.Now()

	// The service heartbeats so only the robot lapses.
	done := make(chan struct{})
	defer close(done)
	go func() {
		tk := time.NewTicker(cfg.HeartbeatInterval / 2)
		defer tk.Stop()
		for {
			select {
			case <-done:
				return
			case <-tk.C:
				data, _ := json.Marshal(protocol.Msg(protocol.TypeHeartbeat, struct{}{}))
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				_ = service.ws.Write(ctx, websocket.MessageText, data)
				cancel()
			}
		}
	}()

	service.expectEvent(protocol.EventRobotOffline)
	if elapsed, limit := time.Since(start), cfg.HeartbeatInterval*5/2+time.Second; elapsed > limit {
		t.Fatalf("robot.offline after %v, want within %v", elapsed, limit)
	}
}
