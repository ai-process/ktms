package agentrun

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Client talks to the MCP server agents with an agent key. It connects on
// first use and again after a failure.
type Client struct {
	Endpoint string // e.g. https://ktms.example.com/mcp/agents
	Key      string
	HTTP     *http.Client
	Version  string

	mu sync.Mutex
	cs *mcp.ClientSession
}

type bearerRT struct {
	key  string
	base http.RoundTripper
}

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.key)
	return b.base.RoundTrip(r)
}

func (c *Client) session(ctx context.Context) (*mcp.ClientSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cs != nil {
		return c.cs, nil
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	base := hc.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	authed := *hc
	authed.Transport = bearerRT{key: c.Key, base: base}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "ktms-agents-run", Version: c.Version}, nil).Connect(ctx,
		&mcp.StreamableClientTransport{Endpoint: c.Endpoint, HTTPClient: &authed, DisableStandaloneSSE: true}, nil)
	if err != nil {
		return nil, err
	}
	c.cs = cs
	return cs, nil
}

func (c *Client) call(ctx context.Context, tool string, args map[string]any, out any) error {
	cs, err := c.session(ctx)
	if err != nil {
		return err
	}
	res, err := c.callTool(ctx, cs, tool, args)
	if err != nil {
		c.mu.Lock()
		if c.cs == cs {
			_ = cs.Close()
			c.cs = nil
		}
		c.mu.Unlock()
		return err
	}
	var text string
	for _, ct := range res.Content {
		if t, ok := ct.(*mcp.TextContent); ok {
			text += t.Text
		}
	}
	if res.IsError {
		return fmt.Errorf("%s: %s", tool, text)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		return fmt.Errorf("%s: unexpected result: %w", tool, err)
	}
	return nil
}

// callTool returns when ctx ends even while a waiting inbox call is in
// flight: the SDK would wait for the server's answer, so the session closes.
func (c *Client) callTool(ctx context.Context, cs *mcp.ClientSession, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	type result struct {
		res *mcp.CallToolResult
		err error
	}
	ch := make(chan result, 1)
	go func() {
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		ch <- result{res, err}
	}()
	select {
	case r := <-ch:
		return r.res, r.err
	case <-ctx.Done():
		_ = cs.Close() // the session is dropped below; its close error says nothing new
		return nil, ctx.Err()
	}
}

func (c *Client) Inbox(ctx context.Context, since time.Time, wait time.Duration) ([]Task, error) {
	var out struct {
		Tasks []Task `json:"tasks"`
	}
	args := map[string]any{"limit": 100}
	if !since.IsZero() {
		args["since"] = since.Format(time.RFC3339Nano)
	}
	if wait > 0 {
		args["wait"] = int(wait.Seconds())
	}
	if err := c.call(ctx, "inbox", args, &out); err != nil {
		return nil, err
	}
	return out.Tasks, nil
}

func (c *Client) Reply(ctx context.Context, taskID, text, status string, needsOwner bool) error {
	args := map[string]any{"task_id": taskID, "text": text, "status": status}
	if needsOwner {
		args["needs_owner"] = true
	}
	return c.call(ctx, "reply", args, nil)
}

// Close ends the session.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cs == nil {
		return nil
	}
	err := c.cs.Close()
	c.cs = nil
	return err
}
