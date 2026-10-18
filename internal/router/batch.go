package router

import (
	"context"

	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

// ResolveBatchModel resolves the route once for a batch submission:
// picks the first model in the rule's chain that has the batch capability,
// a vendor key for the client, and daily budget left.
func ResolveBatchModel(ctx context.Context, snap *registry.Snapshot, th Throttle, use []string, apiKeyID, keyName, userID string) (*proxydb.Model, error) {
	sawCapable := false
	sawKey := false
	lastTripped := ""

	for _, id := range use {
		m, ok := snap.Models[id]
		if !ok || !m.Enabled {
			continue
		}
		if !registry.HasCapability(m, registry.CapabilityBatch) {
			continue
		}
		sawCapable = true

		if !snap.CanCall(apiKeyID, m) {
			continue
		}
		sawKey = true

		if th != nil {
			tripped := th.CheckDailyBudget(ctx, m, keyName, userID)
			if tripped != "" {
				lastTripped = tripped
				continue
			}
		}

		return m, nil
	}

	if sawKey && lastTripped != "" {
		return nil, &ChainExhausted{AllThrottled: true, ThrottleKind: lastTripped}
	}
	if sawCapable && !sawKey {
		return nil, ErrNoVendorKey
	}
	return nil, ErrNoCapableModel
}
