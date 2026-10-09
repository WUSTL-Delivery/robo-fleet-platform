package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"fleetplatform/sdk/go/protocol"
	"fleetplatform/server/internal/app"
	"fleetplatform/server/internal/gateway"
	"fleetplatform/server/internal/store"
	"fleetplatform/server/internal/web"
)

// newRateLimitedHarness is newHarness with the gateway's per-client rate limit
// switched on (fleet-server wires it from config the same way).
func newRateLimitedHarness(t *testing.T, cfg app.Config, rl gateway.RateLimit) *harness {
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

	gw := a.Gateway()
	gw.RateLimit = rl
	srv := httptest.NewServer(web.Handler(gw))
	t.Cleanup(srv.Close)
	return &harness{t: t, url: "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws", store: st, fleet: fleet}
}

// rawSendRateLimit writes one envelope with an id, from any goroutine.
func rawSendRateLimit(c *client, typ, id string, payload any) error {
	env := protocol.Msg(typ, payload)
	env.ID = id
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return c.ws.Write(ctx, websocket.MessageText, data)
}

// A robot flooding telemetry at 10x its limit is throttled, not dropped: it
// gets rate_limited errors naming the dropped messages, the excess never fans
// out, its socket stays open, and its heartbeats keep it online throughout.
func TestIntegrationTelemetryFloodThrottledNotDisconnected(t *testing.T) {
	const perSec, burst = 20, 20
	cfg := defaultConfig()
	cfg.HeartbeatInterval = 300 * time.Millisecond // lapse after 750 ms of silence
	h := newRateLimitedHarness(t, cfg, gateway.RateLimit{PerSec: perSec, Burst: burst})

	svcToken, _, err := h.store.CreateToken(h.fleet.ID, store.KindService, "watcher")
	if err != nil {
		t.Fatal(err)
	}
	robotToken, _, err := h.store.CreateToken(h.fleet.ID, store.KindRobot, "chatty-01")
	if err != nil {
		t.Fatal(err)
	}

	service := h.connect(svcToken)
	service.send(protocol.TypeSubscribe, protocol.Subscribe{Topics: []string{"presence", "telemetry"}})
	service.expect(protocol.TypeSnapshot)
	robot := h.connect(robotToken)
	service.expectEvent(protocol.EventRobotOnline)

	// Both clients heartbeat on schedule for the whole test.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, c := range []*client{service, robot} {
		wg.Add(1)
		go func(c *client) {
			defer wg.Done()
			tk := time.NewTicker(cfg.HeartbeatInterval / 3)
			defer tk.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tk.C:
					_ = rawSendRateLimit(c, protocol.TypeHeartbeat, "", struct{}{})
				}
			}
		}(c)
	}
	defer func() { close(stop); wg.Wait() }()

	// The watcher counts fanned-out telemetry and watches for robot.offline.
	var fanned, offline atomic.Int64
	lastTelemetry := make(chan float64, 1024)
	go func() {
		for {
			env, err := service.recv()
			if err != nil {
				return
			}
			if env.Type != protocol.TypeEvent {
				continue
			}
			var ev protocol.Event
			if json.Unmarshal(env.Payload, &ev) != nil {
				continue
			}
			switch ev.Event {
			case protocol.EventRobotOffline:
				offline.Add(1)
			case protocol.EventRobotTelemetry:
				fanned.Add(1)
				var tel protocol.Telemetry
				if json.Unmarshal(ev.Data, &tel) == nil && tel.Pose != nil && tel.Pose.XM != nil {
					lastTelemetry <- *tel.Pose.XM
				}
			}
		}
	}()

	// The robot collects the errors it is sent.
	type robotErr struct{ code, ref string }
	robotErrs := make(chan robotErr, 1024)
	robotReadErr := make(chan error, 1)
	go func() {
		for {
			env, err := robot.recv()
			if err != nil {
				// recv's own 5 s timeout ends an idle read; only a closed socket counts.
				if !strings.Contains(err.Error(), "deadline") {
					robotReadErr <- err
				}
				return
			}
			if env.Type == protocol.TypeError {
				var e protocol.ErrorMsg
				_ = json.Unmarshal(env.Payload, &e)
				robotErrs <- robotErr{e.Code, e.Ref}
			}
		}
	}()

	// Flood: 10x the limit for 1.5 s, twice the heartbeat lapse window.
	const floodRate, floodFor = 10 * perSec, 1500 * time.Millisecond
	sent := 0
	start := time.Now()
	tk := time.NewTicker(time.Second / floodRate)
	for time.Since(start) < floodFor {
		<-tk.C
		x := float64(sent)
		if err := rawSendRateLimit(robot, protocol.TypeTelemetry, fmt.Sprintf("flood-%d", sent), protocol.Telemetry{
			Pose: &protocol.Pose{Frame: "local", FrameID: "map", XM: &x, YM: &x},
		}); err != nil {
			t.Fatalf("robot socket closed mid-flood after %d sends: %v", sent, err)
		}
		sent++
	}
	tk.Stop()
	elapsed := time.Since(start)

	// Let the bucket refill, then one well-behaved message must get through:
	// the socket is open and the robot is not being punished for the past.
	time.Sleep(time.Second)
	final := float64(1e6)
	if err := rawSendRateLimit(robot, protocol.TypeTelemetry, "after-flood", protocol.Telemetry{
		Pose: &protocol.Pose{Frame: "local", FrameID: "map", XM: &final, YM: &final},
	}); err != nil {
		t.Fatalf("robot socket closed after flood: %v", err)
	}
	deadline := time.After(3 * time.Second)
wait:
	for {
		select {
		case x := <-lastTelemetry:
			if x == final {
				break wait
			}
		case err := <-robotReadErr:
			t.Fatalf("robot socket closed: %v", err)
		case <-deadline:
			t.Fatal("post-flood telemetry never fanned out")
		}
	}

	// Errors: rate_limited, each naming a dropped flood message, and paced
	// (about one per second), not one per drop.
	var errs []robotErr
drain:
	for {
		select {
		case e := <-robotErrs:
			errs = append(errs, e)
		default:
			break drain
		}
	}
	if len(errs) == 0 {
		t.Fatal("no rate_limited error reached the robot")
	}
	for _, e := range errs {
		if e.code != protocol.ErrRateLimited || !strings.HasPrefix(e.ref, "flood-") {
			t.Fatalf("unexpected error %+v", e)
		}
	}
	if maxNotices := int(elapsed/time.Second) + 2; len(errs) > maxNotices {
		t.Fatalf("%d error notices for a %v flood, want <= %d", len(errs), elapsed, maxNotices)
	}

	// Throttled for real: fan-out stayed near the limit, far below what was sent.
	allowed := burst + int(elapsed.Seconds()*perSec) + 2 // +1 post-flood, +1 timing slack
	if got := int(fanned.Load()); got > allowed || got >= sent/2 {
		t.Fatalf("fanned out %d of %d telemetry, limit allows ~%d", got, sent, allowed)
	}

	// Heartbeats were never throttled: the robot never went offline.
	if n := offline.Load(); n != 0 {
		t.Fatalf("robot went offline %d time(s) during the flood", n)
	}
	select {
	case err := <-robotReadErr:
		t.Fatalf("robot socket closed: %v", err)
	default:
	}
	t.Logf("sent %d telemetry in %v; %d fanned out, %d rate_limited notices", sent, elapsed.Round(time.Millisecond), fanned.Load(), len(errs))
}
