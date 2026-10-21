package apikeys

import (
	"sync"
	"time"
)

// staleMax bounds the cache; a full cache is cut back to staleLow at once, so
// the scan is paid once per many inserts rather than on every one.
const (
	staleMax = 100000
	staleLow = staleMax * 3 / 4
)

// staleCache remembers recent successful verifications by lookup hash, a
// SHA-256 of the whole key: a hit can only be the very key that verified.
type staleCache struct {
	mu     sync.RWMutex
	maxAge time.Duration
	byKey  map[string]staleEntry
	now    func() time.Time
}

type staleEntry struct {
	id *Identity
	at time.Time
}

func newStaleCache(maxAge time.Duration) *staleCache {
	return &staleCache{maxAge: maxAge, byKey: map[string]staleEntry{}, now: time.Now}
}

// put refreshes an entry. Most calls find a fresh one under the read lock and
// skip the write, so the hot auth path does not serialise on this cache.
func (c *staleCache) put(lookup string, id *Identity) {
	if c == nil {
		return
	}
	now := c.now()
	c.mu.RLock()
	e, ok := c.byKey[lookup]
	c.mu.RUnlock()
	if ok && now.Sub(e.at) < c.maxAge/10 && e.id.KeyID == id.KeyID {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.byKey) >= staleMax {
		c.shrinkLocked(now)
	}
	c.byKey[lookup] = staleEntry{id: id, at: now}
}

func (c *staleCache) drop(lookup string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.byKey, lookup)
	c.mu.Unlock()
}

func (c *staleCache) get(lookup string) *Identity {
	if c == nil {
		return nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.byKey[lookup]
	if !ok || c.now().Sub(e.at) > c.maxAge {
		return nil
	}
	return e.id
}

// shrinkLocked drops expired entries, then arbitrary ones down to staleLow.
func (c *staleCache) shrinkLocked(now time.Time) {
	for k, e := range c.byKey {
		if now.Sub(e.at) > c.maxAge {
			delete(c.byKey, k)
		}
	}
	for k := range c.byKey {
		if len(c.byKey) <= staleLow {
			return
		}
		delete(c.byKey, k)
	}
}
