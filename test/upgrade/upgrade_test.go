//go:build upgrade

// Package upgrade checks that a database written by the previous release keeps
// working after this build takes over. scripts/upgrade-test.sh runs TestSeed
// against the old release, TestVerify on this build, then TestServe on the old
// release again.
package upgrade

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
)

const (
	clientName = "upgrade-client"
	vendorKey  = "vk-upgrade" // the mock upstream accepts only this bearer
	modelID    = "mock-gpt"
)

type state struct {
	ClientKey string `json:"client_key"`
}

func env(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Fatalf("%s is required", k)
	}
	return v
}

func dial(t *testing.T) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(env(t, "KTMS_ADDR"), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func as(key string) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+key), cancel
}

func ok(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// generate proves the whole path: the client key authenticates, the rule
// routes, and the stored vendor key decrypts (the mock refuses any other).
func generate(t *testing.T, conn *grpc.ClientConn, clientKey string) {
	t.Helper()
	ctx, cancel := as(clientKey)
	defer cancel()
	resp, err := pb.NewGatewayServiceClient(conn).GenerateText(ctx, &pb.GenerateTextRequest{
		Effort:   pb.Effort_EFFORT_MEDIUM,
		Messages: []*pb.ChatMessage{{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "hello"}},
	})
	ok(t, "GenerateText", err)
	if resp.GetResolvedModel() != modelID || len(resp.GetChoices()) == 0 || resp.GetChoices()[0] == "" {
		t.Fatalf("GenerateText = model %q, choices %q", resp.GetResolvedModel(), resp.GetChoices())
	}
}

func TestSeed(t *testing.T) {
	conn := dial(t)
	admin := pb.NewAdminServiceClient(conn)
	ctx, cancel := as(env(t, "ADMIN_KEY"))
	defer cancel()

	minted, err := admin.MintAPIKey(ctx, &pb.MintAPIKeyRequest{Name: clientName, Scopes: []string{"generate"}})
	ok(t, "MintAPIKey", err)
	_, err = admin.UpsertModel(ctx, &pb.UpsertModelRequest{Model: &pb.ModelSpec{
		Id: modelID, Vendor: "openai", Endpoint: env(t, "MOCK_URL"), Enabled: true,
		Efforts:        []pb.Effort{pb.Effort_EFFORT_LOW, pb.Effort_EFFORT_MEDIUM, pb.Effort_EFFORT_HIGH},
		PriceInPerMtok: 1, PriceOutPerMtok: 2,
	}})
	ok(t, "UpsertModel", err)
	_, err = admin.SetVendorKey(ctx, &pb.SetVendorKeyRequest{ApiKeyName: clientName, Vendor: "openai", PlainKey: vendorKey})
	ok(t, "SetVendorKey", err)
	_, err = admin.ReplaceRules(ctx, &pb.ReplaceRulesRequest{Rules: []*pb.RuleSpec{
		{Name: "catch-all", Priority: 10, Use: []string{modelID}, Enabled: true},
	}})
	ok(t, "ReplaceRules", err)
	_, err = admin.SetUserRateLimit(ctx, &pb.SetUserRateLimitRequest{RateLimit: &pb.UserRateLimitSpec{UserId: "u-1", ModelId: "*", Rpm: 100}})
	ok(t, "SetUserRateLimit", err)
	_, err = admin.SetSpendLimit(ctx, &pb.SetSpendLimitRequest{SpendLimit: &pb.SpendLimitSpec{SubjectType: "key", SubjectId: clientName, Period: "day", LimitNanousd: 10e9}})
	ok(t, "SetSpendLimit", err)
	_, err = admin.MintNodeKey(ctx, &pb.MintNodeKeyRequest{Name: "node-1-key", NodeId: "node-1", MaxBaseWeight: 1000, MaxConcurrency: 4})
	ok(t, "MintNodeKey", err)

	// The old release must serve this before the upgrade, or the verify phase
	// would blame the new build for a setup mistake. ListModels answers before
	// the routing reload picks the model up, so wait for the model itself.
	deadline := time.Now().Add(90 * time.Second)
	for {
		ctx, cancel := as(minted.GetPlainKey())
		resp, err := pb.NewGatewayServiceClient(conn).ListModels(ctx, &pb.ListModelsRequest{})
		cancel()
		if (err == nil && len(resp.GetModels()) > 0) || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	generate(t, conn, minted.GetPlainKey())

	b, _ := json.Marshal(state{ClientKey: minted.GetPlainKey()})
	if err := os.WriteFile(env(t, "UPGRADE_STATE"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadState(t *testing.T) state {
	t.Helper()
	raw, err := os.ReadFile(env(t, "UPGRADE_STATE"))
	if err != nil {
		t.Fatalf("seed state: %v", err)
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestVerify(t *testing.T) {
	st := loadState(t)
	conn := dial(t)
	admin := pb.NewAdminServiceClient(conn)
	ctx, cancel := as(env(t, "ADMIN_KEY"))
	defer cancel()

	keys, err := admin.ListAPIKeys(ctx, &pb.ListAPIKeysRequest{})
	ok(t, "ListAPIKeys", err)
	if !slices.ContainsFunc(keys.GetKeys(), func(k *pb.APIKeyInfo) bool { return k.GetName() == clientName && !k.GetDisabled() }) {
		t.Errorf("client key %q lost or disabled: %v", clientName, keys.GetKeys())
	}
	cfg, err := admin.GetConfig(ctx, &pb.GetConfigRequest{})
	ok(t, "GetConfig", err)
	if !slices.ContainsFunc(cfg.GetModels(), func(m *pb.ModelSpec) bool { return m.GetId() == modelID && m.GetEndpoint() != "" }) {
		t.Errorf("model %q lost: %v", modelID, cfg.GetModels())
	}
	if !slices.ContainsFunc(cfg.GetRules(), func(r *pb.RuleSpec) bool { return r.GetName() == "catch-all" }) {
		t.Errorf("rule lost: %v", cfg.GetRules())
	}
	if !slices.ContainsFunc(cfg.GetVendorKeys(), func(v *pb.VendorKeyInfo) bool { return v.GetApiKeyName() == clientName }) {
		t.Errorf("vendor key lost: %v", cfg.GetVendorKeys())
	}
	limits, err := admin.ListUserRateLimits(ctx, &pb.ListUserRateLimitsRequest{})
	ok(t, "ListUserRateLimits", err)
	if len(limits.GetRateLimits()) != 1 || limits.GetRateLimits()[0].GetRpm() != 100 {
		t.Errorf("user rate limit lost: %v", limits.GetRateLimits())
	}
	spend, err := admin.ListSpendLimits(ctx, &pb.ListSpendLimitsRequest{SubjectType: "key"})
	ok(t, "ListSpendLimits", err)
	if len(spend.GetSpendLimits()) != 1 || spend.GetSpendLimits()[0].GetLimitNanousd() != 10e9 {
		t.Errorf("spend limit lost: %v", spend.GetSpendLimits())
	}
	nodeKeys, err := admin.ListNodeKeys(ctx, &pb.ListNodeKeysRequest{})
	ok(t, "ListNodeKeys", err)
	if len(nodeKeys.GetKeys()) != 1 || nodeKeys.GetKeys()[0].GetNodeId() != "node-1" {
		t.Errorf("node key lost: %v", nodeKeys.GetKeys())
	}

	generate(t, conn, st.ClientKey)

	// Migrations added since the old release must have run: write to them.
	org, err := admin.CreateOrganization(ctx, &pb.CreateOrganizationRequest{Name: "upgrade-org"})
	ok(t, "CreateOrganization", err)
	_, err = admin.CreateTeam(ctx, &pb.CreateTeamRequest{OrgId: org.GetOrganization().GetId(), Name: "upgrade-team", MonthlyBudgetUsd: 5})
	ok(t, "CreateTeam", err)
}

// TestServe runs on the old release after the new one migrated the database,
// as during a rolling upgrade: it must keep serving its own clients.
func TestServe(t *testing.T) {
	st := loadState(t)
	conn := dial(t)
	ctx, cancel := as(env(t, "ADMIN_KEY"))
	defer cancel()
	_, err := pb.NewAdminServiceClient(conn).ListAPIKeys(ctx, &pb.ListAPIKeysRequest{})
	ok(t, "ListAPIKeys", err)
	generate(t, conn, st.ClientKey)
}
