package admin_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"fleetplatform/server/internal/protocol"
)

// adminInviteEnrollOverWS sends one enroll.request over a real websocket and
// returns the server's single reply envelope.
func adminInviteEnrollOverWS(t *testing.T, h *adminInviteHarness, key, name string) protocol.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.srv.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	data, err := json.Marshal(protocol.Msg(protocol.TypeEnrollRequest, protocol.EnrollRequest{
		EnrollmentKey: key, Kind: "operator", Name: name,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatal(err)
	}
	_, reply, err := ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var env protocol.Envelope
	if err := json.Unmarshal(reply, &env); err != nil {
		t.Fatal(err)
	}
	return env
}

func adminInviteErrCode(t *testing.T, env protocol.Envelope) string {
	t.Helper()
	if env.Type != protocol.TypeError {
		t.Fatalf("got %s %s, want error", env.Type, env.Payload)
	}
	var e protocol.ErrorMsg
	if err := json.Unmarshal(env.Payload, &e); err != nil {
		t.Fatal(err)
	}
	return e.Code
}

// TestAdminInviteRedeemsOnceOverWebsocket is the full fleetctl path: mint over
// the admin API, then redeem through enroll.request {kind: operator} on /ws.
func TestAdminInviteRedeemsOnceOverWebsocket(t *testing.T) {
	h := newAdminInviteHarness(t)
	bearer := "Bearer " + adminInviteTestToken

	resp, inv := h.mintAdminInvite("club", bearer, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mint status %d", resp.StatusCode)
	}

	env := adminInviteEnrollOverWS(t, h, inv.Key, "alice")
	if env.Type != protocol.TypeEnrollResponse {
		t.Fatalf("first redeem: got %s %s", env.Type, env.Payload)
	}
	var enrolled protocol.EnrollResponse
	if err := json.Unmarshal(env.Payload, &enrolled); err != nil {
		t.Fatal(err)
	}
	if enrolled.FleetID != h.fleet.ID || enrolled.Token == "" {
		t.Fatalf("enroll response = %+v", enrolled)
	}
	c, ok, err := h.store.AuthToken(enrolled.Token)
	if err != nil || !ok || c.Kind != "operator" || c.Name != "alice" {
		t.Fatalf("minted token resolves to %+v ok=%v err=%v", c, ok, err)
	}

	if code := adminInviteErrCode(t, adminInviteEnrollOverWS(t, h, inv.Key, "mallory")); code != protocol.ErrConflict {
		t.Fatalf("second redeem code = %q, want %q", code, protocol.ErrConflict)
	}

	// An invite minted already short-lived is refused once it lapses.
	resp, short := h.mintAdminInvite("club", bearer, `{"ttl":"1ms"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mint short status %d", resp.StatusCode)
	}
	time.Sleep(20 * time.Millisecond)
	if code := adminInviteErrCode(t, adminInviteEnrollOverWS(t, h, short.Key, "late")); code != protocol.ErrAuthFailed {
		t.Fatalf("expired redeem code = %q, want %q", code, protocol.ErrAuthFailed)
	}
}
