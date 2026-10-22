package grpcapi

import (
	"context"
	"errors"
	"strconv"

	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/cache"
	"github.com/ai-process/ktms/internal/guardrails"
	"github.com/ai-process/ktms/internal/observability"
	"github.com/ai-process/ktms/internal/redisguard"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/router"
	usagev1 "github.com/ai-process/ktms/internal/usagereport/gen/usage/v1"
)

// SemanticCache serves responses for prompts that are close in latent space.
type SemanticCache interface {
	Get(ctx context.Context, apiKeyID string, configVersion int64, req *pb.GenerateTextRequest) (*pb.GenerateTextResponse, float32, bool, error)
	Set(ctx context.Context, apiKeyID string, configVersion int64, req *pb.GenerateTextRequest, resp *pb.GenerateTextResponse) error
}

// WithSemanticCache configures latent-space caching, consulted after an exact miss.
func (s *ProxyServer) WithSemanticCache(c SemanticCache) *ProxyServer {
	s.semantic = c
	return s
}

type cacheHit struct {
	resp       *pb.GenerateTextResponse
	semantic   bool
	similarity float32
}

// lookupCache tries the exact cache, then the semantic one.
func (s *ProxyServer) lookupCache(ctx context.Context, snap *registry.Snapshot, id *apikeys.Identity, req *pb.GenerateTextRequest) *cacheHit {
	if !req.GetEnableCache() {
		return nil
	}
	if s.cache != nil {
		cached, hit, err := s.cache.Get(ctx, id.KeyID, snap.Version, req)
		if err != nil {
			observability.DependencyErrorsTotal.WithLabelValues("redis", "cache_get").Inc()
			redisguard.Log(err).Err(err).Str("api_key_id", id.KeyID).Msg("cache lookup failed")
		} else if hit && cached != nil {
			return &cacheHit{resp: cached}
		}
	}
	if s.semantic != nil && s.semanticEligible(req) && s.semanticScreened(ctx, req) {
		cached, score, hit, err := s.semantic.Get(ctx, id.KeyID, snap.Version, req)
		if err != nil {
			observability.DependencyErrorsTotal.WithLabelValues("redis", "semantic_cache_get").Inc()
			redisguard.Log(err).Err(err).Str("api_key_id", id.KeyID).Msg("semantic cache lookup failed")
		} else if hit && cached != nil {
			return &cacheHit{resp: cached, semantic: true, similarity: score}
		}
	}
	return nil
}

// semanticEligible keeps prompts carrying PII out of the semantic cache: a
// near match would hand one person's answer to a differently-personal prompt.
func (s *ProxyServer) semanticEligible(req *pb.GenerateTextRequest) bool {
	text, ok := cache.SemanticText(req)
	return ok && len(guardrails.DetectPII(text)) == 0
}

// semanticScreened runs guardrails on the utterance before a semantic hit is
// served: unlike an exact hit, a near match never passed them itself. A block
// falls through to the normal path, which rejects with the proper error.
func (s *ProxyServer) semanticScreened(ctx context.Context, req *pb.GenerateTextRequest) bool {
	if s.guardrails == nil || !s.guardrails.Enabled {
		return true
	}
	text, _ := cache.SemanticText(req)
	if _, _, err := s.guardrails.InspectText(ctx, text, nil); err != nil {
		log.Debug().Err(err).Msg("semantic cache skipped: utterance failed guardrails")
		return false
	}
	return true
}

// admitCacheHit charges rpm and records usage for a cache hit.
func (s *ProxyServer) admitCacheHit(ctx context.Context, snap *registry.Snapshot, id *apikeys.Identity, req *pb.GenerateTextRequest, hit *cacheHit) error {
	cached := hit.resp
	userID := router.UserID(req.GetAttributes(), id.Name)
	if m, ok := snap.Models[cached.ResolvedModel]; ok && m != nil {
		if !s.throttle.AllowRPM(ctx, id.Name, userID, m) {
			return status.Error(codes.ResourceExhausted, "resource_exhausted: rpm limit exceeded")
		}
	}
	tracker := snap.Tracker()
	if tracker == nil {
		return nil
	}
	meta := usageMeta(req.GetAttributes(), id)
	if cached.MatchedRule != "" {
		meta["rule"] = cached.MatchedRule
	}
	meta["cache"] = "hit"
	if hit.semantic {
		meta["cache_match"] = "semantic"
		meta["cache_similarity"] = strconv.FormatFloat(float64(hit.similarity), 'f', 4, 32)
	}
	if err := tracker.RecordUsage(userID, defaultActionID, cached.ResolvedVendor, cached.ResolvedModel, 0, 0, 0, 0, usagev1.Status_STATUS_SUCCESS, meta); err != nil {
		log.Warn().Err(err).Str("user_id", userID).Str("model", cached.ResolvedModel).Bool("semantic", hit.semantic).Msg("failed to record cache hit usage")
	}
	return nil
}

// storeCache writes a fresh response to every configured cache.
func (s *ProxyServer) storeCache(ctx context.Context, snap *registry.Snapshot, id *apikeys.Identity, req *pb.GenerateTextRequest, out *pb.GenerateTextResponse) {
	if !req.GetEnableCache() {
		return
	}
	if s.cache != nil {
		if err := s.cache.Set(ctx, id.KeyID, snap.Version, req, out); err != nil {
			observability.DependencyErrorsTotal.WithLabelValues("redis", "cache_set").Inc()
			redisguard.Log(err).Err(err).Str("api_key_id", id.KeyID).Msg("failed to cache response")
		}
	}
	if s.semantic != nil && s.semanticEligible(req) {
		if err := s.semantic.Set(ctx, id.KeyID, snap.Version, req, out); err != nil {
			observability.DependencyErrorsTotal.WithLabelValues("redis", "semantic_cache_set").Inc()
			redisguard.Log(err).Err(err).Str("api_key_id", id.KeyID).Msg("failed to store semantic cache entry")
		}
	}
}

// guardrailStatus maps a guardrail error to a status; a cancelled or timed-out
// caller is not a bad request.
func guardrailStatus(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return status.FromContextError(err).Err()
	}
	return status.Errorf(codes.InvalidArgument, "%s", err.Error())
}
