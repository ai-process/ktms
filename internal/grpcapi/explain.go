package grpcapi

import (
	"context"
	"errors"
	"slices"
	"strings"
	"unicode"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/nodes"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/router"
)

// Drops that only ExplainRoute reports: planRoute's tenancy and placement
// filters, and what Execute would skip at call time.
const (
	dropKeyPolicy        = "key_policy"
	dropTeamPolicy       = "team_policy"
	dropPlacement        = "placement"
	dropCircuitOpen      = "circuit_open"
	dropVendorPaused     = "vendor_paused"
	dropNodeUnavailable  = "node_unavailable"
	dropNodePoolDisabled = "node_pool_disabled"
	dropLanguagePair     = "language_pair"
)

// routeTrace is what planRoute records for ExplainRoute. Its methods do
// nothing on a nil trace, so a real call pays nothing for them.
type routeTrace struct {
	caps         []string // capabilities the explained request needs
	needs        map[string]string
	selected     bool // no rule took it: candidates are the selection
	routeMode    string
	chain        []string
	rule         *proxydb.Rule
	candidates   []router.Candidate
	judgeSkipped bool
}

func (t *routeTrace) start(mode string, chain []string, rule *proxydb.Rule) {
	if t != nil {
		t.routeMode, t.chain, t.rule = mode, chain, rule
	}
}

func (t *routeTrace) matched(needs map[string]string) {
	if t != nil {
		t.needs = needs
	}
}

func (t *routeTrace) add(c ...router.Candidate) {
	if t != nil {
		t.candidates = append(t.candidates, c...)
	}
}

// filter is router.FilterChain, recording each model's fate when tracing.
func (t *routeTrace) filter(snap *registry.Snapshot, use []string, keyID string, needs map[string]string, search bool, caps []string) ([]*proxydb.Model, error) {
	if t == nil {
		return router.FilterChain(snap, use, keyID, needs, search, caps...)
	}
	cands := router.ExplainChain(snap, use, keyID, needs, search, caps...)
	t.add(cands...)
	return router.Kept(cands)
}

// languagePair keeps the models declaring the pair. Capable models without it
// are not an error each, but none left is: the caller asked for a pair the
// chain cannot serve.
func (t *routeTrace) languagePair(models []*proxydb.Model, p *languagePair) ([]*proxydb.Model, error) {
	var out []*proxydb.Model
	for _, m := range models {
		if registry.ServesLanguagePair(m, p.source, p.target) {
			out = append(out, m)
		} else if t != nil {
			t.drop(m.ID, dropLanguagePair)
		}
	}
	if len(out) == 0 {
		return nil, status.Errorf(codes.FailedPrecondition,
			"unsupported_language_pair: no model in the chain translates %s to %s", orAuto(p.source), p.target)
	}
	return out, nil
}

func orAuto(lang string) string {
	if lang == "" {
		return "a detected language"
	}
	return lang
}

// capabilities are the request's own, plus those the explained request names.
func (t *routeTrace) capabilities(req *pb.GenerateTextRequest) []string {
	caps := requiredCapabilities(req)
	if t != nil {
		caps = append(caps, t.caps...)
	}
	return caps
}

func (t *routeTrace) drop(id, reason string) {
	for i := range t.candidates {
		if t.candidates[i].ID == id && t.candidates[i].Dropped == "" {
			t.candidates[i].Dropped = reason
			return
		}
	}
}

// policy marks what teamModels is about to remove.
func (t *routeTrace) policy(ctx context.Context, id *apikeys.Identity, models []*proxydb.Model) {
	if t == nil || !id.RestrictsModels() || isInternalCall(ctx) {
		return
	}
	for _, m := range models {
		if !id.CanAccessModel(m.ID) {
			t.drop(m.ID, id.DeniedBy(m.ID)+"_policy")
		}
	}
}

// placed marks what ApplyPlacement removed.
func (t *routeTrace) placed(before, after []*proxydb.Model) { t.cut(before, after, dropPlacement) }

// cut marks the models of before that after lacks with reason.
func (t *routeTrace) cut(before, after []*proxydb.Model, reason string) {
	if t == nil {
		return
	}
	for _, m := range before {
		if !slices.Contains(after, m) {
			t.drop(m.ID, reason)
		}
	}
}

// ExplainRoute runs planRoute with a trace, then marks what Execute would
// skip now. A routing error comes back with the trace, not instead of it.
func (s *ProxyServer) ExplainRoute(ctx context.Context, id *apikeys.Identity, in *pb.ExplainRouteRequest) *pb.ExplainRouteResponse {
	out := &pb.ExplainRouteResponse{}
	if !id.HasScope(apikeys.ScopeGenerate) {
		out.Error = statusText(status.Error(codes.PermissionDenied, "apikeys: insufficient scope: need "+apikeys.ScopeGenerate))
		return out
	}
	snap := s.snapshots.Snapshot()
	if !snap.Configured() {
		out.Error = statusText(router.ToStatus(router.ErrNotConfigured))
		return out
	}
	if in.GetModality() != "" {
		return s.explainMedia(ctx, id, in)
	}
	tr := &routeTrace{caps: in.GetCapabilities()}
	req := &pb.GenerateTextRequest{
		Effort:             in.GetEffort(),
		EffortSpecified:    in.GetEffort() != pb.Effort_EFFORT_UNSPECIFIED,
		Needs:              in.GetNeeds(),
		AutoRoute:          in.GetAutoRoute(),
		Attributes:         router.SanitizeAttributes(in.GetAttributes()),
		Placement:          in.GetPlacement(),
		ModelOverride:      in.GetModelOverride(),
		EnableGoogleSearch: slices.Contains(in.GetCapabilities(), registry.CapabilityGoogleSearch),
		DataClassification: in.GetDataClassification(),
	}
	if err := classify(ctx, id, req); err != nil {
		out.Error = statusText(err)
		return out
	}
	plan, err := s.planRoute(ctx, snap, id, req, tr)

	// Execute's order: a vendor pause first, then the breaker, asked without
	// taking the probe slot a real call would.
	cooldown := s.cooldownFor(snap, id)
	for i := range tr.candidates {
		c := &tr.candidates[i]
		if c.Dropped != "" || c.Model == nil || err != nil {
			continue
		}
		if cooldown != nil && cooldown.Remaining(c.Model) > 0 {
			c.Dropped = dropVendorPaused
		} else if !s.breakerWouldAllow(c.Model) {
			c.Dropped = dropCircuitOpen
		}
	}

	out.JudgeSkipped = tr.judgeSkipped
	tr.write(out, cooldown)
	if err != nil {
		out.Error = statusText(err)
		return out
	}
	for _, m := range plan.models {
		if keptNow(out.Candidates, m.ID) {
			out.Models = append(out.Models, m.ID)
		}
	}
	// Execute mirrors only to a shadow whose breaker is closed.
	if m := plan.shadowModel; m != nil && (s.circuitBreaker == nil || s.circuitBreaker.IsClosed(m.Vendor, m.ID)) {
		out.ShadowModel, out.ShadowPercentage = m.ID, plan.shadowPercentage
	}
	if len(out.Models) == 0 {
		out.Error = statusText(status.Error(codes.Unavailable, "vendors_unavailable"))
	}
	return out
}

// write puts the trace into an ExplainRoute response.
func (t *routeTrace) write(out *pb.ExplainRouteResponse, cooldown router.Cooldown) {
	out.RouteMode, out.Chain, out.Selected = t.routeMode, t.chain, t.selected
	if len(t.needs) > 0 {
		out.Needs = t.needs
		out.Effort = registry.LevelEffort(t.needs[registry.TopicGeneral])
	}
	if t.rule != nil {
		out.Rule, out.Weights = t.rule.Name, t.rule.Weights
	}
	for _, c := range t.candidates {
		rc := &pb.RouteCandidate{ModelId: c.ID, Dropped: c.Dropped, Excess: int32(c.Excess), BlendedPricePerMtok: c.Price}
		if c.Model != nil {
			rc.Vendor = c.Model.Vendor
			if c.Dropped == dropVendorPaused && cooldown != nil {
				rc.PausedFor = durationpb.New(cooldown.Remaining(c.Model))
			}
		}
		out.Candidates = append(out.Candidates, rc)
	}
}

// mediaCapabilities are what each non-text modality's chain is filtered by.
var mediaCapabilities = map[string]string{
	modalityTranslate: registry.CapabilityTranslate,
	modalityJudge:     registry.CapabilityJudge,
	modalityTTS:       registry.CapabilityTTS,
	modalityImage:     registry.CapabilityImage,
}

// explainMedia is ExplainRoute for a non-text modality: planMediaFor with a
// trace, then what ExecuteMedia would skip now.
func (s *ProxyServer) explainMedia(ctx context.Context, id *apikeys.Identity, in *pb.ExplainRouteRequest) *pb.ExplainRouteResponse {
	out := &pb.ExplainRouteResponse{}
	capability, ok := mediaCapabilities[in.GetModality()]
	if !ok {
		out.Error = statusText(status.Errorf(codes.InvalidArgument, "bad_request: modality %q: want translate, judge, tts or image", in.GetModality()))
		return out
	}
	q := mediaQuery{
		attrs:    router.SanitizeAttributes(in.GetAttributes()),
		override: in.GetModelOverride(),
		modality: in.GetModality(),
		caps:     append([]string{capability}, in.GetCapabilities()...),
		needs:    in.GetNeeds(),
	}
	if in.GetModality() == modalityTranslate {
		source, target, err := languageTags(in.GetSourceLanguage(), in.GetTargetLanguage())
		if err != nil {
			out.Error = statusText(err)
			return out
		}
		q.pair = &languagePair{source: source, target: target}
	}
	if v, err := apikeys.ParseDataClassification(in.GetDataClassification(), false); err != nil {
		out.Error = statusText(status.Error(codes.InvalidArgument, "bad_request: "+err.Error()))
		return out
	} else if v == apikeys.DataRestricted {
		ctx = apikeys.WithRestricted(ctx)
	}
	tr := &routeTrace{}
	plan, err := s.planMediaFor(ctx, id, q, tr)

	snap := s.snapshots.Snapshot()
	cooldown := s.cooldownFor(snap, id)
	for i := range tr.candidates {
		c := &tr.candidates[i]
		if c.Dropped != "" || c.Model == nil || err != nil {
			continue
		}
		if cooldown != nil && cooldown.Remaining(c.Model) > 0 {
			c.Dropped = dropVendorPaused
		} else if !s.breakerWouldAllow(c.Model) {
			c.Dropped = dropCircuitOpen
		}
	}
	tr.write(out, cooldown)
	if err != nil {
		out.Error = statusText(err)
		return out
	}
	for _, m := range plan.chain {
		if keptNow(out.Candidates, m.ID) {
			out.Models = append(out.Models, m.ID)
		}
	}
	if len(out.Models) == 0 {
		out.Error = statusText(status.Error(codes.Unavailable, "vendors_unavailable"))
	}
	return out
}

// breakerWouldAllow asks without effects when the breaker can; one that
// cannot is asked whether it is closed, which may hide a due probe.
func (s *ProxyServer) breakerWouldAllow(m *proxydb.Model) bool {
	switch cb := s.circuitBreaker.(type) {
	case nil:
		return true
	case interface {
		WouldAllow(vendor, model string) bool
	}:
		return cb.WouldAllow(m.Vendor, m.ID)
	default:
		return cb.IsClosed(m.Vendor, m.ID)
	}
}

func keptNow(cands []*pb.RouteCandidate, id string) bool {
	for _, c := range cands {
		if c.GetModelId() == id && c.GetDropped() == "" {
			return true
		}
	}
	return false
}

// statusText writes the code as the error contract does, FAILED_PRECONDITION.
func statusText(err error) string {
	st, ok := status.FromError(err)
	if !ok {
		return err.Error()
	}
	var b strings.Builder
	for i, r := range st.Code().String() {
		if i > 0 && unicode.IsUpper(r) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String() + ": " + st.Message()
}

// RouteExplainer is ProxyServer.ExplainRoute, given to the AdminServer.
type RouteExplainer func(context.Context, *apikeys.Identity, *pb.ExplainRouteRequest) *pb.ExplainRouteResponse

// SetRouteExplainer wires ExplainRoute to the gateway's routing.
func (s *AdminServer) SetRouteExplainer(e RouteExplainer) {
	s.explain = e
}

// ExplainRoute routes as the named client key would, without calling anything.
func (s *AdminServer) ExplainRoute(ctx context.Context, req *pb.ExplainRouteRequest) (*pb.ExplainRouteResponse, error) {
	if s.explain == nil {
		return nil, status.Error(codes.Unimplemented, "routing is not available on this server")
	}
	if req.GetApiKeyName() == "" {
		return nil, status.Error(codes.InvalidArgument, "api_key_name is required")
	}
	row, err := s.db.GetAPIKeyByName(ctx, req.GetApiKeyName())
	if errors.Is(err, proxydb.ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "no client key %q", req.GetApiKeyName())
	}
	if err != nil {
		return nil, dbStatus(err)
	}
	if !row.Active() {
		return nil, status.Errorf(codes.FailedPrecondition, "client key %q is revoked", req.GetApiKeyName())
	}
	out := s.explain(ctx, apikeys.IdentityFor(row), req)
	s.markNodes(ctx, out, mediaCapabilities[req.GetModality()])
	return out, nil
}

// markNodes drops local models no node may take now, by the node selector's
// own filter. If the nodes cannot be listed, the trace still comes back.
func (s *AdminServer) markNodes(ctx context.Context, out *pb.ExplainRouteResponse, capability string) {
	asked := map[string]bool{}
	for _, c := range out.GetCandidates() {
		if c.GetDropped() != "" || c.GetVendor() != registry.VendorLocal {
			continue
		}
		if s.nodeMgr == nil {
			c.Dropped = dropNodePoolDisabled
			continue
		}
		serves, ok := asked[c.GetModelId()]
		if !ok {
			var err error
			if serves, err = nodes.Serves(ctx, s.nodeMgr, c.GetModelId(), capability); err != nil {
				out.Error = statusText(status.Errorf(codes.Unavailable, "list nodes: %v", err))
				return
			}
			// A chat model on a node translates by prompt, through chat.
			if !serves && capability == registry.CapabilityTranslate {
				if serves, err = nodes.Serves(ctx, s.nodeMgr, c.GetModelId(), ""); err != nil {
					out.Error = statusText(status.Errorf(codes.Unavailable, "list nodes: %v", err))
					return
				}
			}
			asked[c.GetModelId()] = serves
		}
		if !serves {
			c.Dropped = dropNodeUnavailable
		}
	}
	var models []string
	for _, id := range out.GetModels() {
		if keptNow(out.GetCandidates(), id) {
			models = append(models, id)
		}
	}
	out.Models = models
	if len(models) == 0 && out.GetError() == "" {
		out.Error = statusText(status.Error(codes.Unavailable, "vendors_unavailable"))
	}
}
