package app_test

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"fleetplatform/sdk/go/protocol"
	"fleetplatform/server/internal/signaling"
	"fleetplatform/server/internal/store"
)

const iceTestSecret = "integration-turn-secret-0123456789"

// iceConfig sends ice.request with the given envelope id and reads the reply.
func (c *client) iceConfig(id string) protocol.IceConfig {
	c.t.Helper()
	c.sendRaw(protocol.TypeIceRequest, id, `{}`)
	var cfg protocol.IceConfig
	mustUnmarshal(c.t, c.nextOf(protocol.TypeIceConfig).Payload, &cfg)
	if cfg.Ref != id {
		c.t.Fatalf("ice.config ref = %q, want %q", cfg.Ref, id)
	}
	return cfg
}

// TestIntegrationIceConfig: a robot and an operator each receive the ICE
// servers the installation is configured with, in the RTCIceServer shape, with
// a TURN credential of their own that a TURN server holding the same secret
// would accept. A service gets none.
func TestIntegrationIceConfig(t *testing.T) {
	stun := []string{"stun:turn.example.org:3478"}
	turn := []string{"turn:turn.example.org:3478?transport=udp", "turns:turn.example.org:5349?transport=tcp"}
	cfg := defaultConfig()
	cfg.ICE = signaling.ICE{STUNURLs: stun, TURNURLs: turn, TURNSecret: iceTestSecret, CredentialTTL: 10 * time.Minute}
	h := newHarness(t, cfg)
	opTok, opID := h.token(store.KindOperator, "ada")
	robotTok, robotID := h.token(store.KindRobot, "bot-1")
	svcTok, _ := h.token(store.KindService, "brain")
	op := h.connect(opTok)
	robot := h.connect(robotTok)
	svc := h.connect(svcTok)

	// The wire form, key by key: the entries are what RTCPeerConnection takes.
	before := time.Now()
	op.sendRaw(protocol.TypeIceRequest, "ice-1", `{}`)
	env := op.nextOf(protocol.TypeIceConfig)
	after := time.Now()
	var raw struct {
		IceServers  []map[string]json.RawMessage `json:"ice_servers"`
		ExpiresAtMs int64                        `json:"expires_at_ms"`
		Ref         string                       `json:"ref"`
	}
	mustUnmarshal(t, env.Payload, &raw)
	if raw.Ref != "ice-1" || len(raw.IceServers) != 2 {
		t.Fatalf("ice.config: %s", env.Payload)
	}
	if len(raw.IceServers[0]) != 1 || string(raw.IceServers[0]["urls"]) != `["stun:turn.example.org:3478"]` {
		t.Fatalf("stun entry: %s", env.Payload)
	}
	if len(raw.IceServers[1]) != 3 || string(raw.IceServers[1]["urls"]) != `["turn:turn.example.org:3478?transport=udp","turns:turn.example.org:5349?transport=tcp"]` {
		t.Fatalf("turn entry: %s", env.Payload)
	}
	if strings.Contains(string(env.Payload), iceTestSecret) {
		t.Fatalf("the shared secret left the server: %s", env.Payload)
	}

	// The credential is the time-limited shared-secret scheme: the username is
	// "<expiry unix seconds>:<client id>" and the credential its HMAC-SHA1.
	check := func(who string, got protocol.IceConfig, clientID string) {
		t.Helper()
		if len(got.IceServers) != 2 || !reflect.DeepEqual(got.IceServers[0], protocol.IceServer{URLs: stun}) {
			t.Fatalf("%s: %+v", who, got)
		}
		ts := got.IceServers[1]
		if !reflect.DeepEqual(ts.URLs, turn) {
			t.Fatalf("%s turn urls: %+v", who, ts)
		}
		expiry, identity, ok := strings.Cut(ts.Username, ":")
		secs, err := strconv.ParseInt(expiry, 10, 64)
		if !ok || err != nil || identity != clientID {
			t.Fatalf("%s username %q, want <expiry>:%s", who, ts.Username, clientID)
		}
		if got.ExpiresAtMs != secs*1000 {
			t.Fatalf("%s expires_at_ms %d does not match username %q", who, got.ExpiresAtMs, ts.Username)
		}
		lo, hi := before.Add(10*time.Minute).Unix()-1, after.Add(10*time.Minute).Unix()+60
		if secs < lo || secs > hi {
			t.Fatalf("%s expiry %d not ten minutes out (%d..%d)", who, secs, lo, hi)
		}
		mac := hmac.New(sha1.New, []byte(iceTestSecret))
		mac.Write([]byte(ts.Username))
		if want := base64.StdEncoding.EncodeToString(mac.Sum(nil)); ts.Credential != want {
			t.Fatalf("%s credential %q, want %q", who, ts.Credential, want)
		}
	}
	var first protocol.IceConfig
	mustUnmarshal(t, env.Payload, &first)
	check("operator", first, opID)
	check("robot", robot.iceConfig("ice-r"), robotID)
	// Asking again is always answered, with a credential at least as fresh.
	again := op.iceConfig("ice-2")
	check("operator again", again, opID)
	if again.ExpiresAtMs < first.ExpiresAtMs {
		t.Fatalf("second credential expires earlier: %d < %d", again.ExpiresAtMs, first.ExpiresAtMs)
	}

	// A service is not an end of a teleop connection.
	svc.sendRaw(protocol.TypeIceRequest, "ice-s", `{}`)
	if e := svc.nextError(); e.Code != protocol.ErrNotAuthorized || e.Ref != "ice-s" {
		t.Fatalf("service ice.request: %+v", e)
	}

	// Nobody else hears about any of it.
	for _, c := range []*client{op, robot, svc} {
		c.quiet()
	}
}

// TestIntegrationIceConfigUnconfigured: with no ICE servers configured (the
// default) the answer is an empty list and nothing else, so clients behave as
// they did before the message existed.
func TestIntegrationIceConfigUnconfigured(t *testing.T) {
	h := newHarness(t, defaultConfig())
	opTok, _ := h.token(store.KindOperator, "ada")
	robotTok, _ := h.token(store.KindRobot, "bot-1")
	for _, tok := range []string{opTok, robotTok} {
		c := h.connect(tok)
		c.sendRaw(protocol.TypeIceRequest, "ice-1", `{}`)
		if got := string(c.nextOf(protocol.TypeIceConfig).Payload); got != `{"ice_servers":[],"ref":"ice-1"}` {
			t.Fatalf("unconfigured ice.config: %s", got)
		}
		// Without an id there is no ref.
		c.send(protocol.TypeIceRequest, protocol.IceRequest{})
		if got := string(c.nextOf(protocol.TypeIceConfig).Payload); got != `{"ice_servers":[]}` {
			t.Fatalf("unconfigured ice.config without id: %s", got)
		}
	}

	// STUN alone: addresses, no credentials, no expiry.
	cfg := defaultConfig()
	cfg.ICE = signaling.ICE{STUNURLs: []string{"stun:10.0.0.5:3478"}}
	c := newHarness(t, cfg).connectAs(store.KindRobot, "bot-1")
	c.sendRaw(protocol.TypeIceRequest, "ice-1", `{}`)
	if got := string(c.nextOf(protocol.TypeIceConfig).Payload); got != `{"ice_servers":[{"urls":["stun:10.0.0.5:3478"]}],"ref":"ice-1"}` {
		t.Fatalf("stun-only ice.config: %s", got)
	}
}

// connectAs enrolls a client of the given kind and connects it.
func (h *harness) connectAs(kind store.Kind, name string) *client {
	h.t.Helper()
	tok, _ := h.token(kind, name)
	return h.connect(tok)
}
