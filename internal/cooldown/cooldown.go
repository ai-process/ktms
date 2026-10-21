// Package cooldown holds back upstream targets whose vendor answered 429 or 503
// with Retry-After, so routing skips them without network IO until it expires.
package cooldown

import (
	"sync"
	"sync/atomic"
	"time"
)

// Expired entries are dropped at most once per sweepEvery, so a 429 storm does
// not turn every call into a scan under the lock, and Idle recovers soon after.
const sweepEvery = time.Second

// Store is a per-replica set of pauses. Like the circuit breaker it is not
// shared: each replica pays at most one rejected call per pause window.
type Store struct {
	mu        sync.Mutex
	until     map[string]time.Time
	lastSweep time.Time
	live      atomic.Int64 // len(until), read without the lock on the hot path
	max       time.Duration
	now       func() time.Time
}

// New returns a store that honours Retry-After up to max, or nil when max is
// not positive, which disables pauses.
func New(max time.Duration) *Store {
	if max <= 0 {
		return nil
	}
	return &Store{until: map[string]time.Time{}, max: max, now: time.Now}
}

// Pause holds key off for d, capped at the configured maximum. It never
// shortens a pause already running and returns the one now in force.
func (s *Store) Pause(key string, d time.Duration) time.Duration {
	if s == nil || d <= 0 {
		return 0
	}
	if d > s.max {
		d = s.max
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	until := now.Add(d)
	if cur, ok := s.until[key]; ok && cur.After(until) {
		until = cur
	}
	s.until[key] = until
	s.sweepLocked(now)
	return until.Sub(now)
}

// sweepLocked drops expired pauses, including those of keys never asked about
// again (a rotated vendor key), and refreshes the lock-free live count.
func (s *Store) sweepLocked(now time.Time) {
	if now.Sub(s.lastSweep) >= sweepEvery {
		s.lastSweep = now
		for k, t := range s.until {
			if !t.After(now) {
				delete(s.until, k)
			}
		}
	}
	s.live.Store(int64(len(s.until)))
}

// Idle reports that nothing is paused, letting callers skip building keys.
func (s *Store) Idle() bool {
	return s == nil || s.live.Load() == 0
}

// Remaining is how long key stays paused, zero when it is not.
func (s *Store) Remaining(key string) time.Duration {
	if s.Idle() {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	defer s.sweepLocked(now)
	until, ok := s.until[key]
	if !ok {
		return 0
	}
	d := until.Sub(now)
	if d <= 0 {
		delete(s.until, key)
		return 0
	}
	return d
}
