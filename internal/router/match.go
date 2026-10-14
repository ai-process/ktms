// Package router picks a rule for a request and walks its model chain.
package router

import (
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
)

// Match returns the first rule (priority desc) whose predicates all hold:
// effort equal (or rule matches any) and every rule attribute pair equal to
// the request's. Returns nil only on an unconfigured or invalid snapshot —
// validation guarantees a catch-all per effort otherwise.
func Match(snap *registry.Snapshot, effort string, attrs map[string]string) *proxydb.Rule {
	for _, r := range snap.Rules {
		if r.Effort != "" && r.Effort != effort {
			continue
		}
		matched := true
		for k, v := range r.Attributes {
			if attrs[k] != v {
				matched = false
				break
			}
		}
		if matched {
			return r
		}
	}
	return nil
}

// MatchModality picks a rule for a non-text call. Unlike Match it considers
// only rules that name the modality themselves, so a text rule can never
// capture speech or image traffic just because its other predicates happen to
// hold — that would hand back a chain FilterChain then strips to nothing.
// nil means no rule is configured for this modality.
func MatchModality(snap *registry.Snapshot, effort, modalityKey, modality string, attrs map[string]string) *proxydb.Rule {
	for _, r := range snap.Rules {
		if r.Attributes[modalityKey] != modality {
			continue
		}
		if r.Effort != "" && r.Effort != effort {
			continue
		}
		matched := true
		for k, v := range r.Attributes {
			if attrs[k] != v {
				matched = false
				break
			}
		}
		if matched {
			return r
		}
	}
	return nil
}

// FilterChain narrows a rule's chain to models that can serve this request:
// right effort, google_search capability when asked for, vendor key
// held by the calling client, and all required capabilities.
func FilterChain(snap *registry.Snapshot, use []string, apiKeyID, effort string, needsGoogleSearch bool, requireCapabilities ...string) ([]*proxydb.Model, error) {
	var out []*proxydb.Model
	sawCapable := false
outer:
	for _, id := range use {
		m, ok := snap.Models[id]
		if !ok { // disabled since the rule was written
			continue
		}
		if !registry.Serves(m, effort) {
			continue
		}
		if needsGoogleSearch && !registry.HasCapability(m, registry.CapabilityGoogleSearch) {
			continue
		}
		for _, cap := range requireCapabilities {
			if cap != "" && !registry.HasCapability(m, cap) {
				continue outer
			}
		}
		sawCapable = true
		if _, hasKey := snap.VendorKey(apiKeyID, m.Vendor); !hasKey {
			continue
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		if sawCapable {
			return nil, ErrNoVendorKey
		}
		return nil, ErrNoCapableModel
	}
	return out, nil
}
