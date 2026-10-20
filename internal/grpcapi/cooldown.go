package grpcapi

import (
	"time"

	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/cooldown"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/router"
)

// WithCooldown honours vendor Retry-After by pausing models in store.
func (s *ProxyServer) WithCooldown(store *cooldown.Store) *ProxyServer {
	s.cooldown = store
	return s
}

// cooldownFor binds the store to the caller's vendor credentials. It returns a
// nil interface when pauses are off, so the router skips the lookups entirely.
func (s *ProxyServer) cooldownFor(snap *registry.Snapshot, id *apikeys.Identity) router.Cooldown {
	if s.cooldown == nil || snap == nil || id == nil {
		return nil
	}
	return vendorCooldown{store: s.cooldown, snap: snap, keyID: id.KeyID}
}

// vendorCooldown keys pauses by vendor credential and model. Local nodes are
// left out: the pool has its own admission and one busy node is not the pool.
type vendorCooldown struct {
	store *cooldown.Store
	snap  *registry.Snapshot
	keyID string
}

func (c vendorCooldown) key(m *proxydb.Model) (string, bool) {
	if m.Vendor == registry.VendorLocal {
		return "", false
	}
	return m.Vendor + "\x00" + c.snap.VendorKeyFingerprint(c.keyID, m.Vendor) + "\x00" + m.ID, true
}

func (c vendorCooldown) Remaining(m *proxydb.Model) time.Duration {
	k, ok := c.key(m)
	if !ok {
		return 0
	}
	return c.store.Remaining(k)
}

func (c vendorCooldown) Pause(m *proxydb.Model, d time.Duration) time.Duration {
	k, ok := c.key(m)
	if !ok {
		return 0
	}
	return c.store.Pause(k, d)
}
