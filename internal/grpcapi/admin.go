// Package grpcapi implements the proto services on top of proxydb, the
// registry, and the router.
package grpcapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/crypto"
	"github.com/ai-process/ktms/internal/nodes"
	"github.com/ai-process/ktms/internal/observability"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/spendlimit"
)

// AdminServer implements AdminService.
type AdminServer struct {
	spendReader SpendReader
	spendLimits func(subjectType, subjectID, period string) int64
	pb.UnimplementedAdminServiceServer
	db       *proxydb.DB
	keyring  *crypto.Keyring
	nodeMgr  *nodes.Manager
	explain  RouteExplainer
	mcpTools MCPToolLister
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

// SpendReader reads a spend counter as the throttle keeps it.
type SpendReader func(ctx context.Context, subject, subjectID, period string) (int64, time.Time, error)

// SetSpendReader lets GetTeamSpend report what the gateway enforces; without
// it (no Redis) spend is unmetered and reported as such.
func (s *AdminServer) SetSpendReader(fn SpendReader) {
	s.spendReader = fn
}

// SetSpendLimitResolver gives GetSpend the caps the throttle enforces,
// wildcard limits included.
func (s *AdminServer) SetSpendLimitResolver(fn func(subjectType, subjectID, period string) int64) {
	s.spendLimits = fn
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
			ID:               uuid.NewString(),
			Name:             r.GetName(),
			Priority:         r.GetPriority(),
			Effort:           effortToString(r.GetEffort()),
			Attributes:       attrs,
			Enabled:          r.GetEnabled(),
			Use:              r.GetUse(),
			Weights:          r.GetWeights(),
			ShadowModel:      r.GetShadowModel(),
			ShadowPercentage: r.GetShadowPercentage(),
			ShadowEval:       r.GetShadowEval(),
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
	spendLimits, err := s.db.LoadSpendLimits(ctx)
	if err != nil {
		return nil, dbStatus(err)
	}
	mcpServers, err := s.db.LoadMCPServers(ctx)
	if err != nil {
		return nil, dbStatus(err)
	}
	mcpCreds, err := s.db.LoadMCPCredentials(ctx)
	if err != nil {
		return nil, dbStatus(err)
	}
	version, err := s.db.ConfigVersion(ctx)
	if err != nil {
		return nil, dbStatus(err)
	}

	resp := &pb.GetConfigResponse{Version: version}
	for _, m := range mcpServers {
		resp.McpServers = append(resp.McpServers, mcpServerToProto(m))
	}
	for _, c := range mcpCreds {
		resp.McpCredentials = append(resp.McpCredentials, s.mcpCredentialInfo(c))
	}
	for _, m := range models {
		resp.Models = append(resp.Models, modelToProto(m))
	}
	for _, r := range rules {
		resp.Rules = append(resp.Rules, ruleToProto(r))
	}
	for _, vk := range vendorKeys {
		// Metadata and a fingerprint only — the credential never leaves this service.
		info := &pb.VendorKeyInfo{
			ApiKeyName: vk.APIKeyName,
			Vendor:     vk.Vendor,
			KeyVersion: vk.KeyVersion,
			CreatedAt:  timestamppb.New(vk.CreatedAt),
		}
		if s.keyring != nil {
			if plain, err := s.keyring.Decrypt(vk.KeyCiphertext, int(vk.KeyVersion)); err == nil {
				info.Fingerprint = crypto.Fingerprint(plain)
			} else {
				log.Warn().Err(err).Str("api_key", vk.APIKeyName).Str("vendor", vk.Vendor).Msg("vendor key unreadable for its fingerprint")
			}
		}
		resp.VendorKeys = append(resp.VendorKeys, info)
	}
	for _, u := range userRateLimits {
		resp.UserRateLimits = append(resp.UserRateLimits, userRateLimitToProto(u))
	}
	for _, l := range spendLimits {
		resp.SpendLimits = append(resp.SpendLimits, spendLimitToProto(l))
	}
	return resp, nil
}

func (s *AdminServer) SetUserRateLimit(ctx context.Context, req *pb.SetUserRateLimitRequest) (*pb.SetUserRateLimitResponse, error) {
	rl := req.GetRateLimit()
	if rl == nil || rl.GetUserId() == "" || rl.GetModelId() == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id and model_id are required")
	}
	if rl.GetRpm() < 0 || rl.GetQueueTimeoutMs() < 0 || rl.GetDailyTokens() < 0 {
		return nil, status.Error(codes.InvalidArgument, "limits must be non-negative")
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

func validateSpendScope(subjectType, subjectID, period string) error {
	if subjectType == spendlimit.SubjectTeam {
		// One owner for a team's cap, so the team row and the limit never disagree.
		return status.Error(codes.InvalidArgument, "team budgets are set with CreateTeam/UpdateTeam")
	}
	if !spendlimit.ValidSubject(subjectType) {
		return status.Error(codes.InvalidArgument, `subject_type must be "key" or "user"`)
	}
	if subjectID == "" {
		return status.Error(codes.InvalidArgument, "subject_id is required")
	}
	if !spendlimit.ValidPeriod(period) {
		return status.Error(codes.InvalidArgument, `period must be "day" or "month"`)
	}
	return nil
}

func (s *AdminServer) SetSpendLimit(ctx context.Context, req *pb.SetSpendLimitRequest) (*pb.SetSpendLimitResponse, error) {
	sl := req.GetSpendLimit()
	if sl == nil {
		return nil, status.Error(codes.InvalidArgument, "spend_limit is required")
	}
	if err := validateSpendScope(sl.GetSubjectType(), sl.GetSubjectId(), sl.GetPeriod()); err != nil {
		return nil, err
	}
	if sl.GetLimitNanousd() <= 0 {
		return nil, status.Error(codes.InvalidArgument, "limit_nanousd must be positive; delete the limit to lift it")
	}
	l := spendLimitFromProto(sl)
	_, err := s.db.Mutate(ctx, func(tx pgx.Tx) error {
		return proxydb.UpsertSpendLimit(ctx, tx, l)
	}, nil)
	if err != nil {
		return nil, mutationStatus(err)
	}
	s.onChange()
	return &pb.SetSpendLimitResponse{}, nil
}

func (s *AdminServer) DeleteSpendLimit(ctx context.Context, req *pb.DeleteSpendLimitRequest) (*pb.DeleteSpendLimitResponse, error) {
	if err := validateSpendScope(req.GetSubjectType(), req.GetSubjectId(), req.GetPeriod()); err != nil {
		return nil, err
	}
	_, err := s.db.Mutate(ctx, func(tx pgx.Tx) error {
		return proxydb.DeleteSpendLimit(ctx, tx, req.GetSubjectType(), req.GetSubjectId(), req.GetPeriod())
	}, nil)
	if err != nil {
		return nil, mutationStatus(err)
	}
	s.onChange()
	return &pb.DeleteSpendLimitResponse{}, nil
}

func (s *AdminServer) ListSpendLimits(ctx context.Context, req *pb.ListSpendLimitsRequest) (*pb.ListSpendLimitsResponse, error) {
	limits, err := s.db.LoadSpendLimits(ctx)
	if err != nil {
		return nil, dbStatus(err)
	}
	var out []*pb.SpendLimitSpec
	for _, l := range limits {
		if req.GetSubjectType() != "" && l.SubjectType != req.GetSubjectType() {
			continue
		}
		if req.GetSubjectId() != "" && l.SubjectID != req.GetSubjectId() {
			continue
		}
		out = append(out, spendLimitToProto(l))
	}
	return &pb.ListSpendLimitsResponse{SpendLimits: out}, nil
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

func parseUUID(id, fieldName string) error {
	if _, err := uuid.Parse(id); err != nil {
		return status.Errorf(codes.InvalidArgument, "%s must be a valid UUID: %v", fieldName, err)
	}
	return nil
}

// mutationStatus maps a failed config mutation: validation failures are the
// caller's to fix, everything else is storage trouble.
func mutationStatus(err error) error {
	if errors.Is(err, proxydb.ErrNotFound) {
		return status.Error(codes.NotFound, "not found")
	}
	if errors.Is(err, errInvalidConfig) || errors.Is(err, proxydb.ErrTenantInUse) {
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
	return status.Error(codes.Unavailable, "storage_unavailable")
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
	// The admin reads are not filtered by tenant, so a tenant on a read key would mislead.
	if slices.Contains(scopes, apikeys.ScopeAdminRead) && (req.GetOrgId() != "" || req.GetTeamId() != "") {
		return nil, status.Error(codes.InvalidArgument, "an admin:read key reads every tenant and cannot belong to an organization or team")
	}
	allowed, rpm, tpm, err := keyPolicy(req.GetPolicy())
	if err != nil {
		return nil, err
	}
	allowedMCP, err := normalizeAllowedMCP(req.GetPolicy().GetAllowedMcp())
	if err != nil {
		return nil, err
	}
	dataClass, err := dataClassification(req.GetPolicy().GetDataClassification())
	if err != nil {
		return nil, err
	}
	plain, row, err := apikeys.Generate(req.GetName(), scopes)
	if err != nil {
		return nil, status.Error(codes.Internal, "key generation failed")
	}
	row.AllowedModels, row.RPMLimit, row.TPMLimit, row.AllowedMCP = allowed, rpm, tpm, allowedMCP
	row.DataClassification = dataClass

	var orgID, teamID *string
	if req.GetOrgId() != "" {
		if err := parseUUID(req.GetOrgId(), "org_id"); err != nil {
			return nil, err
		}
		v := req.GetOrgId()
		orgID = &v
	}
	if req.GetTeamId() != "" {
		if err := parseUUID(req.GetTeamId(), "team_id"); err != nil {
			return nil, err
		}
		v := req.GetTeamId()
		teamID = &v
	}
	if teamID != nil {
		team, err := s.db.GetTeam(ctx, *teamID)
		if err != nil {
			if errors.Is(err, proxydb.ErrNotFound) {
				return nil, status.Error(codes.InvalidArgument, "team not found")
			}
			return nil, dbStatus(err)
		}
		if orgID != nil && team.OrgID != *orgID {
			return nil, status.Error(codes.InvalidArgument, "team does not belong to specified organization")
		}
		if orgID == nil {
			orgID = &team.OrgID
		}
	} else if orgID != nil {
		if _, err := s.db.GetOrganization(ctx, *orgID); err != nil {
			if errors.Is(err, proxydb.ErrNotFound) {
				return nil, status.Error(codes.InvalidArgument, "organization not found")
			}
			return nil, dbStatus(err)
		}
	}
	row.OrgID = orgID
	row.TeamID = teamID

	if err := s.db.InsertAPIKey(ctx, row); err != nil {
		// The team or org was deleted after the checks above.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return nil, status.Error(codes.InvalidArgument, "team or organization not found")
		}
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
		info := &pb.APIKeyInfo{
			Name:      k.Name,
			Scopes:    k.Scopes,
			CreatedAt: timestamppb.New(k.CreatedAt),
			Disabled:  !k.Active(),
		}
		if k.OrgID != nil {
			info.OrgId = *k.OrgID
		}
		if k.TeamID != nil {
			info.TeamId = *k.TeamID
		}
		if len(k.AllowedModels) > 0 || k.RPMLimit > 0 || k.TPMLimit > 0 || len(k.AllowedMCP) > 0 || k.DataClassification != "" {
			info.Policy = &pb.APIKeyPolicy{AllowedModels: k.AllowedModels, RpmLimit: k.RPMLimit, TpmLimit: k.TPMLimit, AllowedMcp: k.AllowedMCP,
				DataClassification: k.DataClassification}
		}
		out = append(out, info)
	}
	return &pb.ListAPIKeysResponse{Keys: out}, nil
}

func (s *AdminServer) SetAPIKeyPolicy(ctx context.Context, req *pb.SetAPIKeyPolicyRequest) (*pb.SetAPIKeyPolicyResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	allowed, rpm, tpm, err := keyPolicy(req.GetPolicy())
	if err != nil {
		return nil, err
	}
	allowedMCP, err := normalizeAllowedMCP(req.GetPolicy().GetAllowedMcp())
	if err != nil {
		return nil, err
	}
	dataClass, err := updatedClassification(req.GetPolicy().GetDataClassification())
	if err != nil {
		return nil, err
	}
	if err := s.db.SetAPIKeyPolicy(ctx, req.GetName(), allowed, rpm, tpm, allowedMCP, dataClass); err != nil {
		return nil, dbStatus(err)
	}
	return &pb.SetAPIKeyPolicyResponse{}, nil
}

// keyPolicy validates a key policy; nil is no limit. Model ids are not checked
// against the registry, like a team's: a model may be added after the key.
func keyPolicy(p *pb.APIKeyPolicy) (allowed []string, rpm int32, tpm int64, err error) {
	if p.GetRpmLimit() < 0 || p.GetTpmLimit() < 0 {
		return nil, 0, 0, status.Error(codes.InvalidArgument, "rpm_limit and tpm_limit must be >= 0")
	}
	allowed = []string{}
	for _, m := range p.GetAllowedModels() {
		m = strings.TrimSpace(m)
		if m == "" {
			return nil, 0, 0, status.Error(codes.InvalidArgument, "allowed_models must not contain empty ids")
		}
		if !slices.Contains(allowed, m) {
			allowed = append(allowed, m)
		}
	}
	return allowed, p.GetRpmLimit(), p.GetTpmLimit(), nil
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
		keys, err := proxydb.LockStaleVendorKeysTx(ctx, tx, active)
		if err != nil {
			return err
		}
		for _, vk := range keys {
			plain, err := s.keyring.Decrypt(vk.KeyCiphertext, int(vk.KeyVersion))
			if err != nil {
				return &unreadableKeyError{what: "vendor key " + vk.ID, version: vk.KeyVersion, fix: "SetVendorKey"}
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
		n, err := s.rotateMCPCredentials(ctx, tx, active)
		count += n
		return err
	}, nil)
	var unreadable *unreadableKeyError
	if errors.As(err, &unreadable) {
		return nil, status.Error(codes.FailedPrecondition, unreadable.Error())
	}
	if err != nil {
		return nil, dbStatus(err)
	}
	s.onChange()
	tokens, err := s.rotateNodeTokens(ctx, int(active))
	if err != nil {
		log.Error().Err(err).Msg("node auth token rotation failed")
		return nil, status.Error(codes.Unavailable, "node registry unavailable: vendor keys were re-encrypted, call again")
	}
	return &pb.RotateEncryptionResponse{Reencrypted: count + tokens}, nil
}

// unreadableKeyError stops a rotation that would otherwise report success
// while a key still needs a version the operator is about to drop.
type unreadableKeyError struct {
	what    string // "vendor key <id>" or "mcp credential of <server>"
	version int32
	fix     string // the RPC that sets it again
}

func (e *unreadableKeyError) Error() string {
	return fmt.Sprintf("%s is encrypted with version %d, which KTMS_ENCRYPTION_KEYS lacks: add that version back, or set it again with %s", e.what, e.version, e.fix)
}

// rotateNodeTokens re-encrypts the node auth tokens kept in Redis with the
// same keyring, so dropping an old key version does not break node calls.
func (s *AdminServer) rotateNodeTokens(ctx context.Context, active int) (int32, error) {
	if s.nodeMgr == nil || s.keyring == nil {
		return 0, nil
	}
	store := s.nodeMgr.Store()
	ids, err := store.GetAllNodeIDs(ctx)
	if err != nil {
		return 0, err
	}
	prefix := strconv.Itoa(active) + ":"
	var n int32
	for _, id := range ids {
		var encErr error
		changed, err := store.UpdateMeta(ctx, id, func(meta *nodes.NodeMeta) bool {
			if meta.AuthToken == "" || strings.HasPrefix(meta.AuthToken, prefix) {
				return false
			}
			plain, err := s.keyring.DecodeVersioned(meta.AuthToken)
			if err != nil {
				// Unreadable already; the node gets a fresh token when it re-registers.
				log.Warn().Err(err).Str("node_id", id).Msg("node auth token cannot be decrypted, skipped")
				return false
			}
			ct, v, err := s.keyring.Encrypt(plain)
			if err != nil {
				encErr = err
				return false
			}
			meta.AuthToken = crypto.EncodeVersioned(ct, v)
			return true
		})
		if errors.Is(err, nodes.ErrCorruptNode) {
			log.Warn().Err(err).Str("node_id", id).Msg("node meta unreadable, token not rotated")
			continue
		}
		if err == nil {
			err = encErr
		}
		if err != nil {
			return n, err
		}
		if changed {
			n++
		}
	}
	return n, nil
}

func dbStatus(err error) error {
	if errors.Is(err, proxydb.ErrNotFound) {
		return status.Error(codes.NotFound, "not found")
	}
	log.Error().Err(err).Msg("admin storage call failed")
	// Postgres answered with an error (a constraint, bad data): an operator's
	// problem, not an outage, so it does not feed the dependency alert.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		observability.DependencyErrorsTotal.WithLabelValues("postgres", "admin").Inc()
	}
	return status.Error(codes.Unavailable, "storage_unavailable")
}

// --- organizations and teams ---

func (s *AdminServer) CreateOrganization(ctx context.Context, req *pb.CreateOrganizationRequest) (*pb.CreateOrganizationResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	org := &proxydb.Organization{
		ID:        uuid.NewString(),
		Name:      req.GetName(),
		CreatedAt: time.Now().UTC(),
	}
	if err := s.db.CreateOrganization(ctx, org); err != nil {
		return nil, mutationStatus(err)
	}
	return &pb.CreateOrganizationResponse{Organization: orgToProto(org)}, nil
}

func (s *AdminServer) ListOrganizations(ctx context.Context, _ *pb.ListOrganizationsRequest) (*pb.ListOrganizationsResponse, error) {
	orgs, err := s.db.ListOrganizations(ctx)
	if err != nil {
		return nil, dbStatus(err)
	}
	out := make([]*pb.Organization, 0, len(orgs))
	for _, o := range orgs {
		out = append(out, orgToProto(o))
	}
	return &pb.ListOrganizationsResponse{Organizations: out}, nil
}

func (s *AdminServer) DeleteOrganization(ctx context.Context, req *pb.DeleteOrganizationRequest) (*pb.DeleteOrganizationResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if err := parseUUID(req.GetId(), "id"); err != nil {
		return nil, err
	}
	_, err := s.db.Mutate(ctx, func(tx pgx.Tx) error {
		return proxydb.DeleteOrganizationTx(ctx, tx, req.GetId())
	}, nil)
	if err != nil {
		return nil, mutationStatus(err)
	}
	s.onChange()
	return &pb.DeleteOrganizationResponse{}, nil
}

func (s *AdminServer) CreateTeam(ctx context.Context, req *pb.CreateTeamRequest) (*pb.CreateTeamResponse, error) {
	if req.GetOrgId() == "" {
		return nil, status.Error(codes.InvalidArgument, "org_id is required")
	}
	if err := parseUUID(req.GetOrgId(), "org_id"); err != nil {
		return nil, err
	}
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	budget, err := teamBudgetNano(req.GetMonthlyBudgetUsd())
	if err != nil {
		return nil, err
	}
	if _, err := s.db.GetOrganization(ctx, req.GetOrgId()); err != nil {
		if errors.Is(err, proxydb.ErrNotFound) {
			return nil, status.Error(codes.InvalidArgument, "organization not found")
		}
		return nil, dbStatus(err)
	}
	allowed := req.GetAllowedModels()
	if allowed == nil {
		allowed = []string{}
	}
	allowedMCP, err := normalizeAllowedMCP(req.GetAllowedMcp())
	if err != nil {
		return nil, err
	}
	dataClass, err := dataClassification(req.GetDataClassification())
	if err != nil {
		return nil, err
	}
	team := &proxydb.Team{
		ID:               uuid.NewString(),
		OrgID:            req.GetOrgId(),
		Name:             req.GetName(),
		MonthlyBudgetUSD: req.GetMonthlyBudgetUsd(),
		AllowedModels:    allowed,
		AllowedMCP:       allowedMCP,
		CreatedAt:        time.Now().UTC(),

		DataClassification: dataClass,
	}
	_, err = s.db.Mutate(ctx, func(tx pgx.Tx) error {
		if err := proxydb.CreateTeamTx(ctx, tx, team); err != nil {
			return err
		}
		return proxydb.SetTeamBudgetTx(ctx, tx, team.ID, budget)
	}, nil)
	if err != nil {
		return nil, mutationStatus(err)
	}
	s.onChange()
	return &pb.CreateTeamResponse{Team: teamToProto(team)}, nil
}

func (s *AdminServer) GetTeam(ctx context.Context, req *pb.GetTeamRequest) (*pb.GetTeamResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if err := parseUUID(req.GetId(), "id"); err != nil {
		return nil, err
	}
	team, err := s.db.GetTeam(ctx, req.GetId())
	if err != nil {
		return nil, dbStatus(err)
	}
	return &pb.GetTeamResponse{Team: teamToProto(team)}, nil
}

func (s *AdminServer) ListTeams(ctx context.Context, req *pb.ListTeamsRequest) (*pb.ListTeamsResponse, error) {
	if req.GetOrgId() != "" {
		if err := parseUUID(req.GetOrgId(), "org_id"); err != nil {
			return nil, err
		}
	}
	teams, err := s.db.ListTeams(ctx, req.GetOrgId())
	if err != nil {
		return nil, dbStatus(err)
	}
	out := make([]*pb.Team, 0, len(teams))
	for _, t := range teams {
		out = append(out, teamToProto(t))
	}
	return &pb.ListTeamsResponse{Teams: out}, nil
}

func (s *AdminServer) UpdateTeam(ctx context.Context, req *pb.UpdateTeamRequest) (*pb.UpdateTeamResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if err := parseUUID(req.GetId(), "id"); err != nil {
		return nil, err
	}
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	budget, err := teamBudgetNano(req.GetMonthlyBudgetUsd())
	if err != nil {
		return nil, err
	}
	allowed := req.GetAllowedModels()
	if allowed == nil {
		allowed = []string{}
	}
	allowedMCP, err := normalizeAllowedMCP(req.GetAllowedMcp())
	if err != nil {
		return nil, err
	}
	dataClass, err := updatedClassification(req.GetDataClassification())
	if err != nil {
		return nil, err
	}
	t := &proxydb.Team{
		ID:               req.GetId(),
		Name:             req.GetName(),
		MonthlyBudgetUSD: req.GetMonthlyBudgetUsd(),
		AllowedModels:    allowed,
		AllowedMCP:       allowedMCP,
	}
	_, err = s.db.Mutate(ctx, func(tx pgx.Tx) error {
		if err := proxydb.UpdateTeamTx(ctx, tx, t, dataClass); err != nil {
			return err
		}
		return proxydb.SetTeamBudgetTx(ctx, tx, t.ID, budget)
	}, nil)
	if err != nil {
		return nil, mutationStatus(err)
	}
	s.onChange()
	updated, err := s.db.GetTeam(ctx, req.GetId())
	if err != nil {
		return nil, dbStatus(err)
	}
	return &pb.UpdateTeamResponse{Team: teamToProto(updated)}, nil
}

func (s *AdminServer) DeleteTeam(ctx context.Context, req *pb.DeleteTeamRequest) (*pb.DeleteTeamResponse, error) {
	if req.GetId() == "" {
		return nil, status.Error(codes.InvalidArgument, "id is required")
	}
	if err := parseUUID(req.GetId(), "id"); err != nil {
		return nil, err
	}
	_, err := s.db.Mutate(ctx, func(tx pgx.Tx) error {
		return proxydb.DeleteTeamTx(ctx, tx, req.GetId())
	}, nil)
	if err != nil {
		return nil, mutationStatus(err)
	}
	s.onChange()
	return &pb.DeleteTeamResponse{}, nil
}

// maxTeamBudgetUSD keeps the nano-USD conversion inside int64 with room to spare.
const maxTeamBudgetUSD = 1e9

func teamBudgetNano(usd float64) (int64, error) {
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 || usd > maxTeamBudgetUSD {
		return 0, status.Errorf(codes.InvalidArgument, "monthly_budget_usd must be between 0 and %.0f", maxTeamBudgetUSD)
	}
	return int64(math.Round(usd * 1e9)), nil
}

func (s *AdminServer) GetTeamSpend(ctx context.Context, req *pb.GetTeamSpendRequest) (*pb.GetTeamSpendResponse, error) {
	if err := parseUUID(req.GetId(), "id"); err != nil {
		return nil, err
	}
	team, err := s.db.GetTeam(ctx, req.GetId())
	if err != nil {
		return nil, dbStatus(err)
	}
	limit, _ := teamBudgetNano(team.MonthlyBudgetUSD)
	if s.spendLimits != nil {
		// What the throttle enforces, a team/* wildcard included.
		limit = s.spendLimits(spendlimit.SubjectTeam, team.ID, spendlimit.PeriodMonth)
	}
	_, resets := spendlimit.Window(spendlimit.PeriodMonth, time.Now())
	out := &pb.GetTeamSpendResponse{LimitNanousd: limit, ResetsAt: timestamppb.New(resets)}
	// The throttle keeps counters only for capped subjects, so an uncapped
	// team has no number to report.
	if s.spendReader == nil || limit == 0 {
		return out, nil
	}
	spent, resets, err := s.spendReader(ctx, spendlimit.SubjectTeam, team.ID, spendlimit.PeriodMonth)
	if err != nil {
		log.Warn().Err(err).Str("team_id", team.ID).Msg("team spend counter unreadable")
		return nil, status.Error(codes.Unavailable, "spend counters unavailable")
	}
	out.SpentNanousd, out.ResetsAt, out.Metered = spent, timestamppb.New(resets), true
	return out, nil
}

func (s *AdminServer) GetSpend(ctx context.Context, req *pb.GetSpendRequest) (*pb.GetSpendResponse, error) {
	subject, id := req.GetSubjectType(), req.GetSubjectId()
	if !spendlimit.ValidSubject(subject) || id == "" || id == spendlimit.Wildcard {
		return nil, status.Error(codes.InvalidArgument, "subject_type must be key, user or team, with a subject_id")
	}
	out := &pb.GetSpendResponse{}
	now := time.Now()
	for _, e := range spendlimit.Enforced {
		if e.Subject != subject {
			continue
		}
		_, resets := spendlimit.Window(e.Period, now)
		w := &pb.SpendWindow{Period: e.Period, ResetsAt: timestamppb.New(resets)}
		if s.spendLimits != nil {
			w.LimitNanousd = s.spendLimits(subject, id, e.Period)
		}
		// Counters exist only under a cap, as in GetTeamSpend.
		if s.spendReader != nil && w.LimitNanousd > 0 {
			spent, _, err := s.spendReader(ctx, subject, id, e.Period)
			if err != nil {
				log.Warn().Err(err).Str("subject", subject).Str("id", id).Msg("spend counter unreadable")
				return nil, status.Error(codes.Unavailable, "spend counters unavailable")
			}
			w.SpentNanousd, w.Metered = spent, true
			w.RemainingNanousd = max(w.LimitNanousd-spent, 0)
		}
		out.Windows = append(out.Windows, w)
	}
	return out, nil
}

func orgToProto(o *proxydb.Organization) *pb.Organization {
	if o == nil {
		return nil
	}
	return &pb.Organization{
		Id:        o.ID,
		Name:      o.Name,
		CreatedAt: timestamppb.New(o.CreatedAt),
	}
}

func teamToProto(t *proxydb.Team) *pb.Team {
	if t == nil {
		return nil
	}
	return &pb.Team{
		Id:               t.ID,
		OrgId:            t.OrgID,
		Name:             t.Name,
		MonthlyBudgetUsd: t.MonthlyBudgetUSD,
		AllowedModels:    t.AllowedModels,
		CreatedAt:        timestamppb.New(t.CreatedAt),
		AllowedMcp:       t.AllowedMCP,

		DataClassification: t.DataClassification,
	}
}

// dataClassification checks the value a create RPC stores.
func dataClassification(v string) (string, error) {
	v, err := apikeys.ParseDataClassification(v, false)
	if err != nil {
		return "", status.Error(codes.InvalidArgument, err.Error())
	}
	return v, nil
}

// updatedClassification is what an update RPC stores: nil for "", which keeps
// the stored value so a client that predates the field cannot lift a
// restriction by leaving it out; "unrestricted" lifts it.
func updatedClassification(v string) (*string, error) {
	v, err := apikeys.ParseDataClassification(v, true)
	switch {
	case err != nil:
		return nil, status.Error(codes.InvalidArgument, err.Error())
	case v == "":
		return nil, nil
	case v == apikeys.DataUnrestricted:
		v = ""
	}
	return &v, nil
}
