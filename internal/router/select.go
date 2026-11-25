package router

import (
	"cmp"
	"slices"

	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

// SelectionChainLen is how many models a selection hands the router: the
// best fit and two fallbacks, as a rule's chain usually has.
const SelectionChainLen = 3

// DropChainLength marks a capable model ranked below SelectionChainLen.
const DropChainLength = "chain_length"

// nonText are capabilities of models that do not chat: a text selection
// skips them, as a translate model listing an effort would otherwise qualify.
var nonText = []string{registry.CapabilityTTS, registry.CapabilityImage, registry.CapabilityJudge,
	registry.CapabilityTranslate, registry.CapabilityEmbedding}

// Select is the routing without a rule: every enabled model, filtered as a
// rule's chain is (needs, capabilities, vendor key), then the kept ones
// ranked by excess over the needs, then the blended price, then id. Kept
// candidates come first in that order, dropped ones after them by id. text
// leaves out models of other modalities.
func Select(snap *registry.Snapshot, apiKeyID string, needs map[string]string, needsGoogleSearch, text bool, requireCapabilities ...string) []Candidate {
	ids := make([]string, 0, len(snap.Models))
	for id, m := range snap.Models {
		if !text || !slices.ContainsFunc(nonText, func(c string) bool { return registry.HasCapability(m, c) }) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	cands := ExplainChain(snap, ids, apiKeyID, needs, needsGoogleSearch, requireCapabilities...)
	for i := range cands {
		if cands[i].Dropped == "" {
			cands[i].Excess = registry.Excess(cands[i].Model, needs)
			cands[i].Price = registry.BlendedPrice(cands[i].Model)
		}
	}
	slices.SortStableFunc(cands, func(a, b Candidate) int {
		if ka, kb := a.Dropped == "", b.Dropped == ""; ka != kb {
			if ka {
				return -1
			}
			return 1
		} else if !ka {
			return 0 // dropped ones stay in id order
		}
		return cmp.Or(cmp.Compare(a.Excess, b.Excess), cmp.Compare(a.Price, b.Price))
	})
	return cands
}

// SelectedChain cuts a ranked, policy-filtered selection to its chain; with a
// placement the class order comes first and ranking holds within each class.
func SelectedChain(models []*proxydb.Model, placement string) ([]*proxydb.Model, error) {
	out, err := ApplyPlacement(models, placement)
	if err != nil {
		return nil, err
	}
	if len(out) > SelectionChainLen {
		out = out[:SelectionChainLen]
	}
	return out, nil
}
