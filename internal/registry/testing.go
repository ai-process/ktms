package registry

import (
	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
)

// NewSnapshotForTest builds a snapshot without a database — for tests only.
// vendorKeys is keyed [apiKeyID][vendor] = plain credential.
func NewSnapshotForTest(models []*proxydb.Model, rules []*proxydb.Rule,
	vendorKeys map[string]map[string]string, tracker llm.UsageTracker) *Snapshot {
	snap := &Snapshot{
		Models:     map[string]*proxydb.Model{},
		vendorKeys: vendorKeys,
		tracker:    tracker,
	}
	for _, m := range models {
		if m.Enabled {
			snap.Models[m.ID] = m
		}
	}
	for _, r := range rules {
		if r.Enabled {
			snap.Rules = append(snap.Rules, r)
		}
	}
	return snap
}

// SetAdapter stores a mock adapter for tests.
func (s *Snapshot) SetAdapter(apiKeyID, modelID string, adapter llm.ClientAdapter) {
	s.adapters.Store(apiKeyID+"\x00"+modelID, adapter)
}
