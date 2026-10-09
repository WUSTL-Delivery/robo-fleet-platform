// Package layers retains the latest layer.declare and layer.update per
// (fleet, layer id) so a client that subscribes to `layers` late still sees
// every map layer without the owning service re-sending. Envelopes are stored
// as-is: the platform never looks inside a layer's data (DESIGN.md D4).
//
// A layer belongs to the service that last declared or updated it. When that
// service has been offline longer than the TTL, its layers are dropped; a
// reconnect within the TTL keeps them.
package layers

import (
	"sort"
	"sync"
	"time"

	"fleetplatform/sdk/go/protocol"
)

type entry struct {
	owner   string
	declare *protocol.Envelope
	update  *protocol.Envelope
}

type Store struct {
	mu      sync.Mutex
	fleets  map[string]map[string]*entry // fleetID → layerID → entry
	offline map[string]time.Time         // owner clientID → went offline at
}

func New() *Store {
	return &Store{
		fleets:  make(map[string]map[string]*entry),
		offline: make(map[string]time.Time),
	}
}

func (s *Store) get(fleetID, layerID, owner string) *entry {
	layers, ok := s.fleets[fleetID]
	if !ok {
		layers = make(map[string]*entry)
		s.fleets[fleetID] = layers
	}
	e, ok := layers[layerID]
	if !ok {
		e = &entry{}
		layers[layerID] = e
	}
	e.owner = owner
	return e
}

// retained strips the per-message id: it referred to the sender's request, not
// to anything a later subscriber asked for.
func retained(env protocol.Envelope) *protocol.Envelope {
	env.ID = ""
	return &env
}

// Declare records the latest declaration of a layer.
func (s *Store) Declare(fleetID, owner, layerID string, env protocol.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.get(fleetID, layerID, owner).declare = retained(env)
}

// Update records the latest data for a layer.
func (s *Store) Update(fleetID, owner, layerID string, env protocol.Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.get(fleetID, layerID, owner).update = retained(env)
}

// Replay returns the retained envelopes for a fleet, ordered by layer id, each
// layer's declare before its update.
func (s *Store) Replay(fleetID string) []protocol.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	layers := s.fleets[fleetID]
	ids := make([]string, 0, len(layers))
	for id := range layers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []protocol.Envelope
	for _, id := range ids {
		e := layers[id]
		if e.declare != nil {
			out = append(out, *e.declare)
		}
		if e.update != nil {
			out = append(out, *e.update)
		}
	}
	return out
}

// OwnerUp marks a service online again, cancelling any pending expiry.
func (s *Store) OwnerUp(owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.offline, owner)
}

// OwnerDown starts the offline clock for a service's layers.
func (s *Store) OwnerDown(owner string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.offline[owner] = now
}

// Sweep drops every layer whose owner has been offline for longer than ttl and
// returns how many were dropped.
func (s *Store) Sweep(now time.Time, ttl time.Duration) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	expired := make(map[string]bool)
	for owner, at := range s.offline {
		if now.Sub(at) > ttl {
			expired[owner] = true
			delete(s.offline, owner)
		}
	}
	if len(expired) == 0 {
		return 0
	}
	dropped := 0
	for fleetID, layers := range s.fleets {
		for id, e := range layers {
			if expired[e.owner] {
				delete(layers, id)
				dropped++
			}
		}
		if len(layers) == 0 {
			delete(s.fleets, fleetID)
		}
	}
	return dropped
}
