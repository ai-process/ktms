package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// session connects an MCP client to a ktms mcp server in memory.
func session(t *testing.T, allowWrite bool) *mcp.ClientSession {
	t.Helper()
	a := &app{stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard, timeout: 30 * time.Second}
	a.t.warn = io.Discard
	if _, err := a.t.adminKey(); err != nil {
		t.Fatal(err)
	}
	srv := a.mcpServer(allowWrite)
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	go func() { _ = srv.Run(ctx, st) }()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func TestMCPAgainstKeystone(t *testing.T) {
	addr := keystone(t)
	isolate(t)
	t.Setenv("KTMS_ADDR", addr)
	t.Setenv("KTMS_ADMIN_KEY", bootstrapKey)
	t.Setenv("KTMS_HTTP", "http://127.0.0.1:1")

	ro := session(t, false)
	names := toolNames(t, ro)
	for _, w := range []string{"config_apply", "config_plan", "mint_key", "revoke_key", "drain_node", "set_spend_limit"} {
		if slices.Contains(names, w) {
			t.Fatalf("read-only server offers %s: %v", w, names)
		}
	}
	tools, _ := ro.ListTools(context.Background(), nil)
	for _, tl := range tools.Tools {
		if tl.Annotations == nil || !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", tl.Name)
		}
	}

	rw := session(t, true)
	tools, _ = rw.ListTools(context.Background(), nil)
	for _, tl := range tools.Tools {
		if tl.Name == "revoke_key" && (tl.Annotations.ReadOnlyHint || tl.Annotations.DestructiveHint == nil || !*tl.Annotations.DestructiveHint) {
			t.Errorf("revoke_key annotations = %+v", tl.Annotations)
		}
	}

	// The assistant's path: plan a file that adds a model and a rule, then apply it.
	file := `api_version: ktms.ai-process.dev/v1alpha1
models:
  - {id: gpt-4o-mini, vendor: openai, efforts: [EFFORT_LOW, EFFORT_MEDIUM, EFFORT_HIGH], price_in_per_mtok: 0.15, price_out_per_mtok: 0.6}
rules:
  - {name: default, priority: 1, use: [gpt-4o-mini]}
`
	out, isErr := callTool(t, rw, "config_plan", map[string]any{"config": file})
	if isErr || !strings.Contains(out, `"plan_id"`) || !strings.Contains(out, "+ model gpt-4o-mini") {
		t.Fatalf("config_plan: %s", out)
	}
	id := between(out, `"plan_id": "`, `"`)
	if out, isErr = callTool(t, rw, "config_apply", map[string]any{"plan_id": id}); isErr || !strings.Contains(out, "Applied.") {
		t.Fatalf("config_apply: %s", out)
	}
	if out, isErr = callTool(t, rw, "config_apply", map[string]any{"plan_id": id}); !isErr {
		t.Fatalf("a plan applied twice: %s", out)
	}
	if out, _ = callTool(t, rw, "config_plan", map[string]any{"config": file}); !strings.Contains(out, `"changes": false`) {
		t.Fatalf("plan after apply: %s", out)
	}
	if out, _ = callTool(t, ro, "list_models", nil); !strings.Contains(out, `"id": "gpt-4o-mini"`) {
		t.Fatalf("list_models: %s", out)
	}

	// A plan goes stale when Keystone changes before it is applied.
	file2 := strings.Replace(file, "price_in_per_mtok: 0.15", "price_in_per_mtok: 0.2", 1)
	out, _ = callTool(t, rw, "config_plan", map[string]any{"config": file2})
	stale := between(out, `"plan_id": "`, `"`)
	out, _ = callTool(t, rw, "config_plan", map[string]any{"config": strings.Replace(file, "0.15", "0.3", 1)})
	if _, isErr = callTool(t, rw, "config_apply", map[string]any{"plan_id": between(out, `"plan_id": "`, `"`)}); isErr {
		t.Fatal("apply of a fresh plan failed")
	}
	if out, isErr = callTool(t, rw, "config_apply", map[string]any{"plan_id": stale}); !isErr || !strings.Contains(out, "changed since config_plan") {
		t.Fatalf("stale plan applied: %s", out)
	}

	// A new key comes once, for the user.
	res, err := rw.CallTool(context.Background(), &mcp.CallToolParams{Name: "mint_key", Arguments: map[string]any{"name": "bot"}})
	if err != nil || res.IsError {
		t.Fatalf("mint_key: %v %+v", err, res)
	}
	tc := res.Content[0].(*mcp.TextContent)
	if !strings.Contains(tc.Text, "bot: ktms_") || tc.Annotations == nil || !slices.Contains(tc.Annotations.Audience, mcp.Role("user")) {
		t.Fatalf("mint_key content = %+v", tc)
	}
	if out, _ = callTool(t, ro, "list_keys", nil); strings.Contains(out, "ktms_") || !strings.Contains(out, `"name": "bot"`) {
		t.Fatalf("list_keys: %s", out)
	}
	if out, isErr = callTool(t, rw, "set_spend_limit", map[string]any{"subject_type": "key", "subject_id": "bot", "period": "day", "limit_usd": 2}); isErr {
		t.Fatalf("set_spend_limit: %s", out)
	}
	if out, _ = callTool(t, ro, "list_spend_limits", nil); !strings.Contains(out, `"limit_nanousd": "2000000000"`) {
		t.Fatalf("list_spend_limits: %s", out)
	}
	if out, isErr = callTool(t, ro, "get_spend", map[string]any{"subject_type": "nobody", "subject_id": "x"}); !isErr || !strings.Contains(out, "InvalidArgument") {
		t.Fatalf("get_spend with a bad subject: %s", out)
	}
	if out, isErr = callTool(t, ro, "health", nil); isErr || !strings.Contains(out, `"ready": false`) {
		t.Fatalf("health: %s", out)
	}

	// Resources and prompts.
	rr, err := ro.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "ktms://config"})
	if err != nil || !strings.Contains(rr.Contents[0].Text, "gpt-4o-mini") {
		t.Fatalf("ktms://config: %v", err)
	}
	if rr, err = ro.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "ktms://schema"}); err != nil || !strings.Contains(rr.Contents[0].Text, `"$schema"`) {
		t.Fatalf("ktms://schema: %v", err)
	}
	gp, err := ro.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "configure_routing", Arguments: map[string]string{"requirements": "one cheap model"}})
	if err != nil || !strings.Contains(gp.Messages[0].Content.(*mcp.TextContent).Text, "read-only") {
		t.Fatalf("configure_routing prompt: %v", err)
	}
	if _, err := ro.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "configure_routing"}); err == nil {
		t.Fatal("a prompt without its required argument")
	}

	// A read key reads, and the write tools refuse with it.
	code, keyOut, _ := call("keys", "mint", "viewer", "--scopes", "admin:read")
	if code != 0 {
		t.Fatal("mint viewer")
	}
	t.Setenv("KTMS_ADMIN_KEY", strings.TrimSpace(keyOut))
	rk := session(t, true)
	if _, isErr = callTool(t, rk, "list_models", nil); isErr {
		t.Fatal("read key cannot list models")
	}
	if out, isErr = callTool(t, rk, "revoke_key", map[string]any{"name": "bot"}); !isErr || !strings.Contains(out, "PermissionDenied") {
		t.Fatalf("read key revoked: %s", out)
	}
}

func between(s, from, to string) string {
	i := strings.Index(s, from)
	if i < 0 {
		return ""
	}
	s = s[i+len(from):]
	if j := strings.Index(s, to); j >= 0 {
		return s[:j]
	}
	return s
}

type bearerRT struct{ token string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestMCPOverHTTP(t *testing.T) {
	isolate(t)
	t.Setenv("KTMS_ADMIN_KEY", "ktms_x")
	t.Setenv("KTMS_ADDR", "127.0.0.1:1")
	a := &app{stdin: strings.NewReader(""), stdout: io.Discard, stderr: io.Discard, timeout: time.Second}
	a.t.warn = io.Discard
	if _, err := a.t.adminKey(); err != nil {
		t.Fatal(err)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	_ = l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	token := "0123456789abcdef-test"
	go func() { _ = a.serveMCP(ctx, a.mcpServer(false), addr, token) }()
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			_ = c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	endpoint := "http://" + addr + "/mcp"

	res, err := http.Post(endpoint, "application/json", strings.NewReader(`{}`))
	if err != nil || res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %v %v", err, res.StatusCode)
	}
	res.Body.Close()

	connect := func(tok string) (*mcp.ClientSession, error) {
		return mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
			Endpoint: endpoint, HTTPClient: &http.Client{Transport: bearerRT{tok}}, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	}
	if _, err := connect("wrong-token-wrong"); err == nil {
		t.Fatal("connected with a wrong token")
	}
	cs, err := connect(token)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if names := toolNames(t, cs); !slices.Contains(names, "list_models") {
		t.Fatalf("tools over HTTP: %v", names)
	}
	// Keystone is unreachable here: the tool reports it as a tool error.
	if out, isErr := callTool(t, cs, "list_models", nil); !isErr || !strings.Contains(out, "Unavailable") {
		t.Fatalf("list_models without Keystone: %s", out)
	}
}

func TestMCPNeedsAToken(t *testing.T) {
	isolate(t)
	t.Setenv("KTMS_ADMIN_KEY", "ktms_x")
	t.Setenv("KTMS_MCP_TOKEN", "short")
	if code, _, errs := call("mcp", "--listen", "127.0.0.1:0"); code != exitUsage || !strings.Contains(errs, "bearer token") {
		t.Fatalf("mcp --listen with a short token = %d %q", code, errs)
	}
}
