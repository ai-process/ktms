package grpcapi

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/router"
)

// DataClassificationAttribute marks a restricted call in its usage event.
const DataClassificationAttribute = "data_classification"

// withRestricted marks the model calls a restricted request makes for itself
// (the auto-effort judge, shadow evaluation): they go through planMedia too.
func withRestricted(ctx context.Context) context.Context { return apikeys.WithRestricted(ctx) }

// restrictedCall reports whether a call must stay on nodes: its key or team
// is restricted, or the request is (its header, or the request a judge call serves).
func restrictedCall(ctx context.Context, id *apikeys.Identity) bool {
	return apikeys.RestrictedFrom(ctx) || id.Restricted()
}

// classify makes the request restricted when it, the key, its team or the
// header is, so routing reads one field. A restricted request skips the cache:
// a stored answer could reach another key with a similar prompt.
func classify(ctx context.Context, id *apikeys.Identity, req *pb.GenerateTextRequest) error {
	v, err := apikeys.ParseDataClassification(req.GetDataClassification(), false)
	if err != nil {
		return status.Error(codes.InvalidArgument, "bad_request: "+err.Error())
	}
	if v == "" && restrictedCall(ctx, id) {
		v = apikeys.DataRestricted
	}
	req.DataClassification = v
	if v == apikeys.DataRestricted {
		req.EnableCache = false
	}
	return nil
}

func isRestricted(req *pb.GenerateTextRequest) bool {
	return req.GetDataClassification() == apikeys.DataRestricted
}

// restrictedPlacement is the placement of a restricted request: nodes only,
// whatever it preferred; cloud_only, which no node can serve, is an error.
func restrictedPlacement(placement string) (string, error) {
	if placement == router.PlacementCloudOnly {
		return "", status.Errorf(codes.InvalidArgument,
			"bad_request: placement %s needs a cloud vendor, and data_classification restricted forbids one", placement)
	}
	return router.PlacementNodesOnly, nil
}

// nodeModels keeps the self-hosted models of a restricted call's chain; with
// none it fails rather than reach a cloud vendor.
func nodeModels(models []*proxydb.Model, what string) ([]*proxydb.Model, error) {
	var out []*proxydb.Model
	for _, m := range models {
		if router.IsNodeModel(m) {
			out = append(out, m)
		}
	}
	if len(out) == 0 {
		return nil, noNodeModel(what)
	}
	return out, nil
}

func noNodeModel(what string) error {
	return status.Errorf(codes.FailedPrecondition,
		"no_capable_model: data_classification restricted: no self-hosted node model serves %s", what)
}
