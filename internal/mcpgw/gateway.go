package mcpgw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/observability"
	"github.com/ai-process/ktms/internal/registry"
	"github.com/ai-process/ktms/internal/router"
	"github.com/ai-process/ktms/internal/usagereport"
	usagev1 "github.com/ai-process/ktms/internal/usagereport/gen/usage/v1"
)

// Separator joins a server and a tool name in the /mcp aggregate.
const Separator = "__"

// CodeForbidden is the JSON-RPC error of a tool or server the key may not use.
const CodeForbidden = -32003

// UserHeader names the end user a call is made for, as user_id does on /v1.
const UserHeader = "X-KTMS-User"

// Snapshots is the live configuration (registry.Loader).
type Snapshots interface {
	Snapshot() *registry.Snapshot
}

// Verifier authenticates a client key for a scope (apikeys.Verifier).
type Verifier interface {
	VerifyScope(ctx context.Context, plain, scope string) (*apikeys.Identity, error)
}

// ToolLimiter enforces tool_rpm (throttle.Redis).
type ToolLimiter interface {
	AdmitTool(ctx context.Context, keyName, server, tool string, rpm int32) (bool, time.Duration)
}

// Recorder accounts for tool calls (usagereport.UsageCollectorClient).
type Recorder interface {
	RecordToolCall(usagereport.ToolCall) error
}

// Gateway serves /mcp and /mcp/{server}.
type Gateway struct {
	snaps    Snapshots
	verifier Verifier
	limiter  ToolLimiter
	recorder Recorder
	version  string
	pool     pool
	sdk      *mcp.StreamableHTTPHandler
}

// New builds a gateway; limiter and recorder may be nil.
func New(snaps Snapshots, verifier Verifier, limiter ToolLimiter, recorder Recorder, version string) *Gateway {
	g := &Gateway{snaps: snaps, verifier: verifier, limiter: limiter, recorder: recorder, version: version}
	// Stateless: any replica answers any request, and every request carries
	// the key, so a session cannot outlive the key's revocation.
	g.sdk = mcp.NewStreamableHTTPHandler(g.server, &mcp.StreamableHTTPOptions{
		Stateless:                  true,
		JSONResponse:               true,
		DisableLocalhostProtection: true, // a key is required; KTMS often sits behind a local proxy
	})
	return g
}

// Register mounts the gateway on mux.
func (g *Gateway) Register(mux *http.ServeMux) {
	mux.HandleFunc("/mcp", g.serveHTTP)
	mux.HandleFunc("/mcp/{server}", g.serveHTTP)
}

// Close ends every upstream session.
func (g *Gateway) Close() { g.pool.closeAll() }

type ctxKey struct{}

type caller struct {
	id     *apikeys.Identity
	server string // "" on the aggregate
	user   string
}

func (g *Gateway) serveHTTP(w http.ResponseWriter, r *http.Request) {
	plain, err := apikeys.ParseBearer(r.Header.Get("Authorization"))
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="ktms"`)
		http.Error(w, "unauthenticated: send Authorization: Bearer <KTMS key>", http.StatusUnauthorized)
		return
	}
	id, err := g.verifier.VerifyScope(r.Context(), plain, apikeys.ScopeMCP)
	switch {
	case errors.Is(err, apikeys.ErrUnauthenticated):
		w.Header().Set("WWW-Authenticate", `Bearer realm="ktms"`)
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	case errors.Is(err, apikeys.ErrForbidden):
		http.Error(w, "permission_denied: the key needs the mcp scope", http.StatusForbidden)
		return
	case err != nil:
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	c := &caller{id: id, server: r.PathValue("server"), user: r.Header.Get(UserHeader)}
	if c.server != "" {
		if g.snaps.Snapshot().MCPServers[c.server] == nil {
			http.Error(w, "not_found: no mcp server "+c.server, http.StatusNotFound)
			return
		}
		if !id.CanUseTool(c.server, "") {
			observability.MCPToolCallsTotal.WithLabelValues(c.server, "", "denied").Inc()
			http.Error(w, fmt.Sprintf("permission_denied: mcp server %s is not in the %s's allowed_mcp", c.server, id.MCPDeniedBy(c.server, "")), http.StatusForbidden)
			return
		}
	}
	g.sdk.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, c)))
}

// server is the MCP server one request talks to: tools only, the ones its key
// may use, answered from the upstreams.
func (g *Gateway) server(r *http.Request) *mcp.Server {
	c, _ := r.Context().Value(ctxKey{}).(*caller)
	name, instructions := "ktms", "Tools of the MCP servers registered in Keystone (KTMS), named server__tool."
	if c != nil && c.server != "" {
		name, instructions = "ktms-"+c.server, ""
	}
	s := mcp.NewServer(&mcp.Implementation{Name: name, Version: g.version}, &mcp.ServerOptions{
		Instructions: instructions,
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
	})
	s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if c == nil {
				return next(ctx, method, req)
			}
			// No typed nil may reach the SDK as a result alongside an error.
			switch method {
			case "tools/list":
				if res, err := g.listTools(ctx, c); err != nil {
					return nil, err
				} else {
					return res, nil
				}
			case "tools/call":
				p, _ := req.GetParams().(*mcp.CallToolParamsRaw)
				if p == nil {
					return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "missing params"}
				}
				if res, err := g.callTool(ctx, c, p); err != nil {
					return nil, err
				} else {
					return res, nil
				}
			}
			return next(ctx, method, req)
		}
	})
	return s
}

// servers lists the enabled servers the caller may see, in name order.
func (g *Gateway) servers(c *caller) []*registry.MCPServer {
	all := g.snaps.Snapshot().MCPServers
	g.pool.prune(all)
	var out []*registry.MCPServer
	for name, srv := range all {
		if (c.server == "" || c.server == name) && c.id.CanUseTool(name, "") {
			out = append(out, srv)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (g *Gateway) listTools(ctx context.Context, c *caller) (*mcp.ListToolsResult, error) {
	servers := g.servers(c)
	lists := make([][]*mcp.Tool, len(servers))
	var wg sync.WaitGroup
	for i, srv := range servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tools, err := g.pool.get(srv).listTools(ctx, g.version)
			if err != nil {
				// One server down must not hide the others' tools.
				observability.DependencyErrorsTotal.WithLabelValues("mcp", "tools_list").Inc()
				log.Warn().Err(err).Str("mcp_server", srv.Name).Msg("mcp server tools/list failed; its tools are left out")
				return
			}
			lists[i] = tools
		}()
	}
	wg.Wait()
	if c.server != "" && len(servers) == 1 && lists[0] == nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "mcp server " + c.server + " is unavailable"}
	}
	// The list depends on the key: no intermediary may serve it to another.
	res := &mcp.ListToolsResult{Tools: []*mcp.Tool{}, Cacheable: mcp.Cacheable{CacheScope: "private"}}
	for i, srv := range servers {
		for _, t := range lists[i] {
			if !c.id.CanUseTool(srv.Name, t.Name) {
				continue
			}
			out := *t
			if c.server == "" {
				out.Name = srv.Name + Separator + t.Name
			}
			res.Tools = append(res.Tools, &out)
		}
	}
	return res, nil
}

func (g *Gateway) callTool(ctx context.Context, c *caller, p *mcp.CallToolParamsRaw) (*mcp.CallToolResult, error) {
	server, tool := c.server, p.Name
	if server == "" {
		var ok bool
		if server, tool, ok = strings.Cut(p.Name, Separator); !ok {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("unknown tool %q: tools here are named server%stool", p.Name, Separator)}
		}
	}
	srv := g.snaps.Snapshot().MCPServers[server]
	if srv == nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("unknown tool %q: no mcp server %s", p.Name, server)}
	}
	// Only the server's own tool names become metric labels and rate keys.
	if known, err := g.pool.get(srv).listTools(ctx, g.version); err == nil && !slices.ContainsFunc(known, func(t *mcp.Tool) bool { return t.Name == tool }) {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("unknown tool %q", p.Name)}
	}
	if !c.id.CanUseTool(server, tool) {
		observability.MCPToolCallsTotal.WithLabelValues(server, tool, "denied").Inc()
		return nil, &jsonrpc.Error{Code: CodeForbidden, Message: fmt.Sprintf("permission_denied: tool %s of mcp server %s is not in the %s's allowed_mcp", tool, server, c.id.MCPDeniedBy(server, tool))}
	}
	if g.limiter != nil && srv.ToolRPM > 0 {
		if ok, retry := g.limiter.AdmitTool(ctx, c.id.Name, server, tool, srv.ToolRPM); !ok {
			observability.MCPToolCallsTotal.WithLabelValues(server, tool, "throttled").Inc()
			observability.ThrottledTotal.WithLabelValues("mcp_tool_rpm").Inc()
			return toolError(fmt.Sprintf("throttled: %s allows %d calls a minute per key; retry in %ds", p.Name, srv.ToolRPM, int(retry.Seconds()+0.999))), nil
		}
	}

	start := time.Now()
	cctx, cancel := context.WithTimeout(ctx, srv.Timeout())
	defer cancel()
	cctx, cut := withCutMark(cctx)
	res, err := g.pool.get(srv).call(cctx, g.version, &mcp.CallToolParams{Name: tool, Arguments: p.Arguments})
	elapsed := time.Since(start)

	var size int64
	st, class := usagev1.Status_STATUS_SUCCESS, ""
	var rpcErr *jsonrpc.Error
	switch {
	case err == nil:
		b, merr := json.Marshal(res)
		size = int64(len(b))
		switch {
		case merr != nil || size > srv.MaxResponse():
			st, class = usagev1.Status_STATUS_ERROR, "too_large"
			res = toolError(fmt.Sprintf("the result of %s exceeds max_response_bytes (%d)", p.Name, srv.MaxResponse()))
		case res.IsError:
			st, class = usagev1.Status_STATUS_ERROR, "tool_error"
		}
	case errors.As(err, &rpcErr):
		st, class = usagev1.Status_STATUS_ERROR, "tool_error"
	case cut.Load() || errors.Is(err, errTooLarge):
		st, class = usagev1.Status_STATUS_ERROR, "too_large"
		res, err = toolError(fmt.Sprintf("the result of %s exceeds max_response_bytes (%d)", p.Name, srv.MaxResponse())), nil
	case ctx.Err() != nil:
		st, class = usagev1.Status_STATUS_CANCELLED, "cancelled"
	case errors.Is(err, context.DeadlineExceeded) || cctx.Err() != nil:
		st, class = usagev1.Status_STATUS_TIMEOUT, "timeout"
		res, err = toolError(fmt.Sprintf("mcp server %s did not answer within %s", server, srv.Timeout())), nil
	default:
		st, class = usagev1.Status_STATUS_ERROR, "upstream_error"
		log.Warn().Err(err).Str("mcp_server", server).Str("tool", tool).Msg("mcp tool call failed")
		res, err = toolError(fmt.Sprintf("mcp server %s is unavailable", server)), nil
	}
	g.account(c, server, tool, st, class, elapsed, size)
	return res, err
}

func (g *Gateway) account(c *caller, server, tool string, st usagev1.Status, class string, elapsed time.Duration, size int64) {
	status := class
	if status == "" {
		status = "success"
	}
	observability.MCPToolCallsTotal.WithLabelValues(server, tool, status).Inc()
	observability.MCPToolCallDuration.WithLabelValues(server).Observe(elapsed.Seconds())
	if size > 0 {
		observability.MCPResponseBytes.WithLabelValues(server).Observe(float64(size))
	}
	if g.recorder == nil {
		return
	}
	actionID := uuid.Must(uuid.NewV7()).String()
	err := g.recorder.RecordToolCall(usagereport.ToolCall{
		UserID:        router.UserID(map[string]string{"user_id": c.user}, c.id.Name),
		ActionID:      actionID,
		Server:        server,
		Tool:          tool,
		ErrorClass:    class,
		DurationMs:    elapsed.Milliseconds(),
		ResponseBytes: size,
		Status:        st,
		Meta:          map[string]string{"api_key": c.id.Name, "mcp_server": server, "mcp_tool": tool},
	})
	if err != nil {
		log.Warn().Err(err).Str("action_id", actionID).Str("mcp_server", server).Str("tool", tool).Msg("tool call not accounted")
	}
}

// toolError is a failure the model sees and may act on, as MCP wants for
// errors of the tool rather than of the protocol.
func toolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

// ListTools is the server's tools for an operator (Admin API ListMCPTools).
func (g *Gateway) ListTools(ctx context.Context, server string) ([]*mcp.Tool, error) {
	srv := g.snaps.Snapshot().MCPServers[server]
	if srv == nil {
		return nil, fmt.Errorf("mcp server %s is not registered or not enabled", server)
	}
	return g.pool.get(srv).listTools(ctx, g.version)
}
