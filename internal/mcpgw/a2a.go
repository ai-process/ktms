package mcpgw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ai-process/ktms/internal/agents"
	"github.com/ai-process/ktms/internal/apikeys"
)

// The A2A bridge lets agents that speak A2A (JSON-RPC over HTTP) instead of
// MCP talk to the agents of Keystone: /a2a/{agent} is the agent's A2A
// endpoint and /a2a/{agent}/.well-known/agent-card.json its card. Every call
// is one of the agents tools, so allowed_mcp, AGENTS_TOOL_RPM, accounting,
// policies and guardrails apply as they do over MCP.

// A2AVersion is the A2A protocol version the bridge speaks.
const A2AVersion = "0.3.0"

const maxA2ABody = 1 << 20

// A2A and JSON-RPC error codes.
const (
	codeParse           = -32700
	codeInvalidRequest  = -32600
	codeMethodNotFound  = -32601
	codeInvalidParams   = -32602
	codeInternal        = -32603
	codeTaskNotFound    = -32001
	codeNotCancelable   = -32002
	codeUnsupported     = -32004
	codeContentTypeNotS = -32005
)

func (g *Gateway) registerA2A(mux *http.ServeMux) {
	mux.HandleFunc("POST /a2a/{agent}", g.serveA2A)
	mux.HandleFunc("GET /a2a/{agent}/.well-known/agent-card.json", g.serveA2ACard)
	mux.HandleFunc("GET /a2a/{agent}/.well-known/agent.json", g.serveA2ACard) // A2A before 0.3
}

// a2aCaller authenticates like /mcp/agents: the mcp scope and agents in the
// key's allowed_mcp.
func (g *Gateway) a2aCaller(w http.ResponseWriter, r *http.Request) *caller {
	plain, err := apikeys.ParseBearer(r.Header.Get("Authorization"))
	var id *apikeys.Identity
	if err == nil {
		id, err = g.verifier.VerifyScope(r.Context(), plain, apikeys.ScopeMCP)
	}
	switch {
	case errors.Is(err, apikeys.ErrUnauthenticated):
		w.Header().Set("WWW-Authenticate", `Bearer realm="ktms"`)
		http.Error(w, "unauthenticated: send Authorization: Bearer <KTMS key>", http.StatusUnauthorized)
		return nil
	case errors.Is(err, apikeys.ErrForbidden):
		http.Error(w, "permission_denied: the key needs the mcp scope", http.StatusForbidden)
		return nil
	case err != nil:
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return nil
	}
	if !id.CanUseTool(agents.Server, "") {
		http.Error(w, fmt.Sprintf("permission_denied: mcp server agents is not in the %s's allowed_mcp", id.MCPDeniedBy(agents.Server, "")), http.StatusForbidden)
		return nil
	}
	return &caller{id: id, server: agents.Server, user: r.Header.Get(UserHeader)}
}

// tool runs one agents tool for the caller and decodes its JSON result.
func (g *Gateway) a2aTool(ctx context.Context, c *caller, tool string, args map[string]any, out any) *rpcError {
	raw, _ := json.Marshal(args)
	res, err := g.callAgents(ctx, c, tool, &mcp.CallToolParamsRaw{Name: tool, Arguments: raw})
	if err != nil {
		return &rpcError{Code: codeInvalidRequest, Message: err.Error()}
	}
	text := resultText(res)
	if res.IsError {
		code := codeInvalidParams
		switch {
		case strings.HasPrefix(text, "invalid: no task"):
			code = codeTaskNotFound
		case strings.Contains(text, "has already ended"):
			code = codeNotCancelable
		case strings.HasPrefix(text, "agents are unavailable"):
			code = codeInternal
		}
		return &rpcError{Code: code, Message: text}
	}
	if err := json.Unmarshal([]byte(text), out); err != nil {
		return &rpcError{Code: codeInternal, Message: "unexpected result of " + tool}
	}
	return nil
}

func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// A2A objects, the subset the bridge reads and writes.
type a2aPart struct {
	Kind string          `json:"kind"`
	Text string          `json:"text,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

type a2aMessage struct {
	Kind      string            `json:"kind"`
	MessageID string            `json:"messageId"`
	Role      string            `json:"role"`
	Parts     []a2aPart         `json:"parts"`
	TaskID    string            `json:"taskId,omitempty"`
	ContextID string            `json:"contextId,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

type a2aStatus struct {
	State     string      `json:"state"`
	Message   *a2aMessage `json:"message,omitempty"`
	Timestamp string      `json:"timestamp,omitempty"`
}

type a2aTask struct {
	Kind      string            `json:"kind"`
	ID        string            `json:"id"`
	ContextID string            `json:"contextId"`
	Status    a2aStatus         `json:"status"`
	History   []a2aMessage      `json:"history,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// taskJSON is the agents tools' task.
type taskJSON struct {
	TaskID    string    `json:"task_id"`
	ContextID string    `json:"context_id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Status    string    `json:"status"`
	Reason    string    `json:"reason"`
	Code      string    `json:"reason_code"`
	Answered  string    `json:"answered_by"`
	Updated   time.Time `json:"updated_at"`
	Messages  []struct {
		From       string `json:"from"`
		Role       string `json:"role"`
		Author     string `json:"author"`
		Correction bool   `json:"correction"`
		Text       string `json:"text"`
	} `json:"messages"`
}

func (t taskJSON) a2a(historyLength int) a2aTask {
	out := a2aTask{Kind: "task", ID: t.TaskID, ContextID: t.ContextID,
		Status:   a2aStatus{State: t.Status, Timestamp: t.Updated.UTC().Format(time.RFC3339Nano)},
		Metadata: map[string]string{"from": t.From, "to": t.To}}
	if t.Reason != "" {
		out.Metadata["reason"] = t.Reason
	}
	if t.Code != "" {
		out.Metadata["reason_code"] = t.Code
	}
	if t.Answered != "" {
		out.Metadata["answered_by"] = t.Answered
	}
	for i, m := range t.Messages {
		msg := a2aMessage{Kind: "message", MessageID: t.TaskID + "-" + strconv.Itoa(i+1), Role: m.Role,
			Parts: []a2aPart{{Kind: "text", Text: m.Text}}, TaskID: t.TaskID, ContextID: t.ContextID}
		if m.Author == "owner" || m.Correction {
			msg.Metadata = map[string]string{"author": m.Author}
			if m.Correction {
				msg.Metadata["correction"] = "true"
			}
		}
		out.History = append(out.History, msg)
	}
	if n := len(out.History); n > 0 && out.History[n-1].Role == "agent" {
		last := out.History[n-1]
		out.Status.Message = &last
	}
	if historyLength >= 0 && len(out.History) > historyLength {
		out.History = out.History[len(out.History)-historyLength:]
	}
	return out
}

func ended(state string) bool {
	return state == "completed" || state == "failed" || state == "canceled"
}

func (g *Gateway) serveA2A(w http.ResponseWriter, r *http.Request) {
	c := g.a2aCaller(w, r)
	if c == nil {
		return
	}
	agent := strings.ToLower(r.PathValue("agent"))
	var req rpcRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, maxA2ABody+1))
	if err != nil || len(body) > maxA2ABody {
		writeRPC(w, nil, nil, &rpcError{Code: codeInvalidRequest, Message: "the request body is too large or unreadable"})
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, nil, nil, &rpcError{Code: codeParse, Message: "parse error: " + err.Error()})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, req.ID, nil, &rpcError{Code: codeInvalidRequest, Message: `want a JSON-RPC 2.0 request with "method"`})
		return
	}
	res, rerr := g.a2aCall(r.Context(), c, agent, req.Method, req.Params)
	writeRPC(w, req.ID, res, rerr)
}

func (g *Gateway) a2aCall(ctx context.Context, c *caller, agent, method string, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Message *struct {
			Role      string    `json:"role"`
			Parts     []a2aPart `json:"parts"`
			TaskID    string    `json:"taskId"`
			ContextID string    `json:"contextId"`
		} `json:"message"`
		Configuration *struct {
			Blocking      bool `json:"blocking"`
			HistoryLength *int `json:"historyLength"`
		} `json:"configuration"`
		ID            string `json:"id"`
		HistoryLength *int   `json:"historyLength"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: "invalid params: " + err.Error()}
		}
	}
	history := -1
	if p.HistoryLength != nil {
		history = *p.HistoryLength
	}
	var t taskJSON
	switch method {
	case "message/send":
		if p.Message == nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: "params.message is required"}
		}
		text, rerr := partsText(p.Message.Parts)
		if rerr != nil {
			return nil, rerr
		}
		if p.Configuration != nil && p.Configuration.HistoryLength != nil {
			history = *p.Configuration.HistoryLength
		}
		if p.Message.TaskID != "" {
			// A message on a task answers it: the sender answers a question.
			if rerr := g.a2aTool(ctx, c, "reply", map[string]any{"task_id": p.Message.TaskID, "text": text}, &t); rerr != nil {
				return nil, rerr
			}
		} else {
			args := map[string]any{"agent": agent, "text": text}
			if p.Message.ContextID != "" {
				args["context_id"] = p.Message.ContextID
			}
			if rerr := g.a2aTool(ctx, c, "send_message", args, &t); rerr != nil {
				return nil, rerr
			}
		}
		if p.Configuration != nil && p.Configuration.Blocking {
			t = g.a2aWait(ctx, c, t)
		} else {
			t = g.a2aReload(ctx, c, t)
		}
	case "tasks/get":
		if rerr := g.a2aTool(ctx, c, "get_task", map[string]any{"task_id": p.ID}, &t); rerr != nil {
			return nil, rerr
		}
	case "tasks/cancel":
		if rerr := g.a2aTool(ctx, c, "reply", map[string]any{"task_id": p.ID, "status": "canceled"}, &t); rerr != nil {
			return nil, rerr
		}
		t = g.a2aReload(ctx, c, t)
	case "message/stream", "tasks/resubscribe", "tasks/pushNotificationConfig/set", "tasks/pushNotificationConfig/get",
		"tasks/pushNotificationConfig/list", "tasks/pushNotificationConfig/delete", "agent/getAuthenticatedExtendedCard":
		return nil, &rpcError{Code: codeUnsupported, Message: method + " is not supported: send with message/send and poll tasks/get"}
	default:
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + method}
	}
	if t.To != agent && t.From != agent {
		// The task is the caller's, but not with the agent of this endpoint.
		return nil, &rpcError{Code: codeTaskNotFound, Message: "no task " + t.TaskID + " with agent " + agent}
	}
	return t.a2a(history), nil
}

// a2aWait holds a blocking message/send until the task ends or asks a
// question, for at most a2aBlock; the client then polls tasks/get.
var a2aBlock, a2aPoll = 25 * time.Second, time.Second

func (g *Gateway) a2aWait(ctx context.Context, c *caller, t taskJSON) taskJSON {
	t = g.a2aReload(ctx, c, t)
	deadline := time.Now().Add(a2aBlock)
	for !ended(t.Status) && t.Status != "input-required" && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return t
		case <-time.After(a2aPoll):
		}
		t = g.a2aReload(ctx, c, t)
	}
	return t
}

// a2aReload reads the task with its messages, which send_message and reply
// leave out. The read is the bridge's own: it neither counts against the rate
// limit nor is accounted as a call.
func (g *Gateway) a2aReload(ctx context.Context, c *caller, t taskJSON) taskJSON {
	raw, _ := json.Marshal(map[string]any{"task_id": t.TaskID})
	res, err := g.agents.Call(ctx, c.id, "get_task", raw)
	if err != nil || res.IsError {
		return t
	}
	var next taskJSON
	if json.Unmarshal([]byte(resultText(res)), &next) != nil {
		return t
	}
	return next
}

func partsText(parts []a2aPart) (string, *rpcError) {
	var b []string
	for _, p := range parts {
		switch p.Kind {
		case "text":
			b = append(b, p.Text)
		case "data":
			b = append(b, string(p.Data))
		default:
			return "", &rpcError{Code: codeContentTypeNotS, Message: fmt.Sprintf("part kind %q is not supported: send text or data", p.Kind)}
		}
	}
	text := strings.TrimSpace(strings.Join(b, "\n\n"))
	if text == "" {
		return "", &rpcError{Code: codeInvalidParams, Message: "the message has no text"}
	}
	return text, nil
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, rerr *rpcError) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	out := map[string]any{"jsonrpc": "2.0", "id": id}
	if rerr != nil {
		out["error"] = rerr
	} else {
		out["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// serveA2ACard is the agent's A2A card, its URL on Keystone. It needs a key
// like the endpoint: an agent is visible only inside its organization.
func (g *Gateway) serveA2ACard(w http.ResponseWriter, r *http.Request) {
	c := g.a2aCaller(w, r)
	if c == nil {
		return
	}
	agent := strings.ToLower(r.PathValue("agent"))
	var list struct {
		Agents []struct {
			Name        string   `json:"name"`
			Owner       string   `json:"owner"`
			Description string   `json:"description"`
			Skills      []string `json:"skills"`
		} `json:"agents"`
	}
	if rerr := g.a2aTool(r.Context(), c, "list_agents", nil, &list); rerr != nil {
		code := http.StatusBadRequest
		if strings.HasPrefix(rerr.Message, "throttled") {
			code = http.StatusTooManyRequests
		}
		http.Error(w, rerr.Message, code)
		return
	}
	for _, a := range list.Agents {
		if a.Name != agent {
			continue
		}
		desc := a.Description
		if desc == "" {
			desc = "An agent in Keystone"
			if a.Owner != "" {
				desc += " owned by " + a.Owner
			}
		}
		skills := []map[string]any{}
		for _, s := range a.Skills {
			skills = append(skills, map[string]any{"id": s, "name": s, "description": s, "tags": []string{s}})
		}
		if len(skills) == 0 {
			skills = append(skills, map[string]any{"id": "tasks", "name": "Tasks", "description": desc, "tags": []string{"general"}})
		}
		card := map[string]any{
			"protocolVersion":    A2AVersion,
			"name":               a.Name,
			"description":        desc,
			"url":                baseURL(r) + "/a2a/" + a.Name,
			"preferredTransport": "JSONRPC",
			"version":            g.version,
			"capabilities":       map[string]any{"streaming": false, "pushNotifications": false},
			"defaultInputModes":  []string{"text/plain", "application/json"},
			"defaultOutputModes": []string{"text/plain"},
			"skills":             skills,
			"securitySchemes":    map[string]any{"ktms": map[string]any{"type": "http", "scheme": "bearer", "description": "A Keystone key with the mcp scope"}},
			"security":           []map[string][]string{{"ktms": {}}},
		}
		if a.Owner != "" {
			card["provider"] = map[string]string{"organization": a.Owner, "url": baseURL(r)}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(card)
		return
	}
	http.Error(w, "not_found: no agent "+agent, http.StatusNotFound)
}

// baseURL is Keystone's address as the client reached it, through a proxy
// that sets X-Forwarded-Proto and -Host included.
func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p == "https" || p == "http" {
		scheme = p
	}
	host := r.Host
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		host = strings.TrimSpace(strings.Split(h, ",")[0])
	}
	return scheme + "://" + host
}
