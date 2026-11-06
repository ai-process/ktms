// Package apikeys mints and verifies the caller credentials stored in the api_key
// table. A key is stored as a sha256 hash, never in plain — the plain value exists
// only in the response that mints it.
//
// Deliberately not bcrypt: these keys are 32 bytes from crypto/rand, so there is
// nothing to guess and bcrypt's cost would buy only latency.
package apikeys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/ktms/internal/observability"
	"github.com/ai-process/ktms/internal/proxydb"
)

// Scopes. A key carries a subset; admin implies all of them.
const (
	// ScopeGenerate allows the data-plane RPCs (GenerateText, ListModels).
	ScopeGenerate = "generate"
	// ScopeMCP allows the MCP gateway at /mcp.
	ScopeMCP   = "mcp"
	ScopeAdmin = "admin"
	// ScopeAdminRead allows the Admin API reads and nothing that changes state.
	ScopeAdminRead = "admin:read"
	ScopeNode      = "node"
)

// KeyPrefix marks our keys so a leaked string is recognisable in a log scan.
const (
	KeyPrefix           = "ktms_"
	NodeKeyPrefix       = "ktms_node_"
	LegacyKeyPrefix     = "llm_"
	LegacyNodeKeyPrefix = "llm_node_"
)

var (
	ErrUnauthenticated = errors.New("apikeys: unauthenticated")
	ErrForbidden       = errors.New("apikeys: insufficient scope")
)

// ValidScopes is the closed set the admin endpoint accepts.
var ValidScopes = []string{ScopeGenerate, ScopeMCP, ScopeAdmin, ScopeAdminRead, ScopeNode}

// Store is the slice of proxydb the verifier needs, so tests can fake it.
type Store interface {
	GetAPIKeyByLookup(ctx context.Context, lookup string) (*proxydb.APIKey, error)
	GetNodeKeyByLookup(ctx context.Context, lookup string) (*proxydb.NodeKey, error)
}

// Identity is the authenticated caller.
type Identity struct {
	KeyID             string
	Name              string
	Scopes            []string
	NodeID            string
	AllowedCIDRs      []string
	AllowedModels     []string
	MaxBaseWeight     float64
	MaxConcurrent     int32
	OrgID             string
	TeamID            string
	TeamAllowedModels []string
	// The client key's own policy; empty and 0 mean no limit.
	KeyAllowedModels []string
	RPMLimit         int32
	TPMLimit         int64
	// MCP servers ("server") and tools ("server/tool") allowed; empty = any.
	KeyAllowedMCP  []string
	TeamAllowedMCP []string
	// DataRestricted on the key or its team keeps all its calls on nodes.
	KeyDataClassification  string
	TeamDataClassification string
}

// Data classifications. DataUnrestricted exists only to lift a restriction
// in the update RPCs, where an empty value keeps the current one.
const (
	DataRestricted   = "restricted"
	DataUnrestricted = "unrestricted"
)

// ParseDataClassification normalises a data_classification: "" or
// "restricted", and with update also "unrestricted".
func ParseDataClassification(v string, update bool) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" || v == DataRestricted || update && v == DataUnrestricted {
		return v, nil
	}
	want := `"restricted" or empty`
	if update {
		want = `"restricted", "unrestricted" or empty`
	}
	return "", fmt.Errorf("data_classification %q: want %s", v, want)
}

// Restricted reports whether the key or its team is restricted to nodes.
func (i *Identity) Restricted() bool {
	return i != nil && (i.KeyDataClassification == DataRestricted || i.TeamDataClassification == DataRestricted)
}

// HasScope reports whether the caller may perform an action needing want.
func (i *Identity) HasScope(want string) bool {
	for _, s := range i.Scopes {
		if s == want || s == ScopeAdmin {
			return true
		}
	}
	return false
}

// CanAccessModel reports whether both the key's and its team's allowlists
// (empty = any model) permit modelID.
func (i *Identity) CanAccessModel(modelID string) bool {
	if i == nil {
		return false
	}
	return (len(i.TeamAllowedModels) == 0 || slices.Contains(i.TeamAllowedModels, modelID)) &&
		(len(i.KeyAllowedModels) == 0 || slices.Contains(i.KeyAllowedModels, modelID))
}

// DeniedBy names the allowlist that refuses modelID: "team" when the team's
// does, else "key". Refusal messages name it so the operator knows which to edit.
func (i *Identity) DeniedBy(modelID string) string {
	if i != nil && len(i.TeamAllowedModels) > 0 && !slices.Contains(i.TeamAllowedModels, modelID) {
		return "team"
	}
	return "key"
}

// RestrictsModels reports whether an allowlist narrows the models it may call.
func (i *Identity) RestrictsModels() bool {
	return i != nil && (len(i.TeamAllowedModels) > 0 || len(i.KeyAllowedModels) > 0)
}

// CanUseTool reports whether both the key's and its team's allowed_mcp permit
// the server's tool; tool "" asks whether any tool of the server may be.
func (i *Identity) CanUseTool(server, tool string) bool {
	return i != nil && mcpAllows(i.TeamAllowedMCP, server, tool) && mcpAllows(i.KeyAllowedMCP, server, tool)
}

// MCPDeniedBy names the allowlist that refuses the tool, as DeniedBy does.
func (i *Identity) MCPDeniedBy(server, tool string) string {
	if i != nil && !mcpAllows(i.TeamAllowedMCP, server, tool) {
		return "team"
	}
	return "key"
}

func mcpAllows(list []string, server, tool string) bool {
	if len(list) == 0 {
		return true
	}
	for _, e := range list {
		s, t, one := strings.Cut(e, "/")
		if s == server && (!one || tool == "" || t == tool) {
			return true
		}
	}
	return false
}

// LookupHash is the indexed sha256 hex of a plain key.
func LookupHash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// VerifyHash is what key_hash holds. Same digest as LookupHash on purpose:
// the lookup column finds the row and the hash column authenticates it; a
// future change to how rows are found must not quietly change authentication.
func VerifyHash(plain string) string {
	return LookupHash(plain)
}

// Generate mints a key, returning the plain value (shown to the operator once)
// alongside the row to store.
func Generate(name string, scopes []string) (plain string, row *proxydb.APIKey, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("apikeys: entropy: %w", err)
	}
	plain = KeyPrefix + hex.EncodeToString(raw)

	return plain, &proxydb.APIKey{
		ID:        uuid.NewString(),
		Name:      name,
		KeyLookup: LookupHash(plain),
		KeyHash:   VerifyHash(plain),
		Scopes:    scopes,
	}, nil
}

// GenerateNodeKey mints a node key (prefixed with llm_node_), returning the
// plain secret alongside the database row to store.
func GenerateNodeKey(name, nodeID string, allowedCIDRs, allowedModels []string, maxBaseWeight float64, maxConcurrency int32) (plain string, row *proxydb.NodeKey, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, fmt.Errorf("apikeys: entropy: %w", err)
	}
	plain = NodeKeyPrefix + hex.EncodeToString(raw)

	return plain, &proxydb.NodeKey{
		NodeID:         nodeID,
		Name:           name,
		KeyLookup:      LookupHash(plain),
		KeyHash:        VerifyHash(plain),
		AllowedCIDRs:   allowedCIDRs,
		AllowedModels:  allowedModels,
		MaxBaseWeight:  maxBaseWeight,
		MaxConcurrency: maxConcurrency,
	}, nil
}

// NormalizeScopes rejects unknown scopes and drops duplicates.
func NormalizeScopes(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, errors.New("apikeys: at least one scope is required")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if !isValidScope(s) {
			return nil, fmt.Errorf("apikeys: unknown scope %q", s)
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, nil
}

func isValidScope(s string) bool {
	for _, v := range ValidScopes {
		if v == s {
			return true
		}
	}
	return false
}

// Verifier authenticates bearer keys for the gRPC interceptor.
type Verifier struct {
	store        Store
	bootstrapKey string
	queryTimeout time.Duration // bounds each key lookup; 0 = the caller's deadline only
	stale        *staleCache   // nil = a store outage refuses every key
}

// WithQueryTimeout bounds each key lookup, so a hung database fails calls
// instead of stalling them until every pool connection is stuck.
func (v *Verifier) WithQueryTimeout(d time.Duration) *Verifier {
	v.queryTimeout = d
	return v
}

// WithStaleIfError lets a key verified within maxAge keep working while the
// store is unreachable; revocation applies whenever the store answers, so only
// one made just before an outage can lag, by at most maxAge. 0 disables it.
func (v *Verifier) WithStaleIfError(maxAge time.Duration) *Verifier {
	if maxAge > 0 {
		v.stale = newStaleCache(maxAge)
	} else {
		v.stale = nil
	}
	return v
}

// NewVerifier builds a verifier. bootstrapKey, when non-empty, is accepted as
// an implicit admin key — it exists only to mint the first real key, and can be
// unset afterwards.
func NewVerifier(store Store, bootstrapKey string) *Verifier {
	return &Verifier{store: store, bootstrapKey: bootstrapKey}
}

// ParseBearer extracts the key from an "Authorization: Bearer <key>" value.
func ParseBearer(header string) (string, error) {
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return "", ErrUnauthenticated
	}
	key := strings.TrimSpace(parts[1])
	if key == "" {
		return "", ErrUnauthenticated
	}
	return key, nil
}

// Verify authenticates a plain key. Auth is never dormant here: no key means
// no access, because this service holds LLM vendor keys.
func (v *Verifier) Verify(ctx context.Context, plain string) (*Identity, error) {
	if plain == "" {
		return nil, ErrUnauthenticated
	}

	// Constant-time so a wrong bootstrap key cannot be found by timing.
	if v.bootstrapKey != "" &&
		subtle.ConstantTimeCompare([]byte(plain), []byte(v.bootstrapKey)) == 1 {
		return &Identity{KeyID: "bootstrap", Name: "bootstrap", Scopes: []string{ScopeAdmin}}, nil
	}

	lookup := LookupHash(plain) // also the verify hash: one digest, computed once
	id, err := v.lookup(ctx, plain, lookup)
	switch {
	case err == nil:
		v.stale.put(lookup, id)
		return id, nil
	case errors.Is(err, ErrUnauthenticated):
		v.stale.drop(lookup) // a revoked or unknown key never survives on the cache
		return nil, err
	case ctx.Err() != nil:
		return nil, err // the caller left; that says nothing about the store
	}
	observability.DependencyErrorsTotal.WithLabelValues("postgres", "auth").Inc()
	if cached := v.stale.get(lookup); cached != nil {
		log.Warn().Err(err).Str("key", cached.Name).Msg("key store unreachable, accepting a recently verified key")
		return cached, nil
	}
	log.Error().Err(err).Msg("key store unreachable, refusing the call")
	return nil, err
}

// lookup verifies the key against the store: ErrUnauthenticated for a key
// that is unknown, disabled or wrong, any other error for an unreachable store.
func (v *Verifier) lookup(ctx context.Context, plain, lookup string) (*Identity, error) {
	if v.queryTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, v.queryTimeout)
		defer cancel()
	}
	if strings.HasPrefix(plain, NodeKeyPrefix) || strings.HasPrefix(plain, LegacyNodeKeyPrefix) {
		row, err := v.store.GetNodeKeyByLookup(ctx, lookup)
		if errors.Is(err, proxydb.ErrNotFound) {
			return nil, ErrUnauthenticated
		}
		if err != nil {
			return nil, err
		}
		if !row.Active() {
			return nil, ErrUnauthenticated
		}
		if subtle.ConstantTimeCompare([]byte(row.KeyHash), []byte(lookup)) != 1 {
			return nil, ErrUnauthenticated
		}
		return &Identity{
			KeyID:         fmt.Sprintf("%d", row.ID),
			Name:          row.Name,
			Scopes:        []string{ScopeNode},
			NodeID:        row.NodeID,
			AllowedCIDRs:  row.AllowedCIDRs,
			AllowedModels: row.AllowedModels,
			MaxBaseWeight: row.MaxBaseWeight,
			MaxConcurrent: row.MaxConcurrency,
		}, nil
	}

	row, err := v.store.GetAPIKeyByLookup(ctx, lookup)
	if errors.Is(err, proxydb.ErrNotFound) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	if !row.Active() {
		return nil, ErrUnauthenticated
	}
	if subtle.ConstantTimeCompare([]byte(row.KeyHash), []byte(lookup)) != 1 {
		return nil, ErrUnauthenticated
	}

	return IdentityFor(row), nil
}

// IdentityFor is the caller a client key's row stands for, as Verify builds
// it; ExplainRoute uses it to route as that key would.
func IdentityFor(row *proxydb.APIKey) *Identity {
	var orgID, teamID string
	if row.OrgID != nil {
		orgID = *row.OrgID
	}
	if row.TeamID != nil {
		teamID = *row.TeamID
	}
	return &Identity{
		KeyID:             row.ID,
		Name:              row.Name,
		Scopes:            row.Scopes,
		OrgID:             orgID,
		TeamID:            teamID,
		TeamAllowedModels: row.TeamAllowedModels,
		KeyAllowedModels:  row.AllowedModels,
		RPMLimit:          row.RPMLimit,
		TPMLimit:          row.TPMLimit,
		KeyAllowedMCP:     row.AllowedMCP,
		TeamAllowedMCP:    row.TeamAllowedMCP,

		KeyDataClassification:  row.DataClassification,
		TeamDataClassification: row.TeamDataClassification,
	}
}

// VerifyScope authenticates and authorises in one step.
func (v *Verifier) VerifyScope(ctx context.Context, plain, scope string) (*Identity, error) {
	id, err := v.Verify(ctx, plain)
	if err != nil {
		return nil, err
	}
	if !id.HasScope(scope) {
		return nil, fmt.Errorf("%w: need %s", ErrForbidden, scope)
	}
	return id, nil
}
