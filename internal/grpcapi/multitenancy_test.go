package grpcapi

import (
	"context"
	"fmt"
	"math"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

func TestTeamAllowedModels_ModelOverrideRefusal(t *testing.T) {
	models := []*proxydb.Model{
		{ID: "allowed-model", Vendor: registry.VendorOpenAI, Efforts: []string{registry.EffortMedium}, Enabled: true},
		{ID: "forbidden-model", Vendor: registry.VendorOpenAI, Efforts: []string{registry.EffortMedium}, Enabled: true},
	}
	keys := map[string]map[string]string{
		"key-1": {registry.VendorOpenAI: "test-openai-key"},
	}
	rules := []*proxydb.Rule{
		{ID: "r1", Name: "r1", Effort: registry.EffortMedium, Use: []string{"allowed-model"}, Enabled: true},
	}
	snap := registry.NewSnapshotForTest(models, rules, keys, nil)
	server := NewProxyServer(fixedSnapshot{snap}, nil)

	teamIdentity := &apikeys.Identity{
		KeyID:             "key-1",
		Name:              "team-key",
		OrgID:             "org-1",
		TeamID:            "team-1",
		TeamAllowedModels: []string{"allowed-model"},
	}
	ctx := apikeys.WithIdentity(context.Background(), teamIdentity)

	// Refused with PermissionDenied when requesting forbidden model
	_, err := server.GenerateText(ctx, &pb.GenerateTextRequest{
		ModelOverride: "forbidden-model",
		Messages: []*pb.ChatMessage{
			{Role: pb.MessageRole_MESSAGE_ROLE_USER, Text: "hello"},
		},
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for forbidden model, got: %v", err)
	}

	// Refused for speech media as well
	_, err = server.SynthesizeSpeech(ctx, &pb.SynthesizeSpeechRequest{
		ModelOverride: "forbidden-model",
		Text:          "hello",
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for forbidden media model, got: %v", err)
	}
}

func TestTeamAllowedModels_ChainFiltering(t *testing.T) {
	models := []*proxydb.Model{
		{ID: "m1", Vendor: registry.VendorOpenAI, Efforts: []string{registry.EffortMedium}, Enabled: true},
		{ID: "m2", Vendor: registry.VendorOpenAI, Efforts: []string{registry.EffortMedium}, Enabled: true},
	}
	rules := []*proxydb.Rule{
		{
			ID:       "rule-1",
			Name:     "medium-rule",
			Effort:   registry.EffortMedium,
			Use:      []string{"m1", "m2"},
			Enabled:  true,
			Priority: 10,
		},
	}
	keys := map[string]map[string]string{
		"key-1": {registry.VendorOpenAI: "test-openai-key"},
	}
	snap := registry.NewSnapshotForTest(models, rules, keys, nil)
	server := NewProxyServer(fixedSnapshot{snap}, nil)

	// Team only allows m2
	idWithM2 := &apikeys.Identity{
		KeyID:             "key-1",
		Name:              "team-key",
		TeamID:            "team-1",
		TeamAllowedModels: []string{"m2"},
	}
	ctxM2 := apikeys.WithIdentity(context.Background(), idWithM2)
	plan, err := server.planRoute(ctxM2, snap, idWithM2, &pb.GenerateTextRequest{
		Effort: pb.Effort_EFFORT_MEDIUM,
	})
	if err != nil {
		t.Fatalf("unexpected planRoute error: %v", err)
	}
	if len(plan.models) != 1 || plan.models[0].ID != "m2" {
		t.Fatalf("expected only m2 in planned chain, got: %v", plan.models)
	}

	// Team only allows m3 (neither m1 nor m2) -> PermissionDenied
	idWithM3 := &apikeys.Identity{
		KeyID:             "key-1",
		Name:              "team-key",
		TeamID:            "team-1",
		TeamAllowedModels: []string{"m3"},
	}
	_, err = server.planRoute(ctxM2, snap, idWithM3, &pb.GenerateTextRequest{
		Effort: pb.Effort_EFFORT_MEDIUM,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied when no chain models allowed, got: %v", err)
	}
}

func TestAdminService_OrganizationAndTeamValidation(t *testing.T) {
	server := NewAdminServer(nil, nil, nil)
	ctx := context.Background()

	validOrgID := "11111111-1111-1111-1111-111111111111"
	validTeamID := "22222222-2222-2222-2222-222222222222"

	// MintAPIKey UUID validation
	if _, err := server.MintAPIKey(ctx, &pb.MintAPIKeyRequest{Name: "k", OrgId: "not-a-uuid"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on invalid org_id in MintAPIKey, got %v", err)
	}
	if _, err := server.MintAPIKey(ctx, &pb.MintAPIKeyRequest{Name: "k", TeamId: "not-a-uuid"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on invalid team_id in MintAPIKey, got %v", err)
	}

	// Organization validation
	if _, err := server.CreateOrganization(ctx, &pb.CreateOrganizationRequest{Name: ""}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on empty org name, got %v", err)
	}
	if _, err := server.DeleteOrganization(ctx, &pb.DeleteOrganizationRequest{Id: ""}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on empty org id, got %v", err)
	}
	if _, err := server.DeleteOrganization(ctx, &pb.DeleteOrganizationRequest{Id: "invalid-uuid"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on non-uuid org id, got %v", err)
	}

	// Team validation
	if _, err := server.CreateTeam(ctx, &pb.CreateTeamRequest{OrgId: "", Name: "t"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on empty org_id, got %v", err)
	}
	if _, err := server.CreateTeam(ctx, &pb.CreateTeamRequest{OrgId: "invalid-uuid", Name: "t"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on non-uuid org_id, got %v", err)
	}
	if _, err := server.CreateTeam(ctx, &pb.CreateTeamRequest{OrgId: validOrgID, Name: ""}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on empty team name, got %v", err)
	}
	if _, err := server.CreateTeam(ctx, &pb.CreateTeamRequest{OrgId: validOrgID, Name: "t", MonthlyBudgetUsd: -10}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on negative budget, got %v", err)
	}
	if _, err := server.GetTeam(ctx, &pb.GetTeamRequest{Id: ""}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on empty get team id, got %v", err)
	}
	if _, err := server.GetTeam(ctx, &pb.GetTeamRequest{Id: "invalid-uuid"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on non-uuid get team id, got %v", err)
	}
	if _, err := server.ListTeams(ctx, &pb.ListTeamsRequest{OrgId: "invalid-uuid"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on non-uuid list teams org_id, got %v", err)
	}
	if _, err := server.UpdateTeam(ctx, &pb.UpdateTeamRequest{Id: "", Name: "t"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on empty update team id, got %v", err)
	}
	if _, err := server.UpdateTeam(ctx, &pb.UpdateTeamRequest{Id: "invalid-uuid", Name: "t"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on non-uuid update team id, got %v", err)
	}
	if _, err := server.UpdateTeam(ctx, &pb.UpdateTeamRequest{Id: validTeamID, Name: ""}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on empty update team name, got %v", err)
	}
	if _, err := server.UpdateTeam(ctx, &pb.UpdateTeamRequest{Id: validTeamID, Name: "t", MonthlyBudgetUsd: -5}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on negative update budget, got %v", err)
	}
	if _, err := server.DeleteTeam(ctx, &pb.DeleteTeamRequest{Id: ""}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on empty delete team id, got %v", err)
	}
	if _, err := server.DeleteTeam(ctx, &pb.DeleteTeamRequest{Id: "invalid-uuid"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument on non-uuid delete team id, got %v", err)
	}
}

func TestTeamBudgetValidation(t *testing.T) {
	server := NewAdminServer(nil, nil, nil)
	ctx := context.Background()
	org := "11111111-1111-1111-1111-111111111111"
	for _, usd := range []float64{math.NaN(), math.Inf(1), 1e11} {
		if _, err := server.CreateTeam(ctx, &pb.CreateTeamRequest{OrgId: org, Name: "t", MonthlyBudgetUsd: usd}); status.Code(err) != codes.InvalidArgument {
			t.Errorf("budget %v: err = %v, want InvalidArgument", usd, err)
		}
	}
	if got, err := teamBudgetNano(12.5); err != nil || got != 12_500_000_000 {
		t.Fatalf("teamBudgetNano(12.5) = %d, %v", got, err)
	}
}

// One owner for a team's cap: the generic spend-limit RPCs refuse it, so the
// team row and its limit cannot disagree.
func TestSpendLimitRPCsRefuseTeamSubject(t *testing.T) {
	server := NewAdminServer(nil, nil, nil)
	ctx := context.Background()
	_, err := server.SetSpendLimit(ctx, &pb.SetSpendLimitRequest{SpendLimit: &pb.SpendLimitSpec{
		SubjectType: "team", SubjectId: "22222222-2222-2222-2222-222222222222", Period: "day", LimitNanousd: 1e9,
	}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("SetSpendLimit(team) err = %v, want InvalidArgument", err)
	}
	if _, err := server.DeleteSpendLimit(ctx, &pb.DeleteSpendLimitRequest{SubjectType: "team", SubjectId: "x", Period: "month"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("DeleteSpendLimit(team) err = %v, want InvalidArgument", err)
	}
}

func TestTenantInUseIsFailedPrecondition(t *testing.T) {
	err := mutationStatus(fmt.Errorf("%w: 2 active api key(s)", proxydb.ErrTenantInUse))
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", status.Code(err))
	}
}

// Rules may target a team; the proxy sets team_id and org_id from the key and
// drops any the caller sent.
func TestRoutingAttributesComeFromTheKey(t *testing.T) {
	id := &apikeys.Identity{OrgID: "org-1", TeamID: "team-1"}
	got := routingAttributes(map[string]string{"team_id": "someone-else", "service": "search"}, id)
	if got["team_id"] != "team-1" || got["org_id"] != "org-1" || got["service"] != "search" {
		t.Fatalf("routing attributes = %v", got)
	}
	if got := routingAttributes(nil, id); got["team_id"] != "team-1" {
		t.Fatalf("nil attributes must still carry the key's team: %v", got)
	}
}

// The proxy's own judge calls are not the caller's to restrict.
func TestInternalCallsBypassTeamAllowlist(t *testing.T) {
	id := &apikeys.Identity{TeamAllowedModels: []string{"gpt-x"}}
	chain := []*proxydb.Model{{ID: "judge-1"}}
	if _, err := teamModels(context.Background(), id, chain); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("caller call err = %v, want PermissionDenied", err)
	}
	got, err := teamModels(withInternalCall(context.Background()), id, chain)
	if err != nil || len(got) != 1 {
		t.Fatalf("internal call: %v, %v; the configured judge must stay usable", got, err)
	}
}
