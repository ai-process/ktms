package mcpgw

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/usagereport"
	usagev1 "github.com/ai-process/ktms/internal/usagereport/gen/usage/v1"
)

type snaps struct {
	s atomic.Pointer[registry.Snapshot]
}

func (f *snaps) Snapshot() *registry.Snapshot { return f.s.Load() }

type verifier map[string]*apikeys.Identity

func (v verifier) VerifyScope(_ context.Context, plain, scope string) (*apikeys.Identity, error) {
	id := v[plain]
	if id == nil {
		return nil, apikeys.ErrUnauthenticated
	}
	if !id.HasScope(scope) {
		return nil, apikeys.ErrForbidden
	}
	return id, nil
}

type recorder struct {
	mu    sync.Mutex
	calls []usagereport.ToolCall
}

func (r *recorder) RecordToolCall(t usagereport.ToolCall) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, t)
	return nil
}

func (r *recorder) all() []usagereport.ToolCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

type limiter struct{ allow bool }

func (l limiter) AdmitTool(context.Context, string, string, string, int32) (bool, time.Duration) {
	return l.allow, 2 * time.Second
}

type echoArgs struct {
	Text string `json:"text"`
}

// upstreamServer is an MCP server with the given tools; each echoes its
// text prefixed by the tool name and reports the credential header it saw.
func upstreamServer(t *testing.T, sse bool, tools ...string) (url string, seen *atomic.Value, calls *atomic.Int32) {
	t.Helper()
	seen, calls = &atomic.Value{}, &atomic.Int32{}
	s := mcp.NewServer(&mcp.Implementation{Name: "up", Version: "1"}, nil)
	for _, name := range tools {
		mcp.AddTool(s, &mcp.Tool{Name: name, Description: "the " + name + " tool"},
			func(_ context.Context, _ *mcp.CallToolRequest, in echoArgs) (*mcp.CallToolResult, any, error) {
				calls.Add(1)
				if in.Text == "big" {
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("x", 4096)}}}, nil, nil
				}
				if in.Text == "slow" {
					time.Sleep(300 * time.Millisecond)
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name + ":" + in.Text}}}, nil, nil
			})
	}
	var h http.Handler
	if sse {
		h = mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return s }, nil)
	} else {
		h = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("X-Upstream-Token"); v != "" {
			seen.Store(v)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts.URL, seen, calls
}

type fixture struct {
	gw      *Gateway
	ts      *httptest.Server
	rec     *recorder
	snaps   *snaps
	githubN *atomic.Int32
	docsN   *atomic.Int32
	seen    *atomic.Value
}

func newFixture(t *testing.T, lim ToolLimiter) *fixture {
	t.Helper()
	ghURL, seen, ghN := upstreamServer(t, false, "create_issue", "delete_repo")
	docsURL, _, docsN := upstreamServer(t, true, "search")
	f := &fixture{rec: &recorder{}, snaps: &snaps{}, githubN: ghN, docsN: docsN, seen: seen}
	f.snaps.s.Store(&registry.Snapshot{MCPServers: map[string]*registry.MCPServer{
		"github": {MCPServer: proxydb.MCPServer{Name: "github", URL: ghURL, Transport: registry.MCPStreamableHTTP, Enabled: true, AllowPrivate: true, MaxResponseBytes: 2048, TimeoutMs: 200, ToolRPM: 5},
			Header: "X-Upstream-Token", Value: "upstream-secret"},
		"docs": {MCPServer: proxydb.MCPServer{Name: "docs", URL: docsURL + "/", Transport: registry.MCPSSE, Enabled: true, AllowPrivate: true}},
	}})
	v := verifier{
		"all":    {Name: "all", Scopes: []string{apikeys.ScopeMCP}},
		"narrow": {Name: "narrow", Scopes: []string{apikeys.ScopeMCP}, KeyAllowedMCP: []string{"github/create_issue", "docs"}},
		"team":   {Name: "team", Scopes: []string{apikeys.ScopeMCP}, TeamAllowedMCP: []string{"docs"}},
		"gen":    {Name: "gen", Scopes: []string{apikeys.ScopeGenerate}},
	}
	f.gw = New(f.snaps, v, lim, f.rec, "test")
	t.Cleanup(f.gw.Close)
	mux := http.NewServeMux()
	f.gw.Register(mux)
	f.ts = httptest.NewServer(mux)
	t.Cleanup(f.ts.Close)
	return f
}

type bearer struct{ key string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	r.Header.Set(UserHeader, "alice")
	return http.DefaultTransport.RoundTrip(r)
}

func (f *fixture) connect(t *testing.T, key, path string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
	s, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: f.ts.URL + path, HTTPClient: &http.Client{Transport: bearer{key}}}, nil)
	if err != nil {
		t.Fatalf("connect %s as %s: %v", path, key, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func toolNames(t *testing.T, s *mcp.ClientSession) []string {
	t.Helper()
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

func text(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	if len(r.Content) != 1 {
		t.Fatalf("content = %v", r.Content)
	}
	return r.Content[0].(*mcp.TextContent).Text
}

// Claude Code refuses a tools/list result whose cacheScope is not public or private.
func TestToolsListCacheScope(t *testing.T) {
	f := newFixture(t, nil)
	req, _ := http.NewRequest(http.MethodPost, f.ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer all")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Result struct {
			CacheScope string `json:"cacheScope"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Result.CacheScope != "private" {
		t.Fatalf("cacheScope = %q, %v", body.Result.CacheScope, err)
	}
}

func TestAggregateListsAllowedToolsOfTwoServers(t *testing.T) {
	f := newFixture(t, nil)
	if got := toolNames(t, f.connect(t, "all", "/mcp")); !slices.Equal(got, []string{"docs__search", "github__create_issue", "github__delete_repo"}) {
		t.Fatalf("all: %v", got)
	}
	if got := toolNames(t, f.connect(t, "narrow", "/mcp")); !slices.Equal(got, []string{"docs__search", "github__create_issue"}) {
		t.Fatalf("narrow: %v", got)
	}
	if got := toolNames(t, f.connect(t, "team", "/mcp")); !slices.Equal(got, []string{"docs__search"}) {
		t.Fatalf("team: %v", got)
	}
}

func TestCallsReachUpstreamWithCredentialAndAreAccounted(t *testing.T) {
	f := newFixture(t, nil)
	s := f.connect(t, "narrow", "/mcp")
	for _, c := range []struct{ name, want string }{
		{"github__create_issue", "create_issue:hi"},
		{"docs__search", "search:hi"},
	} {
		res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: c.name, Arguments: map[string]any{"text": "hi"}})
		if err != nil || res.IsError {
			t.Fatalf("%s: %v %v", c.name, err, res)
		}
		if got := text(t, res); got != c.want {
			t.Fatalf("%s = %q", c.name, got)
		}
	}
	if v, _ := f.seen.Load().(string); v != "upstream-secret" {
		t.Fatalf("upstream saw credential %q", v)
	}
	calls := f.rec.all()
	if len(calls) != 2 {
		t.Fatalf("accounted %d calls", len(calls))
	}
	c := calls[0]
	if c.Server != "github" || c.Tool != "create_issue" || c.Status != usagev1.Status_STATUS_SUCCESS || c.UserID != "alice" || c.ResponseBytes == 0 || c.Meta["api_key"] != "narrow" {
		t.Fatalf("event = %+v", c)
	}
	for _, c := range calls {
		b, _ := json.Marshal(c)
		if strings.Contains(string(b), "upstream-secret") {
			t.Fatalf("credential in usage event: %s", b)
		}
	}
}

func TestForbiddenToolAndServerAreRefusedWithoutCallingUpstream(t *testing.T) {
	f := newFixture(t, nil)
	s := f.connect(t, "narrow", "/mcp")
	_, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "github__delete_repo", Arguments: map[string]any{"text": "x"}})
	if err == nil || !strings.Contains(err.Error(), "permission_denied") {
		t.Fatalf("delete_repo: %v", err)
	}
	if _, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "docs__search", Arguments: map[string]any{"text": "x"}}); err != nil {
		t.Fatalf("a refusal must not end the session: %v", err)
	}
	if n := f.githubN.Load(); n != 0 {
		t.Fatalf("upstream called %d times", n)
	}
	if calls := f.rec.all(); len(calls) != 1 || calls[0].Server != "docs" {
		t.Fatalf("only the docs call is accounted, got %+v", calls)
	}

	// The team's list refuses github as a whole, on /mcp/github too.
	c := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
	_, err = c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: f.ts.URL + "/mcp/github", HTTPClient: &http.Client{Transport: bearer{"team"}}}, nil)
	if err == nil || !strings.Contains(err.Error(), "Forbidden") {
		t.Fatalf("team on /mcp/github: %v", err)
	}
}

func TestSingleServerEndpointKeepsToolNames(t *testing.T) {
	f := newFixture(t, nil)
	s := f.connect(t, "narrow", "/mcp/github")
	if got := toolNames(t, s); !slices.Equal(got, []string{"create_issue"}) {
		t.Fatalf("tools: %v", got)
	}
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_issue", Arguments: map[string]any{"text": "a"}})
	if err != nil || text(t, res) != "create_issue:a" {
		t.Fatalf("call: %v %v", err, res)
	}
}

func TestAuthentication(t *testing.T) {
	f := newFixture(t, nil)
	for _, c := range []struct {
		key  string
		want int
	}{{"", http.StatusUnauthorized}, {"wrong", http.StatusUnauthorized}, {"gen", http.StatusForbidden}} {
		req, _ := http.NewRequest(http.MethodPost, f.ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if c.key != "" {
			req.Header.Set("Authorization", "Bearer "+c.key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Fatalf("key %q: status %d, want %d", c.key, resp.StatusCode, c.want)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, f.ts.URL+"/mcp/nope", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer all")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown server: %d", resp.StatusCode)
	}
}

func TestLimits(t *testing.T) {
	f := newFixture(t, nil)
	s := f.connect(t, "all", "/mcp")
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "github__create_issue", Arguments: map[string]any{"text": "big"}})
	if err != nil || !res.IsError || !strings.Contains(text(t, res), "max_response_bytes") {
		t.Fatalf("big: %v %v", err, res)
	}
	res, err = s.CallTool(context.Background(), &mcp.CallToolParams{Name: "github__create_issue", Arguments: map[string]any{"text": "slow"}})
	if err != nil || !res.IsError || !strings.Contains(text(t, res), "did not answer") {
		t.Fatalf("slow: %v %v", err, res)
	}
	calls := f.rec.all()
	if len(calls) != 2 || calls[0].ErrorClass != "too_large" || calls[1].Status != usagev1.Status_STATUS_TIMEOUT {
		t.Fatalf("accounted %+v", calls)
	}

	f = newFixture(t, limiter{allow: false})
	s = f.connect(t, "all", "/mcp")
	res, err = s.CallTool(context.Background(), &mcp.CallToolParams{Name: "github__create_issue", Arguments: map[string]any{"text": "a"}})
	if err != nil || !res.IsError || !strings.Contains(text(t, res), "throttled") || f.githubN.Load() != 0 {
		t.Fatalf("throttled: %v %v", err, res)
	}
}

func TestReconnectsWhenTheCredentialChanges(t *testing.T) {
	f := newFixture(t, nil)
	s := f.connect(t, "all", "/mcp")
	call := func() {
		if _, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "github__create_issue", Arguments: map[string]any{"text": "a"}}); err != nil {
			t.Fatal(err)
		}
	}
	call()
	old := f.snaps.Snapshot()
	gh := *old.MCPServers["github"]
	gh.Value = "rotated"
	f.snaps.s.Store(&registry.Snapshot{MCPServers: map[string]*registry.MCPServer{"github": &gh, "docs": old.MCPServers["docs"]}})
	call()
	if v, _ := f.seen.Load().(string); v != "rotated" {
		t.Fatalf("upstream saw %q after rotation", v)
	}
}
