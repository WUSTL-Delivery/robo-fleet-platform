// Command dispatcher is a small Go service on fleet-server: it keeps a world model
// from callbacks, picks a robot that is online and AUTONOMOUS, hands it a job with
// an acked send on the "jobs" channel, and requeues the job when that fails.
// Env: FLEET_URL, FLEET_ENROLL_KEY (first run only), FLEET_TOKEN_FILE.
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"fleetplatform/sdk/go/fleet"
	"fleetplatform/sdk/go/protocol"
)

type world struct { // what the dispatcher knows about each robot, by robot id
	sync.Mutex
	online map[string]bool
	away   map[string]bool // not AUTONOMOUS: an operator is driving it, or it waits for help
}

// reset replaces the model. Every snapshot is the whole truth: the answer to
// Subscribe, and a fresh one after each reconnect. Events then keep it current.
func (w *world) reset(s fleet.Snapshot) {
	w.Lock()
	defer w.Unlock()
	w.online, w.away = map[string]bool{}, map[string]bool{}
	for _, r := range s.Robots {
		w.online[r.RobotID] = r.Presence == "online"
		w.away[r.RobotID] = r.State != protocol.StateAutonomous
	}
	log.Printf("snapshot: %d robots", len(s.Robots))
}

// pick returns a robot a job may go to: online and AUTONOMOUS, never one in TELEOP
// or HELP_REQUESTED. (One that enrolled after the last snapshot starts AUTONOMOUS.)
func (w *world) pick() (string, error) {
	w.Lock()
	defer w.Unlock()
	for id, on := range w.online {
		if on && !w.away[id] {
			return id, nil
		}
	}
	return "", errors.New("no robot is online and AUTONOMOUS")
}

func main() {
	ctx := context.Background()
	client, err := fleet.Connect(ctx, fleet.Config{ // enrolls once, then reconnects by itself
		URL:       cmp.Or(os.Getenv("FLEET_URL"), "ws://localhost:8080/ws"),
		Kind:      fleet.Service,
		Name:      "dispatcher",
		EnrollKey: os.Getenv("FLEET_ENROLL_KEY"), // used once; the token file after that
		TokenFile: cmp.Or(os.Getenv("FLEET_TOKEN_FILE"), os.ExpandEnv("$HOME/.fleet/dispatcher.json")),
	})
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	w := &world{online: map[string]bool{}, away: map[string]bool{}}
	client.OnSnapshot(w.reset)
	client.OnPresence(func(id string, online bool) { w.Lock(); w.online[id] = online; w.Unlock() })
	client.OnLease(func(c fleet.LeaseChange) { // c.State is where the change leaves the robot
		w.Lock()
		w.away[c.RobotID] = c.State != protocol.StateAutonomous
		w.Unlock()
	})
	client.OnHelp(func(id string, _ fleet.HelpDetails) { w.Lock(); w.away[id] = true; w.Unlock() })
	if _, err := client.Subscribe(ctx, fleet.TopicPresence, fleet.TopicEvents); err != nil {
		log.Fatalf("subscribe: %v", err)
	}

	// A job is opaque to the platform. "id" lets a robot spot a repeat: a failed send may have arrived.
	jobs := client.Channel("jobs")
	var queue []map[string]string
	for n := 1; client.Err() == nil; n++ { // a new job every 2 s; one that fails stays at the head
		time.Sleep(2 * time.Second)
		queue = append(queue, map[string]string{"id": fmt.Sprintf("job-%d", n), "task": "inspect"})
		for len(queue) > 0 {
			id, err := w.pick()
			if err == nil {
				// Re-sent until the robot's application accepts it, the server says the robot is gone, or 5 s pass.
				err = jobs.SendAcked(ctx, id, queue[0], 5*time.Second)
			}
			if err != nil {
				log.Printf("requeue %s (%d waiting): %v", queue[0]["id"], len(queue), err)
				break
			}
			log.Printf("acked %s by %s", queue[0]["id"], id)
			queue = queue[1:]
		}
	}
	log.Fatalf("client closed: %v", client.Err()) // terminal: token revoked, or the identity taken over
}
