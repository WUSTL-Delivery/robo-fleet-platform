package fleet

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"fleetplatform/sdk/go/protocol"
)

func fixture(t *testing.T, name string) protocol.Envelope {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "protocol", "fixtures", "valid", name))
	if err != nil {
		t.Fatal(err)
	}
	var env protocol.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	return env
}

func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		t.Fatalf("not JSON: %s / %s", a, b)
	}
	return reflect.DeepEqual(x, y)
}

func offlineClient(t *testing.T) *Client {
	t.Helper()
	c, err := newClient(Config{URL: "ws://127.0.0.1:1/ws", Kind: Service})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// pend registers a pending acked send as SendAcked does.
func pend(c *Client, seq int64, channel, to string) *ackedSend {
	p := &ackedSend{channel: channel, to: to, done: make(chan error, 1)}
	c.channels.mu.Lock()
	if c.channels.pending == nil {
		c.channels.pending = map[int64]*ackedSend{}
	}
	c.channels.pending[seq] = p
	c.channels.mu.Unlock()
	return p
}

func outcome(p *ackedSend) (err error, resolved bool) {
	select {
	case err := <-p.done:
		return err, true
	default:
		return nil, false
	}
}

// The two golden wire examples of the convention (protocol/README.md): what
// the sender puts on the wire, and the ack that settles it.
func TestAckedGoldenFixtures(t *testing.T) {
	pub := fixture(t, "channel-publish-acked.json")
	var want protocol.ChannelPublish
	if err := json.Unmarshal(pub.Payload, &want); err != nil {
		t.Fatal(err)
	}
	seq, inner, ok := ackedMessage(want.Data)
	if !ok || seq != 1755100000001 || !sameJSON(t, inner, []byte(`{"op":"blink","times":3}`)) {
		t.Fatalf("fixture data read as seq %d, inner %s, ok %v", seq, inner, ok)
	}
	// Built the way SendAcked builds it, the payload is the fixture's.
	built, _ := json.Marshal(protocol.ChannelPublish{Channel: want.Channel, To: want.To, Data: ackedData(seq, inner)})
	if !sameJSON(t, built, pub.Payload) {
		t.Fatalf("built %s\nfixture %s", built, pub.Payload)
	}
	if want.Broadcast || want.To == "" || pub.ID == "" {
		t.Fatalf("an acked message is directed and carries an id: %+v id %q", want, pub.ID)
	}
	if ref := ackedRef(maxSeq, 99999); len(ref) > 64 {
		t.Fatalf("transmission id %q is longer than an envelope id may be", ref)
	}

	ack := fixture(t, "channel-message-ack.json")
	var msg protocol.ChannelMessage
	json.Unmarshal(ack.Payload, &msg)
	if got, ok := ackMessage(msg.Data); !ok || got != seq {
		t.Fatalf("fixture ack read as %d, %v", got, ok)
	}
	c := offlineClient(t)
	p := pend(c, seq, want.Channel, want.To)
	c.observeAcked(ack)
	if err, resolved := outcome(p); !resolved || err != nil {
		t.Fatalf("the fixture ack did not settle the fixture send: resolved %v, err %v", resolved, err)
	}
}

func TestReservedShapes(t *testing.T) {
	for data, want := range map[string]int64{
		`{"seq":1,"data":null}`:                      1,
		`{"data":{"seq":5,"data":1},"seq":7}`:        7,
		`{"seq":9007199254740991,"data":"x"}`:        maxSeq,
		`{"seq":9007199254740992,"data":"x"}`:        0, // out of range
		`{"seq":0,"data":"x"}`:                       0,
		`{"seq":-1,"data":"x"}`:                      0,
		`{"seq":"1","data":"x"}`:                     0, // a string
		`{"seq":1.0,"data":"x"}`:                     0, // not a JSON integer
		`{"seq":1e3,"data":"x"}`:                     0,
		`{"seq":true,"data":"x"}`:                    0,
		`{"seq":1}`:                                  0, // data missing
		`{"seq":1,"data":"x","note":"extra member"}`: 0,
		`{"ack":1}`:                                  0,
		`[1,2]`:                                      0,
		`"seq"`:                                      0,
		`null`:                                       0,
	} {
		if seq, _, ok := ackedMessage(json.RawMessage(data)); ok != (want != 0) || seq != want {
			t.Errorf("ackedMessage(%s) = %d, %v; want %d", data, seq, ok, want)
		}
	}
	for data, want := range map[string]int64{
		`{"ack":1}`:                1,
		`{"ack":1755100000001}`:    1755100000001,
		`{"ack":0}`:                0,
		`{"ack":1.5}`:              0,
		`{"ack":"1"}`:              0,
		`{"ack":null}`:             0,
		`{"ack":1,"data":null}`:    0,
		`{"ack":9007199254740992}`: 0,
		`{}`:                       0,
		`7`:                        0,
	} {
		if seq, ok := ackMessage(json.RawMessage(data)); ok != (want != 0) || seq != want {
			t.Errorf("ackMessage(%s) = %d, %v; want %d", data, seq, ok, want)
		}
	}
}

func TestSeqsAreSeededWithTheClockAndNeverRepeat(t *testing.T) {
	before := time.Now().UnixMilli()
	c := offlineClient(t)
	a, b := c.channels.seq.next(), c.channels.seq.next()
	if a <= before || b != a+1 || b > maxSeq {
		t.Fatalf("seqs %d, %d with the clock at %d ms", a, b, before)
	}
	if seq, n, ok := parseAckedRef(ackedRef(a, 3)); !ok || seq != a || n != 3 {
		t.Fatalf("parseAckedRef round trip: %d, %d, %v", seq, n, ok)
	}
	for _, ref := range []string{"", "sub-1", "acked-", "acked-12", "acked-x.1", "acked-1.x"} {
		if _, _, ok := parseAckedRef(ref); ok {
			t.Errorf("parseAckedRef(%q) accepted", ref)
		}
	}
}

// What does and does not settle a pending send (the Resolution table).
func TestAckedResolution(t *testing.T) {
	ackFrom := func(channel, from string, seq int64) protocol.Envelope {
		data, _ := json.Marshal(map[string]int64{"ack": seq})
		return protocol.Msg(protocol.TypeChannelMessage, protocol.ChannelMessage{Channel: channel, From: from, Data: data})
	}
	errFor := func(code, ref string) protocol.Envelope {
		return protocol.Msg(protocol.TypeError, protocol.ErrorMsg{Code: code, Message: "m", Ref: ref})
	}
	const seq = 1755100000001

	ignored := []protocol.Envelope{
		ackFrom("jobs", "r_other", seq),                   // not the target
		ackFrom("other", "r_1", seq),                      // not the channel
		ackFrom("jobs", "r_1", seq+1),                     // unknown seq
		errFor(protocol.ErrRateLimited, ackedRef(seq, 1)), // that copy was dropped; re-sent later
		errFor(protocol.ErrConflict, ackedRef(seq, 1)),
		errFor(protocol.ErrNotFound, ackedRef(seq+1, 1)), // another send's transmission
		errFor(protocol.ErrNotFound, "sub-1"),
		errFor(protocol.ErrNotFound, ""),
		protocol.Msg(protocol.TypeChannelMessage, protocol.ChannelMessage{
			Channel: "jobs", From: "r_1", Data: json.RawMessage(`{"ack":1755100000001,"extra":1}`),
		}),
	}
	c := offlineClient(t)
	p := pend(c, seq, "jobs", "r_1")
	for _, env := range ignored {
		c.observeAcked(env)
		if err, resolved := outcome(p); resolved {
			t.Fatalf("%s %s settled the send with %v", env.Type, env.Payload, err)
		}
	}

	for _, tc := range []struct {
		env  protocol.Envelope
		want error
		says string
	}{
		{ackFrom("jobs", "r_1", seq), nil, ""},
		{errFor(protocol.ErrNotFound, ackedRef(seq, 1)), ErrNotFound, "not delivered"},
		{errFor(protocol.ErrNotFound, ackedRef(seq, 3)), ErrNotFound, "may have been delivered"},
		{errFor(protocol.ErrInvalidMessage, ackedRef(seq, 2)), ErrInvalidMessage, ""},
		{errFor(protocol.ErrNotAuthorized, ackedRef(seq, 1)), ErrNotAuthorized, ""},
	} {
		c := offlineClient(t)
		p := pend(c, seq, "jobs", "r_1")
		c.observeAcked(tc.env)
		err, resolved := outcome(p)
		if !resolved || !errors.Is(err, tc.want) || (tc.want == nil && err != nil) {
			t.Fatalf("%s %s: resolved %v with %v, want %v", tc.env.Type, tc.env.Payload, resolved, err, tc.want)
		}
		if tc.says != "" && !strings.Contains(err.Error(), tc.says) {
			t.Fatalf("%v does not say %q", err, tc.says)
		}
		if len(c.channels.pending) != 0 {
			t.Fatal("a settled send is still pending")
		}
		// A duplicate of what settled it is ignored, not a second outcome.
		c.observeAcked(tc.env)
	}
}

// The receiver remembers an accepted message for ten minutes and no longer.
func TestAckedReceiverMemory(t *testing.T) {
	c := offlineClient(t)
	now := time.Unix(1_755_100_000, 0)
	c.channels.now = func() time.Time { return now }
	var got []string
	fail := true
	c.Channel("jobs").OnAcked(func(from string, data json.RawMessage) error {
		got = append(got, from+" "+string(data))
		if fail {
			return errors.New("not yet")
		}
		return nil
	})
	var plain []string
	c.Channel("jobs").OnMessage(func(from string, data json.RawMessage) { plain = append(plain, string(data)) })

	deliver := func(from, data string) {
		c.dispatch(protocol.Msg(protocol.TypeChannelMessage, protocol.ChannelMessage{Channel: "jobs", From: from, Data: json.RawMessage(data)}))
	}
	deliver("s_1", `{"seq":7,"data":"a"}`) // fails: forgotten
	fail = false
	deliver("s_1", `{"seq":7,"data":"a"}`) // the re-send is new: accepted
	deliver("s_1", `{"seq":7,"data":"a"}`) // a repeat: not delivered again
	deliver("s_2", `{"seq":7,"data":"b"}`) // same seq from another sender is another message
	deliver("s_1", `{"seq":8,"data":null}`)
	now = now.Add(ackedMemory)
	deliver("s_1", `{"seq":7,"data":"a"}`) // still remembered at exactly ten minutes
	now = now.Add(time.Second)
	deliver("s_1", `{"seq":7,"data":"a"}`) // forgotten: delivered again
	deliver("s_1", `{"ack":7}`)            // an ack is not application data
	deliver("s_1", `{"seq":"7","data":1}`) // not the reserved shape: ordinary data
	deliver("s_1", `"hello"`)

	want := []string{`s_1 "a"`, `s_1 "a"`, `s_2 "b"`, `s_1 null`, `s_1 "a"`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("acked handler got %q, want %q", got, want)
	}
	if !reflect.DeepEqual(plain, []string{`{"seq":"7","data":1}`, `"hello"`}) {
		t.Fatalf("OnMessage got %q", plain)
	}
	if n := len(c.channels.seen); n != 1 || len(c.channels.seenOrder) != 1 {
		t.Fatalf("seen set holds %d keys after the window passed, want only the fresh one", n)
	}
}

func TestClosingNotice(t *testing.T) {
	for payload, want := range map[string]string{
		`{"code":"conflict","message":"replaced by a newer connection"}`:                              "conflict",
		`{"code":"rate_limited","message":"heartbeat lapsed"}`:                                        "rate_limited",
		`{"code":"auth_failed","message":"token revoked"}`:                                            "auth_failed",
		`{"code":"conflict","message":"robot is leased","ref":"claim-1","lease":{"lease_id":"ls_1"}}`: "",
		`{"code":"conflict","message":"robot is leased","lease":{"lease_id":"ls_1"}}`:                 "",
		`{"code":"rate_limited","message":"slow down","ref":"p-1"}`:                                   "",
		`{"code":"not_found","message":"channel target not connected"}`:                               "",
	} {
		got := closingNotice(protocol.Envelope{V: 0, Type: protocol.TypeError, Payload: json.RawMessage(payload)})
		if (got == nil) != (want == "") || (got != nil && got.Code != want) {
			t.Errorf("closingNotice(%s) = %v, want code %q", payload, got, want)
		}
	}
}
