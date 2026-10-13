// Package grpcapi implements the proto services on top of proxydb, the
// registry, and the router.
package grpcapi

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/ai-process/llm-proxy/gen/llmproxy/v1"
	"github.com/ai-process/llm-proxy/internal/apikeys"
	"github.com/ai-process/llm-proxy/internal/crypto"
	"github.com/ai-process/llm-proxy/internal/nodes"
	"github.com/ai-process/llm-proxy/internal/proxydb"
	"github.com/ai-process/llm-proxy/internal/registry"
)

// AdminServer implements LLMProxyAdminService.
type AdminServer struct {
	pb.UnimplementedLLMProxyAdminServiceServer
	db      *proxydb.DB
	keyring *crypto.Keyring
	nodeMgr *nodes.Manager
	// onChange is called after a successful config mutation so this replica
	// picks the change up immediately instead of waiting for the poll.
	onChange func()
}

func NewAdminServer(db *proxydb.DB, keyring *crypto.Keyring, onChange func()) *AdminServer {
	if onChange == nil {
		onChange = func() {}
	}
	return &AdminServer{db: db, keyring: keyring, onChange: onChange}
}

func (s *AdminServer) SetNodeManager(mgr *nodes.Manager) {
	s.nodeMgr = mgr
}

// --- config: models and rules ---

func (s *AdminServer) UpsertModel(ctx context.Context, req *pb.UpsertModelRequest) (*pb.UpsertModelResponse, error) {
	m := modelFromProto(req.GetModel())
	if err := registry.ValidateModel(m); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	_, err := s.db.Mutate(ctx, func(tx pgx.Tx) error {
		return proxydb.UpsertModel(ctx, tx, m)
	}, validateConfig)
	if err != nil {
		return nil, mutationStatus(err)
	}
	s.onChange()
	return &pb.UpsertModelResponse{}, nil
}

func (s *AdminServer) DeleteModel(ctx context.Context, req *pb.DeleteModelRequest) (*pb.DeleteModelResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	_, err := s.db.Mutate(ctx, func(tx pgx.Tx) error {
		return proxydb.DeleteModel(ctx, tx, req.GetId())
	}, validateConfig)
	if err != nil {
		return nil, mutationStatus(err)
	}
	s.onChange()
	return &pb.DeleteModelResponse{}, nil
}

func (s *AdminServer) ReplaceRules(ctx context.Context, req *pb.ReplaceRulesRequest) (*pb.ReplaceRulesResponse, error) {
	rules := make([]*proxydb.Rule, 0, len(req.GetRules()))
	for _, r := range req.GetRules() {
		attrs := r.GetAttributes()
		if attrs == nil {
			attrs = map[string]string{}
		}
		rules = append(rules, &proxydb.Rule{
			ID:         uuid.NewString(),
			Name:       r.GetName(),
			Priority:   r.GetPriority(),
			Effort:     effortToString(r.GetEffort()),
			Attributes: attrs,
			Enabled:    r.GetEnabled(),
			Use:        r.GetUse(),
		})
	}
	_, err := s.db.Mutate(ctx, func(tx pgx.Tx) error {
		return proxydb.ReplaceRules(ctx, tx, rules)
	}, validateConfig)
	if err != nil {
		return nil, mutationStatus(err)
	}
	s.onChange()
	return &pb.ReplaceRulesResponse{}, nil
}

func (s *AdminServer) GetConfig(ctx context.Context, _ *pb.GetConfigRequest) (*pb.GetConfigResponse, error) {
	models, err := s.db.LoadModels(ctx)
	if err != nil {
		return nil, dbStatus(err)
	}
	rules, err := s.db.LoadRules(ctx)
	if err != nil {
		return nil, dbStatus(err)
	}
	vendorKeys, err := s.db.LoadVendorKeys(ctx)
	if err != nil {
		return nil, dbStatus(err)
	}
	userRateLimits, err := s.db.LoadUserRateLimits(ctx)
	if err != nil {
		return nil, dbStatus(err)
	}
	version, err := s.db.ConfigVersion(ctx)
	if err != nil {
		return nil, dbStatus(err)
	}

	resp := &pb.GetConfigResponse{Version: version}
	for _, m := range models {
		resp.Models = append(resp.Models, modelToProto(m))
	}
	for _, r := range rules {
		resp.Rules = append(resp.Rules, ruleToProto(r))
	}
	for _, vk := range vendorKeys {
		// Metadata only — the credential itself never leaves this service.
		resp.VendorKeys = append(resp.VendorKeys, &pb.VendorKeyInfo{
			ApiKeyName: vk.APIKeyName,
			Vendor:     vk.Vendor,
			KeyVersion: vk.KeyVersion,
			CreatedAt:  timestamppb.New(vk.CreatedAt),
		})
	}
	for _, u := range userRateLimits {
		resp.UserRateLimits = append(resp.UserRateLimits, userRateLimitToProto(u))
	}
	return resp, nil
}

func (s *AdminServer) SetUserRateLimit(ctx context.Context, req *pb.SetUserRateLimitRequest) (*pb.SetUserRateLimitResponse, error) {
	rl := req.GetRateLimit()
	if rl == nil || rl.GetUserId() == "" || rl.GetModelId() == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id and model_id are required")
	}
	u := userRateLimitFromProto(rl)
	_, err := s.db.Mutate(ctx, func(tx pgx.Tx) error {
		return proxydb.UpsertUserRateLimit(ctx, tx, u)
	}, nil)
	if err != nil {
		return nil, mutationStatus(err)
	}
	s.onChange()
	return &pb.SetUserRateLimitResponse{}, nil
}

func (s *AdminServer) DeleteUserRateLimit(ctx context.Context, req *pb.DeleteUserRateLimitRequest) (*pb.DeleteUserRateLimitResponse, error) {
	if req.GetUserId() == "" || req.GetModelId() == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id and model_id are required")
	}
	_, err := s.db.Mutate(ctx, func(tx pgx.Tx) error {
		return proxydb.DeleteUserRateLimit(ctx, tx, req.GetUserId(), req.GetModelId())
	}, nil)
	if err != nil {
		return nil, mutationStatus(err)
	}
	s.onChange()
	return &pb.DeleteUserRateLimitResponse{}, nil
}

func (s *AdminServer) ListUserRateLimits(ctx context.Context, req *pb.ListUserRateLimitsRequest) (*pb.ListUserRateLimitsResponse, error) {
	limits, err := s.db.LoadUserRateLimits(ctx)
	if err != nil {
		return nil, dbStatus(err)
	}
	var out []*pb.UserRateLimitSpec
	for _, l := range limits {
		if req.GetUserId() != "" && l.UserID != req.GetUserId() {
			continue
		}
		out = append(out, userRateLimitToProto(l))
	}
	return &pb.ListUserRateLimitsResponse{RateLimits: out}, nil
}

// errInvalidConfig marks registry.Validate failures so mutationStatus can
// tell a config mistake from a storage failure.
var errInvalidConfig = errors.New("invalid config")

// validateConfig is the Mutate callback: it tags validation failures.
func validateConfig(models []*proxydb.Model, rules []*proxydb.Rule) error {
	if err := registry.Validate(models, rules); err != nil {
		return fmt.Errorf("%w: %v", errInvalidConfig, err)
	}
	return nil
}

// mutationStatus maps a failed config mutation: validation failures are the
// caller's to fix, everything else is storage trouble.
func mutationStatus(err error) error {
	if errors.Is(err, proxydb.ErrNotFound) {
		return status.Error(codes.NotFound, "not found")
	}
	if errors.Is(err, errInvalidConfig) {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// FK/unique violations are config mistakes (unknown model id in a
		// chain, duplicate), not infrastructure failures.
		if pgErr.Code == "23503" || pgErr.Code == "23505" {
			return status.Error(codes.InvalidArgument, pgErr.Message)
		}
	}
	log.Error().Err(err).Msg("config mutation failed")
	return status.Error(codes.Unavailable, "storage unavailable")
}

// --- api keys ---

func (s *AdminServer) MintAPIKey(ctx context.Context, req *pb.MintAPIKeyRequest) (*pb.MintAPIKeyResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	scopes, err := apikeys.NormalizeScopes(req.GetScopes())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	plain, row, err := apikeys.Generate(req.GetName(), scopes)
	if err != nil {
		return nil, status.Error(codes.Internal, "key generation failed")
	}
	if err := s.db.InsertAPIKey(ctx, row); err != nil {
		return nil, dbStatus(err)
	}
	// The plain key exists only in this response.
	return &pb.MintAPIKeyResponse{PlainKey: plain}, nil
}

func (s *AdminServer) RevokeAPIKey(ctx context.Context, req *pb.RevokeAPIKeyRequest) (*pb.RevokeAPIKeyResponse, error) {
	if err := s.db.DisableAPIKey(ctx, req.GetName()); err != nil {
		return nil, dbStatus(err)
	}
	return &pb.RevokeAPIKeyResponse{}, nil
}

func (s *AdminServer) ListAPIKeys(ctx context.Context, _ *pb.ListAPIKeysRequest) (*pb.ListAPIKeysResponse, error) {
	keys, err := s.db.ListAPIKeys(ctx)
	if err != nil {
		return nil, dbStatus(err)
	}
	out := make([]*pb.APIKeyInfo, 0, len(keys))
	for _, k := range keys {
		out = append(out, &pb.APIKeyInfo{
			Name:      k.Name,
			Scopes:    k.Scopes,
			CreatedAt: timestamppb.New(k.CreatedAt),
			Disabled:  !k.Active(),
		})
	}
	return &pb.ListAPIKeysResponse{Keys: out}, nil
}

// --- vendor keys ---

func (s *AdminServer) SetVendorKey(ctx context.Context, req *pb.SetVendorKeyRequest) (*pb.SetVendorKeyResponse, error) {
	if !registry.ValidVendor(req.GetVendor()) {
		return nil, status.Errorf(codes.InvalidArgument, "unknown vendor %q", req.GetVendor())
	}
	if req.GetPlainKey() == "" {
		return nil, status.Error(codes.InvalidArgument, "plain_key is required")
	}
	owner, err := s.db.GetAPIKeyByName(ctx, req.GetApiKeyName())
	if err != nil {
		return nil, dbStatus(err)
	}
	ciphertext, version, err := s.keyring.Encrypt(req.GetPlainKey())
	if err != nil {
		return nil, status.Error(codes.Internal, "encryption failed")
	}
	_, err = s.db.Mutate(ctx, func(tx pgx.Tx) error {
		return proxydb.SetVendorKey(ctx, tx, &proxydb.VendorKey{
			ID:            uuid.NewString(),
			APIKeyID:      owner.ID,
			Vendor:        req.GetVendor(),
			KeyCiphertext: ciphertext,
			KeyVersion:    int32(version),
		})
	}, nil)
	if err != nil {
		return nil, dbStatus(err)
	}
	s.onChange()
	return &pb.SetVendorKeyResponse{}, nil
}

func (s *AdminServer) DeleteVendorKey(ctx context.Context, req *pb.DeleteVendorKeyRequest) (*pb.DeleteVendorKeyResponse, error) {
	owner, err := s.db.GetAPIKeyByName(ctx, req.GetApiKeyName())
	if err != nil {
		return nil, dbStatus(err)
	}
	_, err = s.db.Mutate(ctx, func(tx pgx.Tx) error {
		return proxydb.DeleteVendorKey(ctx, tx, owner.ID, req.GetVendor())
	}, nil)
	if err != nil {
		return nil, dbStatus(err)
	}
	s.onChange()
	return &pb.DeleteVendorKeyResponse{}, nil
}

func (s *AdminServer) RotateEncryption(ctx context.Context, _ *pb.RotateEncryptionRequest) (*pb.RotateEncryptionResponse, error) {
	active := int32(s.keyring.ActiveVersion())
	var count int32
	_, err := s.db.Mutate(ctx, func(tx pgx.Tx) error {
		keys, err := proxydb.LoadVendorKeysTx(ctx, tx)
		if err != nil {
			return err
		}
		for _, vk := range keys {
			if vk.KeyVersion == active {
				continue
			}
			plain, err := s.keyring.Decrypt(vk.KeyCiphertext, int(vk.KeyVersion))
			if err != nil {
				return err
			}
			ciphertext, version, err := s.keyring.Encrypt(plain)
			if err != nil {
				return err
			}
			if err := proxydb.UpdateVendorKeyCiphertext(ctx, tx, vk.ID, ciphertext, int32(version)); err != nil {
				return err
			}
			count++
		}
		return nil
	}, nil)
	if err != nil {
		return nil, dbStatus(err)
	}
	s.onChange()
	return &pb.RotateEncryptionResponse{Reencrypted: count}, nil
}

func dbStatus(err error) error {
	if errors.Is(err, proxydb.ErrNotFound) {
		return status.Error(codes.NotFound, "not found")
	}
	return status.Error(codes.Unavailable, "storage unavailable")
}
