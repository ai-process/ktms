package declarative

import (
	"bytes"
	"strings"
	"testing"

	pb "github.com/ai-process/ktms/gen/ktms/v1"
	"github.com/ai-process/ktms/internal/crypto"
)

const mcpExample = `mcp_servers:
  - name: github
    url: https://api.githubcopilot.com/mcp/
    tool_rpm: 60
  - name: docs
    url: http://docs-mcp:8000/sse
    transport: sse
    allow_private: true
mcp_credentials:
  - server: github
    value_env: GITHUB_MCP_TOKEN
`

func TestPlanMCP(t *testing.T) {
	c := mustParse(t, example+mcpExample)
	envs := env(map[string]string{"TEST_OPENAI_KEY": "sk-1", "GITHUB_MCP_TOKEN": "Bearer ghp-1"})
	p, err := Build(c, emptyState(), Options{Env: envs})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	p.Write(&buf)
	out := buf.String()
	if i, j := strings.Index(out, "+ mcp server github"), strings.Index(out, "+ mcp credential github"); i < 0 || j < i {
		t.Fatalf("server before its credential:\n%s", out)
	}
	if strings.Contains(out, "ghp-1") {
		t.Fatal("the plan shows the credential")
	}

	live := liveOf(t, c, "sk-1")
	live.Config.McpServers = []*pb.MCPServerSpec{
		{Name: "github", Url: "https://api.githubcopilot.com/mcp/", Transport: "streamable_http", Enabled: true, ToolRpm: 60},
		{Name: "docs", Url: "http://docs-mcp:8000/sse", Transport: "sse", Enabled: true, AllowPrivate: true},
		{Name: "old", Url: "https://old.example/mcp", Transport: "streamable_http", Enabled: true},
	}
	live.Config.McpCredentials = []*pb.MCPCredentialInfo{{Server: "github", Header: "Authorization", Fingerprint: crypto.Fingerprint("Bearer ghp-0")}}
	p, err = Build(c, live, Options{Env: envs, Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	p.Write(&buf)
	out = buf.String()
	for _, want := range []string{"~ mcp credential github", "changed in $GITHUB_MCP_TOKEN", "- mcp server old"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "mcp server github") || strings.Contains(out, "mcp server docs") {
		t.Errorf("a default transport is not a change:\n%s", out)
	}

	if _, err := Build(c, emptyState(), Options{Env: env(map[string]string{"TEST_OPENAI_KEY": "sk-1"})}); err == nil || !strings.Contains(err.Error(), "GITHUB_MCP_TOKEN is not set") {
		t.Fatalf("missing variable: %v", err)
	}
	if _, err := Parse([]byte("api_version: " + APIVersion + "\nmcp_servers: []\nmcp_credentials:\n  - server: nope\n    value_env: X\n")); err == nil || !strings.Contains(err.Error(), "not in mcp_servers") {
		t.Fatalf("credential of an unknown server: %v", err)
	}
}

func TestExportMCPParsesBack(t *testing.T) {
	live := emptyState()
	live.Config.McpServers = []*pb.MCPServerSpec{{Name: "github", Url: "https://x.example/mcp", Transport: "streamable_http", Enabled: false}}
	live.Config.McpCredentials = []*pb.MCPCredentialInfo{{Server: "github", Header: "Authorization"}}
	live.Teams = []*pb.Team{{Id: "t1", OrgId: "o1", Name: "t", AllowedMcp: []string{"github"}}}
	live.Orgs = []*pb.Organization{{Id: "o1", Name: "o"}}
	var buf bytes.Buffer
	if err := Export(live, &buf); err != nil {
		t.Fatal(err)
	}
	c, err := Parse(buf.Bytes())
	if err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	if len(c.MCPServers) != 1 || c.MCPServers[0].GetEnabled() || c.MCPCredentials[0].ValueEnv != "KTMS_MCP_CREDENTIAL_GITHUB" ||
		c.Organizations[0].Teams[0].AllowedMCP[0] != "github" {
		t.Fatalf("parsed back %+v\n%s", c, buf.String())
	}
}
