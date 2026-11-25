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

// SanitizeAttributes strips reserved internal attributes (shadow, is_shadow, shadow_eval, org_id, team_id,
// data_classification) to prevent client forgery of billing exemption, automated evaluation, tenant
// impersonation, or a restricted mark on a call that was not.
func SanitizeAttributes(attrs map[string]string) map[string]string {
	if attrs == nil {
		return nil
	}
	cleaned := make(map[string]string, len(attrs))
	for k, v := range attrs {
		if k == "shadow" || k == "is_shadow" || k == "shadow_eval" || k == "org_id" || k == "team_id" || k == "data_classification" {
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

// ruleTakes reports whether a rule's need predicates hold for a request: its
// deprecated effort equal to the request's general level, as in 1.6, and each
// of its needs reached at least at the rule's level.
func ruleTakes(r *proxydb.Rule, needs map[string]string) bool {
	if r.Effort != "" && needs[registry.TopicGeneral] != registry.EffortLevel(r.Effort) {
		return false
	}
	return registry.RuleMatchesNeeds(r.Needs, needs)
}

// Match returns the first rule (priority desc) whose predicates all hold:
// its effort and needs (see ruleTakes) and every rule attribute pair equal to
// the request's. nil means no rule takes the request: selection routes it
// when models declare competencies, else the snapshot is not configured.
func Match(snap *registry.Snapshot, needs map[string]string, attrs map[string]string) *proxydb.Rule {
	for _, r := range snap.Rules {
		if !ruleTakes(r, needs) {
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
// A modality call without needs is matched by rules without them.
func MatchModality(snap *registry.Snapshot, needs map[string]string, modalityKey, modality string, attrs map[string]string) *proxydb.Rule {
	for _, r := range snap.Rules {
		if r.Attributes[modalityKey] != modality {
			continue
		}
		if len(needs) > 0 && !ruleTakes(r, needs) {
			continue
		}
		if len(needs) == 0 && (len(r.Needs) > 0 || r.Effort != "" && r.Effort != registry.EffortLow) {
			continue // as in 1.6, where speech and images routed at low
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

// Why ExplainChain drops a model of a rule's chain.
const (
	DropNotFound    = "not_found"
	DropEffort      = "effort" // the general need, effort included
	DropNeeds       = "needs"  // a need of another topic
	DropCapability  = "capability"
	DropNoVendorKey = "no_vendor_key"
)

// Candidate is one model of a chain and why it was dropped ("" if kept).
// Model is nil when the chain names a model the snapshot lacks. Excess and
// Price are what selection ranked a kept model by.
type Candidate struct {
	ID      string
	Model   *proxydb.Model
	Dropped string
	Excess  int
	Price   float64
}

// needsDrop says why m fails needs, "" if it meets them all.
func needsDrop(m *proxydb.Model, needs map[string]string) string {
	if l, ok := needs[registry.TopicGeneral]; ok && !registry.Meets(m, registry.TopicGeneral, l) {
		return DropEffort
	}
	if !registry.MeetsAll(m, needs) {
		return DropNeeds
	}
	return ""
}

// walkChain is the one pass FilterChain and ExplainChain share, so a dry run
// cannot drift from a call; dropped is "" for a model that stays.
func walkChain(snap *registry.Snapshot, use []string, apiKeyID string, needs map[string]string, needsGoogleSearch bool, caps []string, visit func(id string, m *proxydb.Model, dropped string)) {
	for _, id := range use {
		m, ok := snap.Models[id]
		if !ok { // disabled since the rule was written
			visit(id, nil, DropNotFound)
			continue
		}
		dropped := needsDrop(m, needs)
		switch {
		case dropped != "":
		case needsGoogleSearch && !registry.HasCapability(m, registry.CapabilityGoogleSearch):
			dropped = DropCapability
		}
		for _, c := range caps {
			if dropped == "" && c != "" && !registry.HasCapability(m, c) {
				dropped = DropCapability
			}
		}
		if dropped == "" && !snap.CanCall(apiKeyID, m) {
			dropped = DropNoVendorKey
		}
		visit(id, m, dropped)
	}
}

// ExplainChain says what FilterChain does with each model of the chain.
func ExplainChain(snap *registry.Snapshot, use []string, apiKeyID string, needs map[string]string, needsGoogleSearch bool, requireCapabilities ...string) []Candidate {
	out := make([]Candidate, 0, len(use))
	walkChain(snap, use, apiKeyID, needs, needsGoogleSearch, requireCapabilities, func(id string, m *proxydb.Model, dropped string) {
		out = append(out, Candidate{ID: id, Model: m, Dropped: dropped})
	})
	return out
}

// Kept returns the models ExplainChain kept, or the error a call would get
// when none is left.
func Kept(cands []Candidate) ([]*proxydb.Model, error) {
	var out []*proxydb.Model
	sawCapable := false
	for _, c := range cands {
		switch c.Dropped {
		case "":
			out = append(out, c.Model)
			sawCapable = true
		case DropNoVendorKey:
			sawCapable = true
		}
	}
	return out, chainErr(len(out), sawCapable)
}

func chainErr(kept int, sawCapable bool) error {
	switch {
	case kept > 0:
		return nil
	case sawCapable:
		return ErrNoVendorKey
	}
	return ErrNoCapableModel
}

// FilterChain narrows a rule's chain to models that can serve this request:
// meeting its needs, google_search capability when asked for, a vendor key
// where the vendor needs one, and all required capabilities.
func FilterChain(snap *registry.Snapshot, use []string, apiKeyID string, needs map[string]string, needsGoogleSearch bool, requireCapabilities ...string) ([]*proxydb.Model, error) {
	var out []*proxydb.Model
	sawCapable := false
	walkChain(snap, use, apiKeyID, needs, needsGoogleSearch, requireCapabilities, func(_ string, m *proxydb.Model, dropped string) {
		switch dropped {
		case "":
			out = append(out, m)
			sawCapable = true
		case DropNoVendorKey:
			sawCapable = true
		}
	})
	if err := chainErr(len(out), sawCapable); err != nil {
		return nil, err
	}
	return out, nil
}
