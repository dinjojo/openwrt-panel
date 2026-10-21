// Package events keeps a small ring of notable network happenings — WAN
// session changes, WARP tunnel drops and recoveries, router reboots and
// bufferbloat probe results — so the Overview can show a "what happened"
// feed instead of making the user reconstruct the timeline from charts.
// The ring persists to disk so events survive monitor restarts.
package events

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Event is one notable occurrence.
type Event struct {
	At   int64  `json:"at"`  // unix seconds
	Kind string `json:"kind"` // wan | warp | reboot | probe | bloat | conntrack | config | monitor
	Sev  string `json:"sev"`  // ok | warn | crit | info
	Msg  string `json:"msg"`
}

// ringCap is generous on purpose: 1000 entries is still ~100 KB on disk, and
// the feed must hold a week of WARP flaps and probe surprises without the
// early history being pushed out.
const ringCap = 1000

// Store is a persistent, newest-first event ring.
type Store struct {
	mu     sync.Mutex
	events []Event
	path   string
}

// New loads any persisted events and returns the store.
func New(path string) *Store {
	s := &Store{path: path}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &s.events)
	}
	return s
}

// Add appends an event (newest first), persists the ring and drops the tail
// beyond the capacity.
func (s *Store) Add(kind, sev, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append([]Event{{At: time.Now().Unix(), Kind: kind, Sev: sev, Msg: msg}}, s.events...)
	if len(s.events) > ringCap {
		s.events = s.events[:ringCap]
	}
	if s.path != "" {
		if b, err := json.Marshal(s.events); err == nil {
			_ = os.MkdirAll("/opt/monitoring", 0o755)
			tmp := s.path + ".tmp"
			if os.WriteFile(tmp, b, 0o644) == nil {
				_ = os.Rename(tmp, s.path)
			}
		}
	}
}

// List returns up to n most recent events.
func (s *Store) List(n int) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > len(s.events) {
		n = len(s.events)
	}
	out := make([]Event, n)
	copy(out, s.events[:n])
	return out
}

// CountSince counts events of one kind newer than the given unix second.
// Used for "N reconnects in the last 24 h / 7 d" style figures.
func (s *Store) CountSince(kind string, since int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, ev := range s.events {
		if ev.Kind == kind && ev.At >= since {
			n++
		}
	}
	return n
}
