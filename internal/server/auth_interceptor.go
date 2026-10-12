package server

import (
	"context"
	"errors"
	"strings"

	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/apikeys"
)

const metadataKeyAuthorization = "authorization"

// methodScopes is the scope each RPC needs. A method missing from this map is
// refused, so adding an RPC without deciding who may call it fails closed
// instead of shipping an open endpoint.
var methodScopes = map[string]string{
	method(pb.LLMProxyService_GenerateText_FullMethodName):       apikeys.ScopeGenerate,
	method(pb.LLMProxyService_GenerateTextStream_FullMethodName): apikeys.ScopeGenerate,
	method(pb.LLMProxyService_SynthesizeSpeech_FullMethodName):   apikeys.ScopeGenerate,
	method(pb.LLMProxyService_GenerateImage_FullMethodName):      apikeys.ScopeGenerate,
	method(pb.LLMProxyService_Judge_FullMethodName):              apikeys.ScopeGenerate,
	method(pb.LLMProxyService_ListModels_FullMethodName):         apikeys.ScopeGenerate,
	method(pb.LLMProxyService_SubmitBatch_FullMethodName):        apikeys.ScopeGenerate,
	method(pb.LLMProxyService_GetBatch_FullMethodName):           apikeys.ScopeGenerate,
	method(pb.LLMProxyService_ListBatchResults_FullMethodName):   apikeys.ScopeGenerate,
	method(pb.LLMProxyService_CancelBatch_FullMethodName):        apikeys.ScopeGenerate,

	method(pb.LLMProxyAdminService_UpsertModel_FullMethodName):      apikeys.ScopeAdmin,
	method(pb.LLMProxyAdminService_DeleteModel_FullMethodName):      apikeys.ScopeAdmin,
	method(pb.LLMProxyAdminService_ReplaceRules_FullMethodName):     apikeys.ScopeAdmin,
	method(pb.LLMProxyAdminService_SetVendorKey_FullMethodName):     apikeys.ScopeAdmin,
	method(pb.LLMProxyAdminService_DeleteVendorKey_FullMethodName):  apikeys.ScopeAdmin,
	method(pb.LLMProxyAdminService_GetConfig_FullMethodName):        apikeys.ScopeAdmin,
	method(pb.LLMProxyAdminService_MintAPIKey_FullMethodName):       apikeys.ScopeAdmin,
	method(pb.LLMProxyAdminService_RevokeAPIKey_FullMethodName):     apikeys.ScopeAdmin,
	method(pb.LLMProxyAdminService_ListAPIKeys_FullMethodName):      apikeys.ScopeAdmin,
	method(pb.LLMProxyAdminService_RotateEncryption_FullMethodName): apikeys.ScopeAdmin,
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
