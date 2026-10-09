package gateway

import (
	"time"

	"fleetplatform/sdk/go/protocol"
)

// RateLimit caps how fast one connection may send the chatty message types
// (DESIGN.md D2: the control plane stays light by law). Each limited type has
// its own token bucket per connection, so a runaway telemetry loop cannot
// starve the same client's channel publishes. Over-limit messages are dropped
// and the socket stays open: throttling, not disconnecting.
//
// The zero value disables limiting.
type RateLimit struct {
	PerSec float64 // sustained messages per second, per type
	Burst  int     // bucket size: messages allowed back to back
}

func (r RateLimit) enabled() bool { return r.PerSec > 0 && r.Burst > 0 }

// rateLimitedTypes are the message types metered per connection. Everything
// else (heartbeats, leases, subscribe, signaling) is low-rate by construction
// and must never be throttled: a dropped heartbeat would take a robot offline.
var rateLimitedTypes = map[string]bool{
	protocol.TypeTelemetry:      true,
	protocol.TypeChannelPublish: true,
}

// throttleNoticeEvery bounds the error replies a throttled client gets. One
// error per dropped message would turn a flood into an equal flood back into
// the client's own send queue and trip the overflow disconnect this exists to
// avoid; latest-wins data (D8) loses nothing a notice per second does not say.
const throttleNoticeEvery = time.Second

// bucket is a token bucket. It is owned by one connection's read loop, so it
// needs no lock.
type bucket struct {
	tokens float64
	last   time.Time
}

// limiter is one connection's set of buckets plus its notice pacing.
type limiter struct {
	cfg        RateLimit
	buckets    map[string]*bucket
	lastNotice time.Time
}

func newLimiter(cfg RateLimit) *limiter {
	if !cfg.enabled() {
		return nil
	}
	return &limiter{cfg: cfg, buckets: make(map[string]*bucket)}
}

// allow reports whether a message of type typ may pass at now. A nil limiter
// allows everything.
func (l *limiter) allow(typ string, now time.Time) bool {
	if l == nil || !rateLimitedTypes[typ] {
		return true
	}
	b := l.buckets[typ]
	if b == nil {
		b = &bucket{tokens: float64(l.cfg.Burst), last: now}
		l.buckets[typ] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * l.cfg.PerSec
	if max := float64(l.cfg.Burst); b.tokens > max {
		b.tokens = max
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// notify reports whether a drop at now should be answered with an error, and
// if so starts a new notice window.
func (l *limiter) notify(now time.Time) bool {
	if !l.lastNotice.IsZero() && now.Sub(l.lastNotice) < throttleNoticeEvery {
		return false
	}
	l.lastNotice = now
	return true
}
