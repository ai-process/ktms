package grpcapi

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/router"
)

// routingAttributes is what rules match on: the caller's attributes with the
// reserved keys stripped, plus the org and team the key belongs to, which only
// the proxy may state (a caller could otherwise claim another team's rules).
func routingAttributes(attrs map[string]string, id *apikeys.Identity) map[string]string {
	out := router.SanitizeAttributes(attrs)
	if out == nil {
		out = map[string]string{}
	}
	if id != nil && id.OrgID != "" {
		out["org_id"] = id.OrgID
	}
	if id != nil && id.TeamID != "" {
		out["team_id"] = id.TeamID
	}
	return out
}

// usageMeta seeds a call's usage-event meta: the attributes as routing sees
// them and the api key, so no path can attribute spend to a team it is not in.
func usageMeta(attrs map[string]string, id *apikeys.Identity) map[string]string {
	meta := routingAttributes(attrs, id)
	meta[APIKeyAttribute] = id.Name
	return meta
}

type internalCallKey struct{}

// withInternalCall marks a call the proxy makes for itself (auto-effort
// judging, shadow evaluation): the caller's team allowlist governs what the
// caller may reach, not the judge the operator configured.
func withInternalCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, internalCallKey{}, true)
}

func isInternalCall(ctx context.Context) bool {
	v, _ := ctx.Value(internalCallKey{}).(bool)
	return v
}

// teamModels keeps the chain models the caller's key and team may use.
func teamModels(ctx context.Context, id *apikeys.Identity, models []*proxydb.Model) ([]*proxydb.Model, error) {
	if !id.RestrictsModels() || isInternalCall(ctx) {
		return models, nil
	}
	var allowed []*proxydb.Model
	scope := "key"
	for _, m := range models {
		if id.CanAccessModel(m.ID) {
			allowed = append(allowed, m)
		} else if id.DeniedBy(m.ID) == "team" {
			scope = "team"
		}
	}
	if len(allowed) == 0 {
		return nil, status.Error(codes.PermissionDenied, "no models in chain allowed for "+scope)
	}
	return allowed, nil
}
