package grpcapi

import (
	"context"

	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/router"
)

// selectChain routes a request no rule takes: the models that meet its needs
// and capabilities, narrowed by the key's and team's allowed_models before
// ranking, so a policy never empties a chain a wider pool would have filled.
// Placement then orders by class, and the chain keeps the first few.
func (s *ProxyServer) selectChain(ctx context.Context, snap *registry.Snapshot, id *apikeys.Identity, needs map[string]string, search, text bool, caps []string, placement string, pair *languagePair, tr *routeTrace) ([]*proxydb.Model, error) {
	cands := router.Select(snap, id.KeyID, needs, search, text, caps...)
	if tr != nil {
		tr.selected = true
		tr.add(cands...)
	}
	models, err := router.Kept(cands)
	if err != nil {
		return nil, router.ToStatus(err)
	}
	if pair != nil {
		if models, err = tr.languagePair(models, pair); err != nil {
			return nil, err
		}
	}
	tr.policy(ctx, id, models)
	if models, err = teamModels(ctx, id, models); err != nil {
		return nil, err
	}
	before := models
	chain, err := router.ApplyPlacement(models, placement)
	tr.placed(before, chain)
	if err != nil {
		return nil, router.ToStatus(err)
	}
	ranked := chain
	if chain, err = router.SelectedChain(chain, ""); err != nil {
		return nil, router.ToStatus(err)
	}
	tr.cut(ranked, chain, router.DropChainLength)
	if tr != nil {
		tr.chain = make([]string, len(chain))
		for i, m := range chain {
			tr.chain[i] = m.ID
		}
	}
	return chain, nil
}
