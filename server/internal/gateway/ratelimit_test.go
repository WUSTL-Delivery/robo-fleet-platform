package gateway

import (
	"testing"
	"time"

	"fleetplatform/sdk/go/protocol"
)

func TestRateLimitBucketBurstThenRefill(t *testing.T) {
	l := newLimiter(RateLimit{PerSec: 10, Burst: 5})
	t0 := time.Unix(1000, 0)

	for i := 0; i < 5; i++ {
		if !l.allow(protocol.TypeTelemetry, t0) {
			t.Fatalf("message %d within burst was dropped", i)
		}
	}
	if l.allow(protocol.TypeTelemetry, t0) {
		t.Fatal("message past burst was allowed")
	}
	// 10/s refills one token every 100 ms.
	if !l.allow(protocol.TypeTelemetry, t0.Add(100*time.Millisecond)) {
		t.Fatal("refilled token was not granted")
	}
	if l.allow(protocol.TypeTelemetry, t0.Add(100*time.Millisecond)) {
		t.Fatal("bucket granted more than it refilled")
	}
	// A long idle refills to burst, never past it.
	later := t0.Add(time.Hour)
	for i := 0; i < 5; i++ {
		if !l.allow(protocol.TypeTelemetry, later) {
			t.Fatalf("message %d after idle was dropped", i)
		}
	}
	if l.allow(protocol.TypeTelemetry, later) {
		t.Fatal("bucket overfilled past burst")
	}
}

func TestRateLimitPerTypeBucketsAndExemptTypes(t *testing.T) {
	l := newLimiter(RateLimit{PerSec: 1, Burst: 1})
	now := time.Unix(1000, 0)

	if !l.allow(protocol.TypeTelemetry, now) || l.allow(protocol.TypeTelemetry, now) {
		t.Fatal("telemetry bucket should pass one then drop")
	}
	// Exhausted telemetry does not starve channel publishes.
	if !l.allow(protocol.TypeChannelPublish, now) {
		t.Fatal("channel.publish shares telemetry's bucket")
	}
	// Heartbeats and everything unmetered always pass.
	for i := 0; i < 100; i++ {
		if !l.allow(protocol.TypeHeartbeat, now) || !l.allow(protocol.TypeLeaseRenew, now) {
			t.Fatal("unmetered type was throttled")
		}
	}
}

func TestRateLimitDisabledAndNoticePacing(t *testing.T) {
	var off *limiter = newLimiter(RateLimit{})
	if off != nil {
		t.Fatal("zero RateLimit should disable limiting")
	}
	if !off.allow(protocol.TypeTelemetry, time.Now()) {
		t.Fatal("nil limiter must allow")
	}

	l := newLimiter(RateLimit{PerSec: 1, Burst: 1})
	t0 := time.Unix(1000, 0)
	if !l.notify(t0) {
		t.Fatal("first drop should be noticed")
	}
	if l.notify(t0.Add(throttleNoticeEvery - time.Millisecond)) {
		t.Fatal("second notice inside the window")
	}
	if !l.notify(t0.Add(throttleNoticeEvery)) {
		t.Fatal("notice after the window was suppressed")
	}
}
