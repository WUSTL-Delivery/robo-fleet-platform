package fleet

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"fleetplatform/sdk/go/protocol"
)

// These tests drive the stream gate and the typed dispatch directly, with no
// socket: the orderings they pin down (an event that is on the wire before its
// snapshot, two subscribes in flight) cannot be forced against a real server.
// Everything that involves a connection is in subscribe_test.go.

func envOf(typ, id string, payload any) protocol.Envelope {
	env := protocol.Msg(typ, payload)
	env.ID = id
	return env
}

func event(name, robot string) protocol.Envelope {
	return envOf(protocol.TypeEvent, "", protocol.Event{Event: name, RobotID: robot})
}

// names is the gate's output as "type" or "type:event-name".
func names(envs []protocol.Envelope) []string {
	out := []string{}
	for _, env := range envs {
		name := env.Type
		if env.Type == protocol.TypeEvent {
			var ev protocol.Event
			json.Unmarshal(env.Payload, &ev)
			name += ":" + ev.Event + ":" + ev.RobotID
		}
		out = append(out, name)
	}
	return out
}

func wantOut(t *testing.T, got []protocol.Envelope, want ...string) {
	t.Helper()
	if !slices.Equal(names(got), want) {
		t.Fatalf("gate released %v, want %v", names(got), want)
	}
}

func inflight(s *stream, topics ...string) *subCall {
	call := &subCall{added: topics, done: make(chan struct{})}
	s.topics = append(s.topics, topics...)
	s.inflight = append(s.inflight, &subSend{id: s.nextID(), calls: []*subCall{call}})
	return call
}

func finished(call *subCall) bool {
	select {
	case <-call.done:
		return true
	default:
		return false
	}
}

func TestGateHoldsStreamEnvelopesUntilTheSnapshot(t *testing.T) {
	s := &stream{}
	snapshot := envOf(protocol.TypeSnapshot, "", protocol.Snapshot{Robots: []protocol.RobotSummary{{RobotID: "r_1"}}})

	// Nothing outstanding: everything passes straight through.
	wantOut(t, s.gate(event("robot.online", "r_0")), "event:robot.online:r_0")

	call := inflight(s, "presence")
	wantOut(t, s.gate(event("robot.online", "r_1")))
	wantOut(t, s.gate(envOf(protocol.TypeChannelMessage, "", protocol.ChannelMessage{Channel: "c", From: "r_1"})))
	wantOut(t, s.gate(envOf(protocol.TypeLayerUpdate, "", protocol.LayerUpdate{LayerID: "l"})))
	// Replies and directed messages are never held.
	wantOut(t, s.gate(envOf(protocol.TypeLeaseGranted, "", protocol.Lease{LeaseID: "ls_1"})), protocol.TypeLeaseGranted)
	wantOut(t, s.gate(envOf(protocol.TypeError, "", protocol.ErrorMsg{Code: "not_found", Ref: "pub-1"})), protocol.TypeError)
	if finished(call) {
		t.Fatal("Subscribe answered before its snapshot")
	}

	wantOut(t, s.gate(snapshot),
		protocol.TypeSnapshot, "event:robot.online:r_1", protocol.TypeChannelMessage, protocol.TypeLayerUpdate)
	if !finished(call) || call.err != nil || len(call.snap.Robots) != 1 || call.snap.Robots[0].RobotID != "r_1" {
		t.Fatalf("call = %+v, want the snapshot", call)
	}
	wantOut(t, s.gate(event("robot.offline", "r_1")), "event:robot.offline:r_1")

	// An unsolicited snapshot is passed on like any other envelope.
	wantOut(t, s.gate(snapshot), protocol.TypeSnapshot)
}

func TestGateWithTwoSubscribesInFlight(t *testing.T) {
	s := &stream{}
	snapshot := envOf(protocol.TypeSnapshot, "", protocol.Snapshot{})
	first, second := inflight(s, "presence"), inflight(s, "events")

	wantOut(t, s.gate(event("robot.online", "r_1")))
	// The first snapshot is delivered at once, but held envelopes wait for the
	// last one: the second snapshot would otherwise replace a model they had
	// already been applied to.
	wantOut(t, s.gate(snapshot), protocol.TypeSnapshot)
	if !finished(first) || finished(second) {
		t.Fatalf("after one snapshot: first done %v, second done %v", finished(first), finished(second))
	}
	wantOut(t, s.gate(event("robot.offline", "r_1")))
	wantOut(t, s.gate(snapshot), protocol.TypeSnapshot, "event:robot.online:r_1", "event:robot.offline:r_1")
	if !finished(second) {
		t.Fatal("second Subscribe not answered")
	}
}

func TestGateRefusedSubscribe(t *testing.T) {
	s := &stream{topics: []string{"presence"}}
	refused, kept := inflight(s, "events", "telemetry"), inflight(s, "layers")
	wantOut(t, s.gate(event("robot.online", "r_1")))

	// An error for something else changes nothing.
	wantOut(t, s.gate(envOf(protocol.TypeError, "", protocol.ErrorMsg{Code: "not_found", Ref: "pub-9"})), protocol.TypeError)
	if finished(refused) {
		t.Fatal("an unrelated error answered a Subscribe")
	}

	// The error naming the first subscribe's id refuses it; the second is
	// still outstanding, so nothing is released yet.
	wantOut(t, s.gate(envOf(protocol.TypeError, "", protocol.ErrorMsg{Code: "invalid_message", Message: "no", Ref: "sub-1"})), protocol.TypeError)
	if !finished(refused) || !errors.Is(refused.err, ErrInvalidMessage) {
		t.Fatalf("refused call: done %v, err %v", finished(refused), refused.err)
	}
	if want := []string{"presence", "layers"}; !slices.Equal(s.topics, want) {
		t.Fatalf("topics after a refusal = %v, want %v", s.topics, want)
	}
	wantOut(t, s.gate(envOf(protocol.TypeSnapshot, "", protocol.Snapshot{})), protocol.TypeSnapshot, "event:robot.online:r_1")
	if !finished(kept) || kept.err != nil {
		t.Fatalf("second call: done %v, err %v", finished(kept), kept.err)
	}
}

func TestLostSessionDropsHeldEnvelopesAndKeepsTheCallsWaiting(t *testing.T) {
	s := &stream{}
	call := inflight(s, "presence")
	wantOut(t, s.gate(event("robot.online", "r_1")))

	s.lost()
	if finished(call) || len(s.inflight) != 0 || len(s.held) != 0 {
		t.Fatalf("after lost: done %v, inflight %d, held %d", finished(call), len(s.inflight), len(s.held))
	}
	if len(s.deferred) != 1 || s.deferred[0] != call || !slices.Equal(s.topics, []string{"presence"}) {
		t.Fatalf("after lost: deferred %v, topics %v", s.deferred, s.topics)
	}
	// Nothing is outstanding on the next socket until opened() subscribes.
	wantOut(t, s.gate(event("robot.online", "r_2")), "event:robot.online:r_2")
}

// The typed callbacks decode what the server sends, and an event name this SDK
// has never heard of is delivered to OnEvent, not dropped and not an error.
func TestDispatchStream(t *testing.T) {
	c, err := newClient(Config{URL: "ws://127.0.0.1:1/ws", Kind: Service})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	c.OnPresence(func(id string, online bool) {
		got = append(got, "presence "+id+" "+map[bool]string{true: "online", false: "offline"}[online])
	})
	c.OnTelemetry(func(id string, tel Telemetry) { got = append(got, "telemetry "+id) })
	c.OnHelp(func(id string, h HelpDetails) { got = append(got, "help "+id+" "+h.Reason) })
	c.OnLease(func(ch LeaseChange) {
		got = append(got, "lease "+ch.RobotID+" "+ch.Event+" "+ch.Reason+" -> "+ch.State)
	})
	c.OnEvent(func(ev Event) { got = append(got, "event "+ev.Event) })

	send := func(name string, data any) {
		ev := protocol.Event{Event: name, RobotID: "r_1"}
		if data != nil {
			ev.Data, _ = json.Marshal(data)
		}
		c.dispatch(protocol.Msg(protocol.TypeEvent, ev))
	}
	help := &protocol.HelpDetails{Reason: "stuck", RequestedAtMs: 5}
	send("robot.online", nil)
	send("robot.telemetry", protocol.Telemetry{})
	send("robot.help_requested", help)
	send("robot.lease_granted", protocol.Lease{LeaseID: "ls_1", RobotID: "r_1", OperatorID: "o_1"})
	send("robot.lease_revoked", protocol.LeaseRevoked{LeaseID: "ls_1", RobotID: "r_1", Reason: "stolen"})
	send("robot.lease_revoked", protocol.LeaseRevoked{LeaseID: "ls_2", RobotID: "r_1", Reason: "expired", Help: help})
	send("robot.lease_revoked", protocol.LeaseRevoked{LeaseID: "ls_3", RobotID: "r_1", Reason: "a_new_reason"})
	send("robot.lease_released", protocol.LeaseRevoked{LeaseID: "ls_4", RobotID: "r_1", Reason: "released"})
	send("operator.online", map[string]any{"operator_id": "o_1"})
	send("robot.telemetry", "not an object")
	send("robot.offline", nil)

	want := []string{
		"presence r_1 online", "event robot.online",
		"telemetry r_1", "event robot.telemetry",
		"help r_1 stuck", "event robot.help_requested",
		"lease r_1 robot.lease_granted  -> TELEOP", "event robot.lease_granted",
		"lease r_1 robot.lease_revoked stolen -> TELEOP", "event robot.lease_revoked",
		"lease r_1 robot.lease_revoked expired -> HELP_REQUESTED", "help r_1 stuck", "event robot.lease_revoked",
		"lease r_1 robot.lease_revoked a_new_reason -> ", "event robot.lease_revoked",
		"lease r_1 robot.lease_released released -> AUTONOMOUS", "event robot.lease_released",
		"event operator.online",
		"event robot.telemetry",
		"presence r_1 offline", "event robot.offline",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("callbacks:\n got %q\nwant %q", got, want)
	}
}
