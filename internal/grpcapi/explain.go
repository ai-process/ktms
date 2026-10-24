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
)

// routeTrace is what planRoute records for ExplainRoute. Its methods do
// nothing on a nil trace, so a real call pays nothing for them.
type routeTrace struct {
	caps         []string // capabilities the explained request needs
	effort       string
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

func (t *routeTrace) matched(effort string) {
	if t != nil {
		t.effort = effort
	}
}

func (t *routeTrace) add(c ...router.Candidate) {
	if t != nil {
		t.candidates = append(t.candidates, c...)
	}
}

// filter is router.FilterChain, recording each model's fate when tracing.
func (t *routeTrace) filter(snap *registry.Snapshot, use []string, keyID, effort string, search bool, caps []string) ([]*proxydb.Model, error) {
	if t == nil {
		return router.FilterChain(snap, use, keyID, effort, search, caps...)
	}
	cands := router.ExplainChain(snap, use, keyID, effort, search, caps...)
	t.add(cands...)
	return router.Kept(cands)
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
func (t *routeTrace) placed(before, after []*proxydb.Model) {
	if t == nil {
		return
	}
	for _, m := range before {
		if !slices.Contains(after, m) {
			t.drop(m.ID, dropPlacement)
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
	tr := &routeTrace{caps: in.GetCapabilities()}
	req := &pb.GenerateTextRequest{
		Effort:             in.GetEffort(),
		EffortSpecified:    in.GetEffort() != pb.Effort_EFFORT_UNSPECIFIED,
		AutoRoute:          in.GetAutoRoute(),
		Attributes:         router.SanitizeAttributes(in.GetAttributes()),
		Placement:          in.GetPlacement(),
		ModelOverride:      in.GetModelOverride(),
		EnableGoogleSearch: slices.Contains(in.GetCapabilities(), registry.CapabilityGoogleSearch),
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

	out.Effort, out.RouteMode, out.Chain, out.JudgeSkipped = tr.effort, tr.routeMode, tr.chain, tr.judgeSkipped
	if tr.rule != nil {
		out.Rule, out.Weights = tr.rule.Name, tr.rule.Weights
	}
	for _, c := range tr.candidates {
		rc := &pb.RouteCandidate{ModelId: c.ID, Dropped: c.Dropped}
		if c.Model != nil {
			rc.Vendor = c.Model.Vendor
			if c.Dropped == dropVendorPaused && cooldown != nil {
				rc.PausedFor = durationpb.New(cooldown.Remaining(c.Model))
			}
		}
		out.Candidates = append(out.Candidates, rc)
	}
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
	s.markNodes(ctx, out)
	return out, nil
}

// markNodes drops local models no node may take now, by the node selector's
// own filter. If the nodes cannot be listed, the trace still comes back.
func (s *AdminServer) markNodes(ctx context.Context, out *pb.ExplainRouteResponse) {
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
			if serves, err = nodes.Serves(ctx, s.nodeMgr, c.GetModelId()); err != nil {
				out.Error = statusText(status.Errorf(codes.Unavailable, "list nodes: %v", err))
				return
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
