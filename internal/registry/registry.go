package registry

import (
	"fmt"
	"sort"
	"sync"

	"github.com/ai-process/llm-proxy/internal/llm"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/spendlimit"
)

// Snapshot is one immutable view of the routing config: enabled models and
// rules plus decrypted vendor keys. Replaced wholesale on config change;
// decrypted keys live only here, in process memory.
type Snapshot struct {
	Version int64
	Models  map[string]*proxydb.Model // enabled only
	Rules   []*proxydb.Rule           // enabled only, sorted by priority desc
	// vendorKeys[apiKeyID][vendor] = decrypted credential.
	vendorKeys map[string]map[string]string
	// UserRateLimits[userID][modelID] = user rate limit overrides.
	UserRateLimits map[string]map[string]*proxydb.UserRateLimit
	// SpendLimits[subjectType+"\x00"+subjectID][period] = cap in nano-USD.
	SpendLimits map[string]map[string]int64
	// adapters caches built vendor adapters per (apiKeyID, modelID) for this
	// snapshot's lifetime; dropped wholesale on config change.
	adapters sync.Map
	tracker  llm.UsageTracker
	nodePool NodePoolResolver
}

func (s *Snapshot) SetNodePool(np NodePoolResolver) {
	s.nodePool = np
}

// Tracker returns the usage tracker configured on the snapshot.
func (s *Snapshot) Tracker() llm.UsageTracker {
	if s == nil {
		return nil
	}
	return s.tracker
}

// UserRateLimit returns the rate limit override for a user and model, or for the user wildcard "*".
func (s *Snapshot) UserRateLimit(userID, modelID string) *proxydb.UserRateLimit {
	if s == nil || s.UserRateLimits == nil || userID == "" {
		return nil
	}
	if userMap, ok := s.UserRateLimits[userID]; ok {
		if limit, ok := userMap[modelID]; ok {
			return limit
		}
		if limit, ok := userMap["*"]; ok {
			return limit
		}
	}
	if starMap, ok := s.UserRateLimits["*"]; ok {
		if limit, ok := starMap[modelID]; ok {
			return limit
		}
		return starMap["*"]
	}
	return nil
}

// SpendLimit returns the cap in nano-USD for a subject and period, falling
// back to the subject type's "*" default; 0 means no cap.
func (s *Snapshot) SpendLimit(subjectType, subjectID, period string) int64 {
	if s == nil || s.SpendLimits == nil || subjectID == "" {
		return 0
	}
	if limit, ok := s.SpendLimits[spendKey(subjectType, subjectID)][period]; ok {
		return limit
	}
	return s.SpendLimits[spendKey(subjectType, spendlimit.Wildcard)][period]
}

func spendKey(subjectType, subjectID string) string { return subjectType + "\x00" + subjectID }

// VendorKey returns the calling client's decrypted credential for a vendor.
func (s *Snapshot) VendorKey(apiKeyID, vendor string) (string, bool) {
	key, ok := s.vendorKeys[apiKeyID][vendor]
	return key, ok
}

// CanCall reports whether the client may use m. Local models reach nodes with
// the node's own token, so they need no vendor key.
func (s *Snapshot) CanCall(apiKeyID string, m *proxydb.Model) bool {
	if m.Vendor == VendorLocal {
		return true
	}
	_, ok := s.vendorKeys[apiKeyID][m.Vendor]
	return ok
}

// ClientModels lists enabled models the client can call,
// sorted by id for stable ListModels output.
func (s *Snapshot) ClientModels(apiKeyID string) []*proxydb.Model {
	var out []*proxydb.Model
	for _, m := range s.Models {
		if s.CanCall(apiKeyID, m) {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Configured reports whether any routing config exists at all.
func (s *Snapshot) Configured() bool {
	return len(s.Models) > 0 && len(s.Rules) > 0
}

// ValidateModel checks a single spec before it reaches the database.
func ValidateModel(m *proxydb.Model) error {
	if m.ID == "" {
		return fmt.Errorf("model id is required")
	}
	if !ValidVendor(m.Vendor) {
		return fmt.Errorf("model %s: unknown vendor %q", m.ID, m.Vendor)
	}
	if m.Vendor == VendorOllama && m.Endpoint == "" {
		return fmt.Errorf("model %s: an ollama model needs its endpoint", m.ID)
	}
	if len(m.Efforts) == 0 {
		return fmt.Errorf("model %s: at least one effort is required", m.ID)
	}
	for _, e := range m.Efforts {
		if !ValidEffort(e) {
			return fmt.Errorf("model %s: unknown effort %q", m.ID, e)
		}
	}
	if m.RPM < 0 || m.PriceInPerMtok < 0 || m.PriceOutPerMtok < 0 ||
		m.PriceInPeakPerMtok < 0 || m.PriceOutPeakPerMtok < 0 ||
		m.DailyTokensPerKey < 0 || m.DailyTokensPerUser < 0 ||
		m.QueueTimeoutMs < 0 {
		return fmt.Errorf("model %s: negative limits, prices, or queue timeout", m.ID)
	}
	if m.BudgetGraceMargin < -1.0 || m.BudgetGraceMargin > 1.0 {
		return fmt.Errorf("model %s: budget grace margin must be between -1.0 and 1.0", m.ID)
	}
	return nil
}

// Validate checks the full config as it would take effect: enabled rules with
// chains filtered to enabled models. Runs inside every admin mutation
// transaction and on load, so an invalid state can never become active.
func Validate(models []*proxydb.Model, rules []*proxydb.Rule) error {
	byID := map[string]*proxydb.Model{}
	for _, m := range models {
		if err := ValidateModel(m); err != nil {
			return err
		}
		if _, dup := byID[m.ID]; dup {
			return fmt.Errorf("duplicate model id %s", m.ID)
		}
		byID[m.ID] = m
	}

	names := map[string]bool{}
	priorities := map[int32]bool{}
	// catchAll[effort] flips when an enabled attribute-free rule serves it.
	catchAll := map[string]bool{}

	for _, r := range rules {
		if r.Name == "" {
			return fmt.Errorf("rule name is required")
		}
		if names[r.Name] {
			return fmt.Errorf("duplicate rule name %s", r.Name)
		}
		names[r.Name] = true
		if priorities[r.Priority] {
			return fmt.Errorf("rule %s: duplicate priority %d", r.Name, r.Priority)
		}
		priorities[r.Priority] = true
		if r.Effort != "" && !ValidEffort(r.Effort) {
			return fmt.Errorf("rule %s: unknown effort %q", r.Name, r.Effort)
		}
		if len(r.Use) == 0 {
			return fmt.Errorf("rule %s: empty model chain", r.Name)
		}
		if len(r.Weights) > 0 {
			if len(r.Weights) != len(r.Use) {
				return fmt.Errorf("rule %s: weights count (%d) must match use count (%d)", r.Name, len(r.Weights), len(r.Use))
			}
			for _, w := range r.Weights {
				if w <= 0 {
					return fmt.Errorf("rule %s: weight %d must be > 0", r.Name, w)
				}
			}
		}

		seen := map[string]bool{}
		var enabledChain []*proxydb.Model
		for _, id := range r.Use {
			m, ok := byID[id]
			if !ok {
				return fmt.Errorf("rule %s: unknown model %s", r.Name, id)
			}
			if seen[id] {
				return fmt.Errorf("rule %s: duplicate model %s in chain", r.Name, id)
			}
			seen[id] = true
			if m.Enabled {
				enabledChain = append(enabledChain, m)
			}
		}
		if !r.Enabled {
			continue
		}
		if len(enabledChain) == 0 {
			return fmt.Errorf("rule %s: every model in the chain is disabled", r.Name)
		}
		// Every effort the rule can match needs at least one serving model,
		// or a matching request would find an empty chain at runtime.
		for _, e := range matchableEfforts(r) {
			if !anyServes(enabledChain, e) {
				return fmt.Errorf("rule %s: no enabled model in the chain serves effort %s", r.Name, e)
			}
			if len(r.Attributes) == 0 {
				catchAll[e] = true
			}
		}
	}

	// No rules at all is the models-first bootstrap state: GenerateText fails
	// with not_configured until rules arrive. Once any rule exists, every
	// effort must have a catch-all.
	if len(rules) > 0 {
		for _, e := range []string{EffortLow, EffortMedium, EffortHigh} {
			if !catchAll[e] {
				return fmt.Errorf("no catch-all rule (empty attributes) covers effort %s", e)
			}
		}
	}
	return nil
}

func matchableEfforts(r *proxydb.Rule) []string {
	if r.Effort == "" {
		return []string{EffortLow, EffortMedium, EffortHigh}
	}
	return []string{r.Effort}
}

func anyServes(models []*proxydb.Model, effort string) bool {
	for _, m := range models {
		for _, e := range m.Efforts {
			if e == effort {
				return true
			}
		}
	}
	return false
}

// Serves reports whether a model lists the effort.
func Serves(m *proxydb.Model, effort string) bool {
	for _, e := range m.Efforts {
		if e == effort {
			return true
		}
	}
	return false
}

// HasCapability reports whether a model lists the capability.
func HasCapability(m *proxydb.Model, capability string) bool {
	for _, c := range m.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}
