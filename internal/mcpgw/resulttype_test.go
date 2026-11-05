package mcpgw

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
