// Package registry tracks who is present and what they declared. Presence is
// heartbeat-based with expiry, never socket-lifecycle (DESIGN.md D6 lesson), and
// applies to every client class: robots, services, operators alike.
package registry

import (
	"sync"
	"time"

	"fleetplatform/server/internal/protocol"
	"fleetplatform/server/internal/store"
)

type Entry struct {
	Client   store.Client
	Manifest *protocol.Manifest
	LastSeen time.Time
}

type Registry struct {
	mu      sync.Mutex
	entries map[string]*Entry // clientID → live entry
}

func New() *Registry {
	return &Registry{entries: make(map[string]*Entry)}
}

func (r *Registry) Up(c store.Client, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[c.ID] = &Entry{Client: c, LastSeen: now}
}

func (r *Registry) Heartbeat(clientID string, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[clientID]; ok {
		e.LastSeen = now
	}
}

func (r *Registry) SetManifest(clientID string, m *protocol.Manifest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[clientID]; ok {
		e.Manifest = m
	}
}

func (r *Registry) Get(clientID string) (Entry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[clientID]; ok {
		return *e, true
	}
	return Entry{}, false
}

func (r *Registry) Down(clientID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.entries[clientID]
	delete(r.entries, clientID)
	return ok
}

// SweepStale removes entries whose heartbeat lapsed past ttl and returns them.
func (r *Registry) SweepStale(now time.Time, ttl time.Duration) []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Entry
	for id, e := range r.entries {
		if now.Sub(e.LastSeen) > ttl {
			out = append(out, *e)
			delete(r.entries, id)
		}
	}
	return out
}
