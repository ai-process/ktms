package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/proxydb"
)

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var (
	str     = map[string]any{"type": "string"}
	strList = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	limit   = map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "Most tasks to return, default 20."}
	since   = map[string]any{"type": "string", "format": "date-time", "description": "Only tasks changed after this time: pass the newest updated_at you have seen."}
	wait    = map[string]any{"type": "integer", "minimum": 0, "maximum": 50, "description": "Seconds to wait for a task when there is none yet; the answer comes as soon as one changes."}
)

func describe(s map[string]any, d string) map[string]any {
	out := map[string]any{"description": d}
	for k, v := range s {
		out[k] = v
	}
	return out
}

// Tools are the tools of the agents server, named without the server prefix.
func Tools() []*mcp.Tool {
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true}
	return []*mcp.Tool{
		{
			Name:        "list_agents",
			Description: "Agents of other people you can send tasks to, with their owner, skills and repositories.",
			InputSchema: obj(map[string]any{}),
			Annotations: readOnly,
		},
		{
			Name:        "register_agent",
			Description: "Describe your agent to the others: what it does, its skills and repositories, and who may send it tasks. Your name is your key's.",
			InputSchema: obj(map[string]any{
				"description": describe(str, "What you can help with."),
				"skills":      describe(strList, "Short skill names, such as code-review or deploy."),
				"repos":       describe(strList, "Repositories you work in."),
				"accept_from": describe(strList, `Agents ("name"), people ("owner:EMAIL") and teams ("team:ID") that may send you tasks; empty means anyone who can see you.`),
			}),
		},
		{
			Name:        "send_message",
			Description: "Send a task to another agent. It lands in their inbox; check its answer later with get_task or outbox.",
			InputSchema: obj(map[string]any{
				"agent":      describe(str, "Name of the agent, from list_agents."),
				"text":       describe(str, "The request, self-contained: the other agent does not see your conversation."),
				"context_id": describe(str, "To continue a conversation: the context_id of one of its tasks."),
			}, "agent", "text"),
		},
		{
			Name:        "inbox",
			Description: "Tasks other agents sent you that are still open, with their messages. Answer each with reply. With since and wait it waits for the next change instead of returning nothing.",
			InputSchema: obj(map[string]any{
				"all":   map[string]any{"type": "boolean", "description": "Include tasks that have ended."},
				"limit": limit,
				"since": since,
				"wait":  wait,
			}),
			Annotations: readOnly,
		},
		{
			Name:        "reply",
			Description: "Answer a task. As its recipient set status completed (default), working, input-required (to ask a question) or failed; as its sender answer a question (working) or cancel it (canceled).",
			InputSchema: obj(map[string]any{
				"task_id":     str,
				"text":        describe(str, "The answer or the question."),
				"status":      map[string]any{"type": "string", "enum": []string{"completed", "working", "input-required", "failed", "canceled"}},
				"needs_owner": map[string]any{"type": "boolean", "description": "With status input-required: the question is for your own person, not the sender; the sender can only wait or cancel."},
			}, "task_id"),
		},
		{
			Name:        "get_task",
			Description: "A task you sent or received, with all its messages and its status.",
			InputSchema: obj(map[string]any{"task_id": str}, "task_id"),
			Annotations: readOnly,
		},
		{
			Name:        "outbox",
			Description: "Tasks you sent, most recently changed first, with their status. With since and wait it waits for the next change.",
			InputSchema: obj(map[string]any{"limit": limit, "since": since, "wait": wait}),
			Annotations: readOnly,
		},
	}
}

// Call runs one tool for the caller. Policy refusals and mistakes come back
// as tool errors the model can act on, as MCP wants.
func (s *Service) Call(ctx context.Context, id *apikeys.Identity, tool string, args json.RawMessage) (*mcp.CallToolResult, error) {
	var in struct {
		Card
		Agent      string `json:"agent"`
		Text       string `json:"text"`
		ContextID  string `json:"context_id"`
		TaskID     string `json:"task_id"`
		Status     string `json:"status"`
		All        bool   `json:"all"`
		Limit      int    `json:"limit"`
		Since      string `json:"since"`
		Wait       int    `json:"wait"`
		NeedsOwner bool   `json:"needs_owner"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return toolError("invalid arguments: " + err.Error()), nil
		}
	}
	if in.Limit <= 0 || in.Limit > 100 {
		in.Limit = 20
	}
	q := Query{All: in.All, Limit: in.Limit, Wait: time.Duration(max(in.Wait, 0)) * time.Second}
	if in.Since != "" {
		t, err := time.Parse(time.RFC3339Nano, in.Since)
		if err != nil {
			return toolError("since: want an RFC 3339 time, such as an updated_at of an earlier answer"), nil
		}
		q.Since = t
	}
	var out any
	var err error
	switch tool {
	case "list_agents":
		var me *proxydb.Agent
		var list []*proxydb.Agent
		if me, list, err = s.List(ctx, id); err == nil {
			out = agentsView(me, list)
		}
	case "register_agent":
		var a *proxydb.Agent
		if a, err = s.Register(ctx, id, in.Card); err == nil {
			out = cardView(a, nil)
		}
	case "send_message":
		var t *proxydb.AgentTask
		if t, err = s.Send(ctx, id, in.Agent, in.Text, in.ContextID); err == nil {
			out = taskView(t)
		}
	case "inbox":
		var ts []*proxydb.AgentTask
		if ts, err = s.Inbox(ctx, id, q); err == nil {
			out = map[string]any{"tasks": tasksView(ts)}
		}
	case "reply":
		var t *proxydb.AgentTask
		if t, err = s.Reply(ctx, id, in.TaskID, in.Text, in.Status, in.NeedsOwner); err == nil {
			out = taskView(t)
		}
	case "get_task":
		var t *proxydb.AgentTask
		if t, err = s.Get(ctx, id, in.TaskID); err == nil {
			out = taskView(t)
		}
	case "outbox":
		var ts []*proxydb.AgentTask
		if ts, err = s.Outbox(ctx, id, q); err == nil {
			out = map[string]any{"tasks": tasksView(ts)}
		}
	default:
		return nil, fmt.Errorf("unknown tool %q", tool)
	}
	switch {
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrDenied):
		return toolError(err.Error()), nil
	case err != nil:
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		log.Error().Err(err).Str("tool", tool).Str("key", id.Name).Msg("agents: tool failed")
		return toolError("agents are unavailable, try again later"), nil
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}, StructuredContent: out}, nil
}

func toolError(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

type agentView struct {
	Name        string   `json:"name"`
	Owner       string   `json:"owner,omitempty"`
	TeamID      string   `json:"team_id,omitempty"`
	Description string   `json:"description,omitempty"`
	Skills      []string `json:"skills,omitempty"`
	Repos       []string `json:"repos,omitempty"`
	AcceptFrom  []string `json:"accept_from,omitempty"`
	You         bool     `json:"you,omitempty"`
	// False when its accept_from leaves you out.
	AcceptsYou *bool `json:"accepts_you,omitempty"`
}

func cardView(a *proxydb.Agent, me *proxydb.Agent) agentView {
	v := agentView{Name: a.Name, Owner: a.Owner, TeamID: a.TeamID, Description: a.Description, Skills: a.Skills, Repos: a.Repos, AcceptFrom: a.AcceptFrom}
	if me != nil {
		if v.You = a.KeyID == me.KeyID; !v.You {
			ok := accepts(a, me)
			v.AcceptsYou = &ok
		}
	}
	return v
}

func agentsView(me *proxydb.Agent, list []*proxydb.Agent) map[string]any {
	out := make([]agentView, 0, len(list))
	for _, a := range list {
		out = append(out, cardView(a, me))
	}
	return map[string]any{"agents": out}
}

type messageView struct {
	From string    `json:"from"`
	Role string    `json:"role"`
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

type taskOut struct {
	TaskID    string `json:"task_id"`
	ContextID string `json:"context_id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
	// question, guardrail_hold or needs_owner while input-required.
	ReasonCode string        `json:"reason_code,omitempty"`
	Depth      int32         `json:"depth,omitempty"`
	Created    time.Time     `json:"created_at"`
	Updated    time.Time     `json:"updated_at"`
	Messages   []messageView `json:"messages,omitempty"`
}

func taskView(t *proxydb.AgentTask) taskOut {
	v := taskOut{TaskID: t.ID, ContextID: t.ContextID, From: t.FromName, To: t.ToName, Status: t.Status, Reason: t.StatusReason, ReasonCode: t.ReasonCode,
		Depth: t.Depth, Created: t.CreatedAt, Updated: t.UpdatedAt}
	for _, m := range t.Messages {
		v.Messages = append(v.Messages, messageView{From: m.FromName, Role: m.Role, Text: m.Text, At: m.CreatedAt})
	}
	return v
}

func tasksView(ts []*proxydb.AgentTask) []taskOut {
	out := make([]taskOut, 0, len(ts))
	for _, t := range ts {
		out = append(out, taskView(t))
	}
	return out
}

// Tools are the service's tools, for the MCP gateway.
func (s *Service) Tools() []*mcp.Tool { return tools }

var tools = Tools()
