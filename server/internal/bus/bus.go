// Package bus owns subscriptions and the control-plane limits. Payloads are
// bytes to us — no domain imports EVER (DESIGN.md D10); heavy data rides the
// data plane, and the limit here is the law that keeps it that way.
package bus

import "sync"

// MaxPayloadBytes bounds any single control-plane payload (video never rides
// this pipe; ~1 KB/s per robot steady state is the design point).
const MaxPayloadBytes = 64 * 1024

// Well-known topics. Domain channels subscribe as "channel:<name>".
const (
	TopicPresence  = "presence"
	TopicEvents    = "events"
	TopicTelemetry = "telemetry"
	TopicLayers    = "layers"
	ChannelPrefix  = "channel:"
)

type Bus struct {
	mu   sync.Mutex
	subs map[string]map[string]bool // clientID → topic set
}

func New() *Bus {
	return &Bus{subs: make(map[string]map[string]bool)}
}

func (b *Bus) Subscribe(clientID string, topics []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	set, ok := b.subs[clientID]
	if !ok {
		set = make(map[string]bool)
		b.subs[clientID] = set
	}
	for _, t := range topics {
		set[t] = true
	}
}

func (b *Bus) Drop(clientID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, clientID)
}

// SubscribersOf returns the client ids subscribed to a topic.
func (b *Bus) SubscribersOf(topic string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for id, set := range b.subs {
		if set[topic] {
			out = append(out, id)
		}
	}
	return out
}
