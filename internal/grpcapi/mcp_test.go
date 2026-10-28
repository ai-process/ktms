package grpcapi

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/registry"
)

func TestMCPServerRegistryAndCredential(t *testing.T) {
	db := rotateTestDB(t)
	ctx := context.Background()
	const secret = "Bearer ghp_upstream_secret"
	s := NewAdminServer(db, keyring(t, 1), nil)

	for _, c := range []struct {
		spec *pb.MCPServerSpec
		code codes.Code
	}{
		{&pb.MCPServerSpec{Name: "local", Url: "https://127.0.0.1/mcp", Enabled: true}, codes.InvalidArgument},
		{&pb.MCPServerSpec{Name: "meta", Url: "https://169.254.169.254/", Enabled: true}, codes.InvalidArgument},
		{&pb.MCPServerSpec{Name: "plain", Url: "http://93.184.215.14/mcp", Enabled: true}, codes.InvalidArgument},
		{&pb.MCPServerSpec{Name: "Bad_Name", Url: "https://93.184.215.14/mcp"}, codes.InvalidArgument},
		{&pb.MCPServerSpec{Name: "sse", Url: "https://93.184.215.14/sse", Transport: "ws"}, codes.InvalidArgument},
		{&pb.MCPServerSpec{Name: "creds", Url: "https://u:p@93.184.215.14/mcp"}, codes.InvalidArgument},
		{&pb.MCPServerSpec{Name: "inside", Url: "http://10.0.0.7:8000/mcp", Enabled: true, AllowPrivate: true}, codes.OK},
		{&pb.MCPServerSpec{Name: "github", Url: "https://93.184.215.14/mcp", Enabled: true, ToolRpm: 30}, codes.OK},
	} {
		_, err := s.UpsertMCPServer(ctx, &pb.UpsertMCPServerRequest{Server: c.spec})
		if status.Code(err) != c.code {
			t.Fatalf("%s: %v, want %s", c.spec.GetName(), err, c.code)
		}
	}
	if _, err := s.SetMCPCredential(ctx, &pb.SetMCPCredentialRequest{Server: "nope", PlainValue: "x"}); status.Code(err) != codes.NotFound {
		t.Fatalf("credential of an unknown server: %v", err)
	}
	if _, err := s.SetMCPCredential(ctx, &pb.SetMCPCredentialRequest{Server: "github", Header: "Bad Header", PlainValue: "x"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad header: %v", err)
	}
	if _, err := s.SetMCPCredential(ctx, &pb.SetMCPCredentialRequest{Server: "github", PlainValue: secret}); err != nil {
		t.Fatal(err)
	}

	cfg, err := s.GetConfig(ctx, &pb.GetConfigRequest{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := protojson.Marshal(cfg)
	if strings.Contains(string(b), "ghp_upstream_secret") {
		t.Fatalf("GetConfig returned the credential: %s", b)
	}
	if len(cfg.GetMcpServers()) != 2 || cfg.GetMcpServers()[0].GetTransport() != registry.MCPStreamableHTTP {
		t.Fatalf("servers = %v", cfg.GetMcpServers())
	}
	creds := cfg.GetMcpCredentials()
	if len(creds) != 1 || creds[0].GetHeader() != "Authorization" || creds[0].GetFingerprint() == "" {
		t.Fatalf("credentials = %v", creds)
	}

	// The loader hands the gateway the decrypted credential.
	l := registry.NewLoader(db, keyring(t, 1), nil)
	if err := l.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if gh := l.Snapshot().MCPServers["github"]; gh == nil || gh.Value != secret || gh.ToolRPM != 30 {
		t.Fatalf("snapshot server = %+v", gh)
	}

	// Rotation re-encrypts MCP credentials with the vendor keys.
	both := NewAdminServer(db, keyring(t, 1, 2), nil)
	if res, err := both.RotateEncryption(ctx, &pb.RotateEncryptionRequest{}); err != nil || res.GetReencrypted() != 1 {
		t.Fatalf("rotation = %v, %v", res, err)
	}
	l = registry.NewLoader(db, keyring(t, 2), nil)
	if err := l.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if gh := l.Snapshot().MCPServers["github"]; gh == nil || gh.Value != secret {
		t.Fatalf("after rotation, version 2 alone reads %+v", gh)
	}

	if _, err := s.DeleteMCPServer(ctx, &pb.DeleteMCPServerRequest{Name: "github"}); err != nil {
		t.Fatal(err)
	}
	if creds, _ := db.LoadMCPCredentials(ctx); len(creds) != 0 {
		t.Fatalf("credential outlived its server: %v", creds)
	}
	if _, err := s.ListMCPTools(ctx, &pb.ListMCPToolsRequest{Server: "inside"}); status.Code(err) != codes.Unavailable {
		t.Fatalf("ListMCPTools without a gateway: %v", err)
	}
}

func TestKeyAndTeamAllowedMCP(t *testing.T) {
	db := rotateTestDB(t)
	ctx := context.Background()
	s := NewAdminServer(db, keyring(t, 1), nil)
	org, err := s.CreateOrganization(ctx, &pb.CreateOrganizationRequest{Name: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeam(ctx, &pb.CreateTeamRequest{OrgId: org.GetOrganization().GetId(), Name: "t", AllowedMcp: []string{"github/"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad entry: %v", err)
	}
	team, err := s.CreateTeam(ctx, &pb.CreateTeamRequest{OrgId: org.GetOrganization().GetId(), Name: "t", AllowedMcp: []string{"github", "docs"}})
	if err != nil {
		t.Fatal(err)
	}
	name := "agent-" + uuid.NewString()[:8]
	if _, err := s.MintAPIKey(ctx, &pb.MintAPIKeyRequest{Name: name, Scopes: []string{"mcp"}, TeamId: team.GetTeam().GetId(),
		Policy: &pb.APIKeyPolicy{AllowedMcp: []string{"github/create_issue", "github/create_issue"}}}); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAPIKeyByName(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	id := apikeys.IdentityFor(row)
	if !slices.Equal(row.AllowedMCP, []string{"github/create_issue"}) || !id.CanUseTool("github", "create_issue") ||
		id.CanUseTool("github", "delete_repo") || id.CanUseTool("docs", "search") || !id.HasScope(apikeys.ScopeMCP) {
		t.Fatalf("identity = %+v", id)
	}

	if _, err := s.SetAPIKeyPolicy(ctx, &pb.SetAPIKeyPolicyRequest{Name: name}); err != nil {
		t.Fatal(err)
	}
	row, _ = db.GetAPIKeyByName(ctx, name)
	id = apikeys.IdentityFor(row)
	if !id.CanUseTool("docs", "search") || id.CanUseTool("slack", "post") || id.MCPDeniedBy("slack", "post") != "team" {
		t.Fatalf("without a key list the team's applies: %+v", id)
	}
	keys, err := s.ListAPIKeys(ctx, &pb.ListAPIKeysRequest{})
	if err != nil || len(keys.GetKeys()) != 1 || keys.GetKeys()[0].GetPolicy() != nil {
		t.Fatalf("keys = %v, %v", keys, err)
	}
	got, err := s.GetTeam(ctx, &pb.GetTeamRequest{Id: team.GetTeam().GetId()})
	if err != nil || !slices.Equal(got.GetTeam().GetAllowedMcp(), []string{"github", "docs"}) {
		t.Fatalf("team = %v, %v", got, err)
	}
}

func TestKeyAndTeamDataClassification(t *testing.T) {
	db := rotateTestDB(t)
	ctx := context.Background()
	s := NewAdminServer(db, keyring(t, 1), nil)
	org, err := s.CreateOrganization(ctx, &pb.CreateOrganizationRequest{Name: "clinic"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateTeam(ctx, &pb.CreateTeamRequest{OrgId: org.GetOrganization().GetId(), Name: "t", DataClassification: "secret"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad class: %v", err)
	}
	team, err := s.CreateTeam(ctx, &pb.CreateTeamRequest{OrgId: org.GetOrganization().GetId(), Name: "t", DataClassification: apikeys.DataRestricted})
	if err != nil {
		t.Fatal(err)
	}
	name := "notes-" + uuid.NewString()[:8]
	if _, err := s.MintAPIKey(ctx, &pb.MintAPIKeyRequest{Name: name, Scopes: []string{"generate"}, TeamId: team.GetTeam().GetId()}); err != nil {
		t.Fatal(err)
	}
	row, _ := db.GetAPIKeyByName(ctx, name)
	if id := apikeys.IdentityFor(row); !id.Restricted() || id.KeyDataClassification != "" {
		t.Fatalf("team restriction: %+v", id)
	}

	// The key's own restriction stays when the team's is lifted.
	if _, err := s.SetAPIKeyPolicy(ctx, &pb.SetAPIKeyPolicyRequest{Name: name, Policy: &pb.APIKeyPolicy{DataClassification: apikeys.DataRestricted}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateTeam(ctx, &pb.UpdateTeamRequest{Id: team.GetTeam().GetId(), Name: "t"}); err != nil {
		t.Fatal(err)
	}
	row, _ = db.GetAPIKeyByName(ctx, name)
	if id := apikeys.IdentityFor(row); !id.Restricted() || id.TeamDataClassification != "" {
		t.Fatalf("key restriction: %+v", id)
	}
	keys, _ := s.ListAPIKeys(ctx, &pb.ListAPIKeysRequest{})
	got, _ := s.GetTeam(ctx, &pb.GetTeamRequest{Id: team.GetTeam().GetId()})
	if len(keys.GetKeys()) != 1 || keys.GetKeys()[0].GetPolicy().GetDataClassification() != apikeys.DataRestricted || got.GetTeam().GetDataClassification() != "" {
		t.Fatalf("keys %v team %v", keys, got)
	}
	if _, err := s.SetAPIKeyPolicy(ctx, &pb.SetAPIKeyPolicyRequest{Name: name, Policy: &pb.APIKeyPolicy{DataClassification: "public"}}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad key class: %v", err)
	}
}
