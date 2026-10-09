package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"fleetplatform/sdk/go/protocol"
	"fleetplatform/server/internal/admin"
	"fleetplatform/server/internal/store"
)

// revokeWS is a raw protocol client for the client revocation tests.
type revokeWS struct {
	t  *testing.T
	ws *websocket.Conn
}

func (h *adminInviteHarness) revokeDial(token string) (*revokeWS, protocol.Envelope) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.srv.URL, "http")+"/ws", nil)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { ws.CloseNow() })
	c := &revokeWS{t: h.t, ws: ws}
	c.send(protocol.TypeHello, protocol.Hello{Token: token})
	env, err := c.recv()
	if err != nil {
		h.t.Fatalf("hello reply: %v", err)
	}
	return c, env
}

func (h *adminInviteHarness) revokeConnect(token string) *revokeWS {
	h.t.Helper()
	c, env := h.revokeDial(token)
	if env.Type != protocol.TypeWelcome {
		h.t.Fatalf("hello: got %s %s, want welcome", env.Type, env.Payload)
	}
	return c
}

func (c *revokeWS) send(typ string, payload any) {
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

func (c *revokeWS) recv() (protocol.Envelope, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, data, err := c.ws.Read(ctx)
	if err != nil {
		return protocol.Envelope{}, err
	}
	var env protocol.Envelope
	err = json.Unmarshal(data, &env)
	return env, err
}

// expect reads until an envelope of type typ arrives (skipping others).
func (c *revokeWS) expect(typ string) protocol.Envelope {
	c.t.Helper()
	for i := 0; i < 50; i++ {
		env, err := c.recv()
		if err != nil {
			c.t.Fatalf("waiting for %s: %v", typ, err)
		}
		if env.Type == typ {
			return env
		}
	}
	c.t.Fatalf("gave up waiting for %s", typ)
	return protocol.Envelope{}
}

// expectEvent reads events until the named one arrives for robotID.
func (c *revokeWS) expectEvent(name, robotID string) protocol.Event {
	c.t.Helper()
	for i := 0; i < 50; i++ {
		var ev protocol.Event
		if err := json.Unmarshal(c.expect(protocol.TypeEvent).Payload, &ev); err != nil {
			c.t.Fatal(err)
		}
		if ev.Event == name && ev.RobotID == robotID {
			return ev
		}
	}
	c.t.Fatalf("gave up waiting for %s on %s", name, robotID)
	return protocol.Event{}
}

// expectKicked reads until the server's final error, then the close.
func (c *revokeWS) expectKicked(wantCode string) {
	c.t.Helper()
	sawErr := false
	for i := 0; i < 50; i++ {
		env, err := c.recv()
		if err != nil {
			if !sawErr {
				c.t.Fatalf("socket closed without an error envelope: %v", err)
			}
			var ce websocket.CloseError
			if errors.As(err, &ce) || strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "closed") {
				return
			}
			c.t.Fatalf("after error, want close, got %v", err)
		}
		if env.Type == protocol.TypeError {
			var e protocol.ErrorMsg
			json.Unmarshal(env.Payload, &e)
			if e.Code != wantCode {
				c.t.Fatalf("kick error code %q, want %q", e.Code, wantCode)
			}
			sawErr = true
		}
	}
	c.t.Fatal("never closed")
}

func (h *adminInviteHarness) revokeClientCall(fleet, id string) (int, admin.RevokeClientResponse) {
	h.t.Helper()
	status, raw := h.enrollKeyLifecycleCall(http.MethodPost, "/api/admin/fleets/"+fleet+"/clients/"+id+"/revoke", "Bearer "+adminInviteTestToken, "")
	var out admin.RevokeClientResponse
	if status == http.StatusOK {
		if err := json.Unmarshal(raw, &out); err != nil {
			h.t.Fatalf("revoke body %s: %v", raw, err)
		}
	}
	return status, out
}

// TestAdminClientRevokeDropsLiveConnection is the lost-robot / lost-laptop
// path: revoke over the admin API, the live socket is closed with auth_failed,
// presence goes offline, leases the client held are revoked through the normal
// disconnect path, the token is refused on the next hello, and every other
// client in the fleet is unaffected.
func TestAdminClientRevokeDropsLiveConnection(t *testing.T) {
	h := newAdminInviteHarness(t)
	mk := func(kind store.Kind, name string) (string, store.Client) {
		tok, c, err := h.store.CreateToken(h.fleet.ID, kind, name)
		if err != nil {
			t.Fatal(err)
		}
		return tok, c
	}
	lostTok, lost := mk(store.KindRobot, "lost-01")
	keptTok, kept := mk(store.KindRobot, "kept-02")
	opTok, op := mk(store.KindOperator, "laptop")
	svcTok, _ := mk(store.KindService, "watcher")

	watcher := h.revokeConnect(svcTok)
	watcher.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: []string{"presence", "events"}})
	watcher.expect(protocol.TypeSnapshot)

	lostWS := h.revokeConnect(lostTok)
	watcher.expectEvent(protocol.EventRobotOnline, lost.ID)
	keptWS := h.revokeConnect(keptTok)
	watcher.expectEvent(protocol.EventRobotOnline, kept.ID)
	opWS := h.revokeConnect(opTok)

	// The operator holds the wheel on the kept robot.
	opWS.send(protocol.TypeLeaseClaim, protocol.LeaseClaim{RobotID: kept.ID})
	opWS.expect(protocol.TypeLeaseGranted)
	keptWS.expect(protocol.TypeLeaseGranted)
	watcher.expectEvent(protocol.EventRobotLeaseGranted, kept.ID)

	// 1. Revoke the robot: its socket is closed promptly, the watcher sees it go offline.
	start := time.Now()
	status, rv := h.revokeClientCall("club", lost.ID)
	if status != http.StatusOK || rv.State != admin.ClientRevoked || rv.RevokedAt == nil || !rv.Disconnected || rv.Kind != "robot" {
		t.Fatalf("revoke robot: %d %+v", status, rv)
	}
	lostWS.expectKicked(protocol.ErrAuthFailed)
	watcher.expectEvent(protocol.EventRobotOffline, lost.ID)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("robot.offline %v after revoke", elapsed)
	}

	// 2. Its token is dead: the re-hello is refused with auth_failed.
	_, env := h.revokeDial(lostTok)
	if code := adminInviteErrCode(t, env); code != protocol.ErrAuthFailed {
		t.Fatalf("re-hello after revoke: code %q, want %q", code, protocol.ErrAuthFailed)
	}

	// 3. Revoke the operator: its lease on the kept robot is revoked (operator lost).
	status, rv = h.revokeClientCall("club", op.ID)
	if status != http.StatusOK || !rv.Disconnected || rv.Kind != "operator" {
		t.Fatalf("revoke operator: %d %+v", status, rv)
	}
	opWS.expectKicked(protocol.ErrAuthFailed)
	var lr protocol.LeaseRevoked
	if err := json.Unmarshal(keptWS.expect(protocol.TypeLeaseRevoked).Payload, &lr); err != nil || lr.Reason != protocol.RevokeOperatorLost {
		t.Fatalf("kept robot lease.revoked = %+v, %v", lr, err)
	}
	watcher.expectEvent(protocol.EventRobotLeaseRevoked, kept.ID)

	// 4. The other robot is untouched: still online, still talking.
	keptWS.send(protocol.TypeHeartbeat, struct{}{})
	watcher.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: []string{"presence"}})
	var snap protocol.Snapshot
	if err := json.Unmarshal(watcher.expect(protocol.TypeSnapshot).Payload, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Robots) != 1 || snap.Robots[0].RobotID != kept.ID || snap.Robots[0].Presence != "online" {
		t.Fatalf("snapshot after revoke = %+v, want only %s online (revoked robots are gone)", snap.Robots, kept.ID)
	}
	if _, env := h.revokeDial(keptTok); env.Type != protocol.TypeWelcome {
		t.Fatalf("kept robot token refused after another client's revoke: %s %s", env.Type, env.Payload)
	}

	// 5. Idempotent: revoking again changes nothing and closes nothing.
	status, again := h.revokeClientCall("club", op.ID)
	if status != http.StatusOK || again.Disconnected || again.RevokedAt == nil || !again.RevokedAt.Equal(*rv.RevokedAt) {
		t.Fatalf("second revoke: %d %+v, first %+v", status, again, rv)
	}
}

func TestAdminClientListAndRevokeErrors(t *testing.T) {
	h := newAdminInviteHarness(t)
	bearer := "Bearer " + adminInviteTestToken
	tok, robot, err := h.store.CreateToken(h.fleet.ID, store.KindRobot, "bot")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.store.CreateToken(h.fleet.ID, store.KindService, "brain"); err != nil {
		t.Fatal(err)
	}
	other, err := h.store.CreateFleet("elsewhere")
	if err != nil {
		t.Fatal(err)
	}
	_, stranger, err := h.store.CreateToken(other.ID, store.KindRobot, "not-yours")
	if err != nil {
		t.Fatal(err)
	}

	status, raw := h.enrollKeyLifecycleCall(http.MethodGet, "/api/admin/fleets/club/clients", bearer, "")
	var list admin.ClientList
	if status != http.StatusOK || json.Unmarshal(raw, &list) != nil || len(list.Clients) != 2 {
		t.Fatalf("list: %d %s", status, raw)
	}
	if strings.Contains(string(raw), tok) || strings.Contains(string(raw), "token") {
		t.Fatalf("list leaks tokens: %s", raw)
	}
	if c := list.Clients[0]; c.ID != robot.ID || c.Kind != "robot" || c.Name != "bot" || c.State != admin.ClientActive || c.RevokedAt != nil || c.CreatedAt.IsZero() {
		t.Fatalf("listed robot = %+v", c)
	}

	// Not connected: revoke still works, nothing to disconnect.
	if status, rv := h.revokeClientCall("club", robot.ID); status != http.StatusOK || rv.Disconnected || rv.State != admin.ClientRevoked {
		t.Fatalf("revoke offline robot: %d %+v", status, rv)
	}
	status, raw = h.enrollKeyLifecycleCall(http.MethodGet, "/api/admin/fleets/club/clients", bearer, "")
	if status != http.StatusOK || json.Unmarshal(raw, &list) != nil || list.Clients[0].State != admin.ClientRevoked || list.Clients[1].State != admin.ClientActive {
		t.Fatalf("list after revoke: %d %s", status, raw)
	}

	// A client of another fleet is not found through this fleet.
	if status, _ := h.revokeClientCall("club", stranger.ID); status != http.StatusNotFound {
		t.Fatalf("revoke other fleet's client: %d, want 404", status)
	}
	if _, ok, _ := h.store.AuthToken(tok); ok {
		t.Fatal("revoked token still authenticates")
	}
	for _, path := range []string{"/api/admin/fleets/nope/clients", "/api/admin/fleets/club/clients/r_nope/revoke"} {
		method := http.MethodGet
		if strings.HasSuffix(path, "/revoke") {
			method = http.MethodPost
		}
		if status, _ := h.enrollKeyLifecycleCall(method, path, bearer, ""); status != http.StatusNotFound {
			t.Fatalf("%s %s: %d, want 404", method, path, status)
		}
	}
	if status, _ := h.enrollKeyLifecycleCall(http.MethodGet, "/api/admin/fleets/club/clients", "Bearer wrong", ""); status != http.StatusUnauthorized {
		t.Fatalf("list with wrong admin token: %d", status)
	}
}
