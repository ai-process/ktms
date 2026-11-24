package mcpgw

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

// tap keeps the raw bodies of tools/call responses.
type tap struct {
	bearer
	mu     sync.Mutex
	bodies []string
}

func (t *tap) RoundTrip(r *http.Request) (*http.Response, error) {
	var call bool
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		call = bytes.Contains(b, []byte(`"tools/call"`))
		r.Body = io.NopCloser(bytes.NewReader(b))
	}
	res, err := t.bearer.RoundTrip(r)
	if err != nil || !call {
		return res, err
	}
	b, _ := io.ReadAll(res.Body)
	res.Body = io.NopCloser(bytes.NewReader(b))
	t.mu.Lock()
	t.bodies = append(t.bodies, string(b))
	t.mu.Unlock()
	return res, nil
}

// Claude Code 2.1 and other MCP 2026-07-28 clients refuse a tools/call result without resultType.
func TestCallResultCarriesResultType(t *testing.T) {
	f := newFixture(t, nil)
	tp := &tap{bearer: bearer{"all"}}
	c := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil)
	s, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: f.ts.URL + "/mcp", HTTPClient: &http.Client{Transport: tp}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "github__create_issue"}); err != nil {
		t.Fatal(err)
	}
	if len(tp.bodies) == 0 {
		t.Fatal("no tools/call response seen")
	}
	for _, b := range tp.bodies {
		if !strings.Contains(b, `"resultType":"complete"`) {
			t.Fatalf("tools/call result has no resultType: %s", b)
		}
	}
}

// A result that carries resultType already is passed through, and the bytes
// callTool marshalled are reused rather than marshalled again.
func TestCompletedSkipsAndReuses(t *testing.T) {
	req := &mcp.CallToolRequest{}
	var marked mcp.CallToolResult
	if err := json.Unmarshal([]byte(`{"content":[{"type":"text","text":"up"}],"resultType":"complete"}`), &marked); err != nil {
		t.Fatal(err)
	}
	if got := completed(req, &marked, nil); got != &marked {
		t.Fatal("a result with resultType was rebuilt")
	}
	res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "from res"}}}
	raw := []byte(`{"content":[{"type":"text","text":"from raw"}]}`)
	got := completed(req, res, raw)
	if got == res || got.Content[0].(*mcp.TextContent).Text != "from raw" {
		t.Fatalf("raw was not reused: %+v", got)
	}
	if b, _ := json.Marshal(got); !strings.HasSuffix(string(b), `"resultType":"complete"}`) {
		t.Fatalf("no resultType: %s", b)
	}
	if b, _ := json.Marshal(completed(req, &mcp.CallToolResult{}, nil)); !strings.Contains(string(b), `"resultType":"complete"`) {
		t.Fatalf("empty result: %s", b)
	}
}

// An upstream registered as agents before the name was reserved is shadowed
// by the inbox; the operator is told once per configuration.
func TestShadowedUpstreamIsLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := log.Logger
	log.Logger = zerolog.New(&buf)
	t.Cleanup(func() { log.Logger = prev })
	sn := &snaps{}
	shadowed := &registry.Snapshot{MCPServers: map[string]*registry.MCPServer{"agents": {MCPServer: proxydb.MCPServer{Name: "agents"}}}}
	sn.s.Store(shadowed)
	gw := New(sn, verifier{}, nil, nil, "test").WithAgents(&fakeAgents{}, 0)
	t.Cleanup(gw.Close)
	c := &caller{id: &apikeys.Identity{Scopes: []string{apikeys.ScopeMCP}}}
	gw.servers(c)
	if n := strings.Count(buf.String(), "shadows it"); n != 1 {
		t.Fatalf("%d warnings for one snapshot: %s", n, buf.String())
	}
	sn.s.Store(&registry.Snapshot{MCPServers: map[string]*registry.MCPServer{}})
	gw.servers(c)
	sn.s.Store(shadowed)
	gw.servers(c)
	if n := strings.Count(buf.String(), "shadows it"); n != 2 {
		t.Fatalf("%d warnings after the upstream came back: %s", n, buf.String())
	}
}
