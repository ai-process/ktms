// Package router picks a rule for a request and walks its model chain.
package router

import (
	"crypto/rand"
	"hash/fnv"
	"math/big"

	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

// CanaryKey extracts the stickiness key for canary routing from request attributes.
// When canary_mode is "random", stickiness is disabled.
// Otherwise, it checks for an explicit "canary_key", falling back to "user_id".
func CanaryKey(attrs map[string]string) string {
	if attrs == nil || attrs["canary_mode"] == "random" {
		return ""
	}
	if k := attrs["canary_key"]; k != "" {
		return k
	}
	return attrs["user_id"]
}

// SanitizeAttributes strips reserved internal attributes (shadow, is_shadow, shadow_eval)
// to prevent client forgery of billing exemption or automated evaluation.
func SanitizeAttributes(attrs map[string]string) map[string]string {
	if attrs == nil {
		return nil
	}
	cleaned := make(map[string]string, len(attrs))
	for k, v := range attrs {
		if k == "shadow" || k == "is_shadow" || k == "shadow_eval" {
			continue
		}
		cleaned[k] = v
	}
	return cleaned
}

// ResolveChain returns the model chain for a rule, selecting the primary
// candidate via weights when defined while retaining remaining fallbacks in order.
// If a non-empty hashKey is provided, canary distribution is deterministic per key.
func ResolveChain(rule *proxydb.Rule, hashKey string) []string {
	if rule == nil {
		return nil
	}
	if len(rule.Use) <= 1 || len(rule.Weights) != len(rule.Use) {
		return rule.Use
	}

	var totalWeight int64
	for _, w := range rule.Weights {
		if w > 0 {
			totalWeight += int64(w)
		}
	}
	if totalWeight <= 0 {
		return rule.Use
	}

	var val int64
	if hashKey != "" {
		h := fnv.New64a()
		_, _ = h.Write([]byte(rule.Name + ":" + hashKey))
		val = int64(h.Sum64() % uint64(totalWeight))
	} else {
		n, err := rand.Int(rand.Reader, big.NewInt(totalWeight))
		if err != nil {
			return rule.Use
		}
		val = n.Int64()
	}
	var cumulative int64
	selectedIdx := 0
	for i, w := range rule.Weights {
		if w > 0 {
			cumulative += int64(w)
			if val < cumulative {
				selectedIdx = i
				break
			}
		}
	}

	chain := make([]string, 0, len(rule.Use))
	chain = append(chain, rule.Use[selectedIdx])
	for i, m := range rule.Use {
		if i != selectedIdx {
			chain = append(chain, m)
		}
	}
	return chain
}

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
// right effort, google_search capability when asked for, a vendor key
// where the vendor needs one, and all required capabilities.
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
		if !snap.CanCall(apiKeyID, m) {
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
