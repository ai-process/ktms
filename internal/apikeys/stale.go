package apikeys

import (
	"crypto/subtle"
	"sync"
	"time"
)

// staleMax bounds the cache; past it the oldest half is dropped. Keys that
// stop being used age out, and it only matters during an outage anyway.
const staleMax = 100000

// staleCache remembers recent successful verifications, keyed by the lookup
// hash and checked against the verify hash, so it never holds a usable secret.
type staleCache struct {
	mu     sync.Mutex
	maxAge time.Duration
	byKey  map[string]staleEntry
	now    func() time.Time
}

type staleEntry struct {
	secret string
	id     *Identity
	at     time.Time
}

func newStaleCache(maxAge time.Duration) *staleCache {
	return &staleCache{maxAge: maxAge, byKey: map[string]staleEntry{}, now: time.Now}
}

func (c *staleCache) put(lookup, secret string, id *Identity) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.byKey) >= staleMax {
		c.evictOldestLocked()
	}
	c.byKey[lookup] = staleEntry{secret: secret, id: id, at: c.now()}
}

func (c *staleCache) drop(lookup string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.byKey, lookup)
	c.mu.Unlock()
}

func (c *staleCache) get(lookup, secret string) *Identity {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.byKey[lookup]
	if !ok || c.now().Sub(e.at) > c.maxAge {
		return nil
	}
	if subtle.ConstantTimeCompare([]byte(e.secret), []byte(secret)) != 1 {
		return nil
	}
	return e.id
}

func (c *staleCache) evictOldestLocked() {
	cutoff := c.now().Add(-c.maxAge / 2)
	for k, e := range c.byKey {
		if e.at.Before(cutoff) {
			delete(c.byKey, k)
		}
	}
	if len(c.byKey) >= staleMax { // all recent: drop arbitrary entries to make room
		for k := range c.byKey {
			delete(c.byKey, k)
			if len(c.byKey) < staleMax/2 {
				break
			}
		}
	}
}
