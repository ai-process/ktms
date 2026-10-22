package server

import (
	"context"
	"errors"
	"strings"

	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
)

const metadataKeyAuthorization = "authorization"

// methodScopes is the scope each RPC needs. A method missing from this map is
// refused, so adding an RPC without deciding who may call it fails closed
// instead of shipping an open endpoint.
var methodScopes = map[string]string{
	method(pb.GatewayService_GenerateText_FullMethodName):       apikeys.ScopeGenerate,
	method(pb.GatewayService_GenerateTextStream_FullMethodName): apikeys.ScopeGenerate,
	method(pb.GatewayService_SynthesizeSpeech_FullMethodName):   apikeys.ScopeGenerate,
	method(pb.GatewayService_GenerateImage_FullMethodName):      apikeys.ScopeGenerate,
	method(pb.GatewayService_Judge_FullMethodName):              apikeys.ScopeGenerate,
	method(pb.GatewayService_GetEmbeddings_FullMethodName):      apikeys.ScopeGenerate,
	method(pb.GatewayService_JudgeStream_FullMethodName):        apikeys.ScopeGenerate,
	method(pb.GatewayService_EvaluateRollouts_FullMethodName):   apikeys.ScopeGenerate,
	method(pb.GatewayService_ListModels_FullMethodName):         apikeys.ScopeGenerate,
	method(pb.GatewayService_SubmitBatch_FullMethodName):        apikeys.ScopeGenerate,
	method(pb.GatewayService_GetBatch_FullMethodName):           apikeys.ScopeGenerate,
	method(pb.GatewayService_ListBatchResults_FullMethodName):   apikeys.ScopeGenerate,
	method(pb.GatewayService_CancelBatch_FullMethodName):        apikeys.ScopeGenerate,

	method(pb.AdminService_UpsertModel_FullMethodName):         apikeys.ScopeAdmin,
	method(pb.AdminService_DeleteModel_FullMethodName):         apikeys.ScopeAdmin,
	method(pb.AdminService_ReplaceRules_FullMethodName):        apikeys.ScopeAdmin,
	method(pb.AdminService_SetVendorKey_FullMethodName):        apikeys.ScopeAdmin,
	method(pb.AdminService_DeleteVendorKey_FullMethodName):     apikeys.ScopeAdmin,
	method(pb.AdminService_GetConfig_FullMethodName):           apikeys.ScopeAdmin,
	method(pb.AdminService_MintAPIKey_FullMethodName):          apikeys.ScopeAdmin,
	method(pb.AdminService_RevokeAPIKey_FullMethodName):        apikeys.ScopeAdmin,
	method(pb.AdminService_ListAPIKeys_FullMethodName):         apikeys.ScopeAdmin,
	method(pb.AdminService_RotateEncryption_FullMethodName):    apikeys.ScopeAdmin,
	method(pb.AdminService_MintNodeKey_FullMethodName):         apikeys.ScopeAdmin,
	method(pb.AdminService_RevokeNodeKey_FullMethodName):       apikeys.ScopeAdmin,
	method(pb.AdminService_ListNodeKeys_FullMethodName):        apikeys.ScopeAdmin,
	method(pb.AdminService_ListNodes_FullMethodName):           apikeys.ScopeAdmin,
	method(pb.AdminService_DrainNode_FullMethodName):           apikeys.ScopeAdmin,
	method(pb.AdminService_SetUserRateLimit_FullMethodName):    apikeys.ScopeAdmin,
	method(pb.AdminService_DeleteUserRateLimit_FullMethodName): apikeys.ScopeAdmin,
	method(pb.AdminService_ListUserRateLimits_FullMethodName):  apikeys.ScopeAdmin,
	method(pb.AdminService_SetSpendLimit_FullMethodName):       apikeys.ScopeAdmin,
	method(pb.AdminService_DeleteSpendLimit_FullMethodName):    apikeys.ScopeAdmin,
	method(pb.AdminService_ListSpendLimits_FullMethodName):     apikeys.ScopeAdmin,

	method(pb.AdminService_CreateOrganization_FullMethodName): apikeys.ScopeAdmin,
	method(pb.AdminService_ListOrganizations_FullMethodName):  apikeys.ScopeAdmin,
	method(pb.AdminService_DeleteOrganization_FullMethodName): apikeys.ScopeAdmin,
	method(pb.AdminService_CreateTeam_FullMethodName):         apikeys.ScopeAdmin,
	method(pb.AdminService_GetTeam_FullMethodName):            apikeys.ScopeAdmin,
	method(pb.AdminService_ListTeams_FullMethodName):          apikeys.ScopeAdmin,
	method(pb.AdminService_UpdateTeam_FullMethodName):         apikeys.ScopeAdmin,
	method(pb.AdminService_DeleteTeam_FullMethodName):         apikeys.ScopeAdmin,
	method(pb.AdminService_GetTeamSpend_FullMethodName):       apikeys.ScopeAdmin,

	method(pb.NodeRegistryService_RegisterNode_FullMethodName):   apikeys.ScopeNode,
	method(pb.NodeRegistryService_ValidateNode_FullMethodName):   apikeys.ScopeNode,
	method(pb.NodeRegistryService_Heartbeat_FullMethodName):      apikeys.ScopeNode,
	method(pb.NodeRegistryService_UnregisterNode_FullMethodName): apikeys.ScopeNode,
}

// method normalises the generated constants, which carry no leading slash on
// some plugin versions, to the FullMethod form the interceptor sees.
func method(name string) string {
	if strings.HasPrefix(name, "/") {
		return name
	}
	return "/" + name
}

// authUnaryInterceptor authenticates the bearer key from the "authorization"
// metadata, checks the method's scope, and injects the caller identity for
// handlers. Auth is never dormant: this service holds LLM vendor keys.
func authUnaryInterceptor(verifier *apikeys.Verifier) grpclib.UnaryServerInterceptor {
	return func(
		ctx context.Context, req any, info *grpclib.UnaryServerInfo, handler grpclib.UnaryHandler,
	) (any, error) {
		if isAuthExempt(info.FullMethod) {
			return handler(ctx, req)
		}
		id, err := authorize(ctx, verifier, info.FullMethod)
		if err != nil {
			return nil, err
		}
		return handler(apikeys.WithIdentity(ctx, id), req)
	}
}

type wrappedServerStream struct {
	grpclib.ServerStream
	ctx context.Context
}

func (w *wrappedServerStream) Context() context.Context {
	return w.ctx
}

func authStreamInterceptor(verifier *apikeys.Verifier) grpclib.StreamServerInterceptor {
	return func(
		srv any, ss grpclib.ServerStream, info *grpclib.StreamServerInfo, handler grpclib.StreamHandler,
	) error {
		if isAuthExempt(info.FullMethod) {
			return handler(srv, ss)
		}
		id, err := authorize(ss.Context(), verifier, info.FullMethod)
		if err != nil {
			return err
		}
		return handler(srv, &wrappedServerStream{
			ServerStream: ss,
			ctx:          apikeys.WithIdentity(ss.Context(), id),
		})
	}
}

func isAuthExempt(fullMethod string) bool {
	return strings.HasPrefix(fullMethod, "/grpc.health.") ||
		strings.HasPrefix(fullMethod, "/grpc.reflection.")
}

func authorize(ctx context.Context, verifier *apikeys.Verifier, fullMethod string) (*apikeys.Identity, error) {
	scope, known := methodScopes[fullMethod]
	if !known {
		return nil, status.Errorf(codes.PermissionDenied, "no scope is configured for %s", fullMethod)
	}
	key, err := bearerFrom(ctx)
	if err != nil {
		return nil, err
	}

	id, err := verifier.VerifyScope(ctx, key, scope)
	switch {
	case errors.Is(err, apikeys.ErrUnauthenticated):
		return nil, status.Error(codes.Unauthenticated, "invalid api key")
	case errors.Is(err, apikeys.ErrForbidden):
		return nil, status.Error(codes.PermissionDenied, "insufficient scope: "+scope)
	case err != nil:
		// A store failure must never fall through to allow.
		return nil, status.Error(codes.Unavailable, "auth backend unavailable")
	}
	return id, nil
}

func bearerFrom(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "missing metadata")
	}
	vals := md.Get(metadataKeyAuthorization)
	if len(vals) == 0 {
		return "", status.Error(codes.Unauthenticated, "missing authorization")
	}
	key, err := apikeys.ParseBearer(vals[0])
	if err != nil {
		return "", status.Error(codes.Unauthenticated, "invalid authorization format")
	}
	return key, nil
}
