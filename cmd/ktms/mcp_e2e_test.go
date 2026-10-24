//go:build e2e

package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestMCPStdio drives the built binary's ktms mcp over stdio, as an assistant
// would: read the configuration, add a model and a rule through plan and
// apply. scripts/cli-e2e.sh runs it against a live Keystone.
func TestMCPStdio(t *testing.T) {
	bin, mock := os.Getenv("KTMS_E2E_BIN"), os.Getenv("KTMS_E2E_MOCK")
	if bin == "" || mock == "" {
		t.Skip("KTMS_E2E_BIN and KTMS_E2E_MOCK are set by scripts/cli-e2e.sh")
	}
	ctx := context.Background()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "e2e"}, nil).Connect(ctx,
		&mcp.CommandTransport{Command: exec.Command(bin, "mcp", "--allow-write")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	call := func(name string, args map[string]any) string {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var b strings.Builder
		for _, c := range res.Content {
			b.WriteString(c.(*mcp.TextContent).Text)
		}
		if res.IsError {
			t.Fatalf("%s: %s", name, b.String())
		}
		return b.String()
	}

	if out := call("get_config", nil); !strings.Contains(out, "mock-model") {
		t.Fatalf("get_config:\n%s", out)
	}
	file := `api_version: ktms.ai-process.dev/v1alpha1
models:
  - {id: mock-model, vendor: openai, endpoint: "` + mock + `", efforts: [EFFORT_LOW, EFFORT_MEDIUM, EFFORT_HIGH], price_in_per_mtok: 1000, price_out_per_mtok: 2000}
  - {id: mcp-model, vendor: openai, endpoint: "` + mock + `", efforts: [EFFORT_LOW, EFFORT_MEDIUM, EFFORT_HIGH], price_in_per_mtok: 1, price_out_per_mtok: 2}
rules:
  - {name: from-mcp, priority: 5, attributes: {tier: mcp}, use: [mcp-model]}
  - {name: default, priority: 1, use: [mock-model]}
`
	out := call("config_plan", map[string]any{"config": file})
	if !strings.Contains(out, "+ model mcp-model") || !strings.Contains(out, "+ rule from-mcp") {
		t.Fatalf("config_plan:\n%s", out)
	}
	id := between(out, `"plan_id": "`, `"`)
	if out := call("config_apply", map[string]any{"plan_id": id}); !strings.Contains(out, "Applied.") {
		t.Fatalf("config_apply:\n%s", out)
	}
	if out := call("list_rules", nil); !strings.Contains(out, `"name": "from-mcp"`) {
		t.Fatalf("list_rules:\n%s", out)
	}
}
