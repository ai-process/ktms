// Package agents lets the agents of different people talk through Keystone:
// an agent is a client key with a card, and a task is one request from one
// agent to another, delivered to the recipient's inbox (MCP server "agents").
// The model follows A2A: tasks, context ids, messages and statuses.
package agents

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/ai-process/ktms/internal/apikeys"
	"github.com/ai-process/ktms/internal/observability"
	"github.com/ai-process/ktms/internal/proxydb"
	"github.com/ai-process/ktms/internal/registry"
)

// Server is the name of the built-in MCP server; allowed_mcp ["agents"] makes
// a key that may only talk to agents.
const Server = registry.ReservedMCPServer

// Recording levels of the journal.
const (
	RecordOff      = "off"
	RecordMetadata = "metadata"
	RecordFull     = "full"
)

// ValidRecording reports whether v is a recording level; "" keeps the default.
func ValidRecording(v string) bool {
	return v == "" || v == RecordOff || v == RecordMetadata || v == RecordFull
}

var (
	// ErrInvalid is a request the caller can fix; its text says how.
	ErrInvalid = errors.New("invalid")
	// ErrDenied is a request the policy refuses.
	ErrDenied = errors.New("permission_denied")
)

// Store is the slice of proxydb the service needs.
type Store interface {
	UpsertAgent(ctx context.Context, a *proxydb.Agent) error
	GetAgentByKey(ctx context.Context, keyID string) (*proxydb.Agent, error)
	GetAgentByName(ctx context.Context, name string) (*proxydb.Agent, error)
	GetAPIKeyByName(ctx context.Context, name string) (*proxydb.APIKey, error)
	ListAgents(ctx context.Context, f proxydb.AgentFilter) ([]*proxydb.Agent, error)
	ContextDepth(ctx context.Context, contextID string) (int32, error)
	WithAgentMessages(ctx context.Context, tasks []*proxydb.AgentTask) error
	AgentInContext(ctx context.Context, keyID, contextID string) (bool, error)
	CreateAgentTask(ctx context.Context, t *proxydb.AgentTask, text string, maxMessages int64) error
	AddAgentMessage(ctx context.Context, taskID, fromAgent, role, text, status, reason string, maxMessages int64) (*proxydb.AgentTask, error)
	GetAgentTask(ctx context.Context, id string) (*proxydb.AgentTask, error)
	ListAgentTasks(ctx context.Context, f proxydb.TaskFilter) ([]*proxydb.AgentTask, error)
	DeleteAgentTasksBefore(ctx context.Context, cutoff time.Time) (int64, error)
}

// Config bounds conversations; zero values take the defaults.
type Config struct {
	MaxDepth        int32 // tasks one task may cause in a chain
	MaxMessages     int64 // messages in one conversation (context id)
	MaxMessageBytes int   // one message
	Recording       string
	Retention       time.Duration
}

func (c Config) withDefaults() Config {
	if c.MaxDepth <= 0 {
		c.MaxDepth = 5
	}
	if c.MaxMessages <= 0 {
		c.MaxMessages = 200
	}
	if c.MaxMessageBytes <= 0 {
		c.MaxMessageBytes = 64 << 10
	}
	if c.Recording == "" {
		c.Recording = RecordMetadata
	}
	return c
}

// Service holds the policy; guard and journal may be nil.
type Service struct {
	store   Store
	cfg     Config
	guard   *Guard
	journal Journal
	now     func() time.Time
}

func New(store Store, cfg Config, guard *Guard, journal Journal) *Service {
	return &Service{store: store, cfg: cfg.withDefaults(), guard: guard, journal: journal, now: time.Now}
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// Me is the caller's agent, registered on first use. Its name is the key's
// and cannot change: whoever mints keys names agents, so no agent can take
// another's name to pass an accept_from list.
func (s *Service) Me(ctx context.Context, id *apikeys.Identity) (*proxydb.Agent, error) {
	if id == nil || uuid.Validate(id.KeyID) != nil {
		return nil, fmt.Errorf("%w: only a client key minted in Keystone can be an agent", ErrDenied)
	}
	a, err := s.store.GetAgentByKey(ctx, id.KeyID)
	if !errors.Is(err, proxydb.ErrNotFound) {
		return a, err
	}
	name := strings.ToLower(id.Name)
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("%w: the key name %q is not a valid agent name (lowercase letters, digits, '.', '_' or '-'); ask the operator for a key named so", ErrDenied, id.Name)
	}
	if err := s.store.UpsertAgent(ctx, &proxydb.Agent{KeyID: id.KeyID, Name: name}); err != nil {
		if errors.Is(err, proxydb.ErrAlreadyExists) {
			return nil, fmt.Errorf("%w: another key's agent is named %q; ask the operator for a key with another name", ErrDenied, name)
		}
		return nil, err
	}
	return s.store.GetAgentByKey(ctx, id.KeyID)
}

// Card is what an agent says about itself.
type Card struct {
	Description string   `json:"description,omitempty"`
	Skills      []string `json:"skills,omitempty"`
	Repos       []string `json:"repos,omitempty"`
	AcceptFrom  []string `json:"accept_from,omitempty"`
}

// Register writes the caller's card. Its name and recording level are not the
// agent's to choose.
func (s *Service) Register(ctx context.Context, id *apikeys.Identity, c Card) (*proxydb.Agent, error) {
	me, err := s.Me(ctx, id)
	if err != nil {
		return nil, err
	}
	a := *me
	if len(c.Description) > 2000 {
		return nil, fmt.Errorf("%w: description is longer than 2000 characters", ErrInvalid)
	}
	a.Description = strings.TrimSpace(c.Description)
	a.Skills, a.Repos = clean(c.Skills), clean(c.Repos)
	a.AcceptFrom = clean(c.AcceptFrom)
	if err := s.store.UpsertAgent(ctx, &a); err != nil {
		return nil, err
	}
	return s.store.GetAgentByKey(ctx, id.KeyID)
}

func clean(in []string) []string {
	out := []string{}
	for _, v := range in {
		if v = strings.TrimSpace(v); v != "" && !slices.Contains(out, v) && len(out) < 50 {
			out = append(out, v)
		}
	}
	return out
}

// visible: agents see the agents of their own organization; keys without one
// see each other.
func visible(me, other *proxydb.Agent) bool {
	return !other.Disabled && me.OrgID == other.OrgID
}

// accepts reports whether to takes tasks from from.
func accepts(to, from *proxydb.Agent) bool {
	if len(to.AcceptFrom) == 0 {
		return true
	}
	for _, e := range to.AcceptFrom {
		if e == from.Name || from.TeamID != "" && e == "team:"+from.TeamID || from.Owner != "" && strings.EqualFold(e, "owner:"+from.Owner) {
			return true
		}
	}
	return false
}

// target is the named agent. A key that may use the agents server but has not
// called it yet gets its card now, so a person can be sent tasks as soon as
// their key is minted, before their agent first connects.
func (s *Service) target(ctx context.Context, name string) (*proxydb.Agent, error) {
	a, err := s.store.GetAgentByName(ctx, name)
	if !errors.Is(err, proxydb.ErrNotFound) {
		return a, err
	}
	row, err := s.store.GetAPIKeyByName(ctx, name)
	if err != nil {
		return nil, err
	}
	id := apikeys.IdentityFor(row)
	if !row.Active() || !id.HasScope(apikeys.ScopeMCP) || !id.CanUseTool(Server, "") {
		return nil, proxydb.ErrNotFound
	}
	return s.Me(ctx, id)
}

// List is the agents the caller can see, itself included.
func (s *Service) List(ctx context.Context, id *apikeys.Identity) (me *proxydb.Agent, out []*proxydb.Agent, err error) {
	if me, err = s.Me(ctx, id); err != nil {
		return nil, nil, err
	}
	all, err := s.store.ListAgents(ctx, proxydb.AgentFilter{})
	if err != nil {
		return nil, nil, err
	}
	for _, a := range all {
		if visible(me, a) {
			out = append(out, a)
		}
	}
	return me, out, nil
}

// Send opens a task for agent to. With contextID it continues a conversation
// the caller takes part in, one level deeper.
func (s *Service) Send(ctx context.Context, id *apikeys.Identity, to, text, contextID string) (*proxydb.AgentTask, error) {
	me, err := s.Me(ctx, id)
	if err != nil {
		return nil, err
	}
	target, err := s.target(ctx, strings.ToLower(strings.TrimSpace(to)))
	if errors.Is(err, proxydb.ErrNotFound) || err == nil && !visible(me, target) {
		return nil, fmt.Errorf("%w: no agent %q; list_agents shows the ones you can reach", ErrInvalid, to)
	}
	if err != nil {
		return nil, err
	}
	if target.KeyID == me.KeyID {
		return nil, fmt.Errorf("%w: an agent cannot send a task to itself", ErrInvalid)
	}
	if !accepts(target, me) {
		s.deny("accept_from")
		return nil, fmt.Errorf("%w: agent %s does not accept tasks from %s", ErrDenied, target.Name, me.Name)
	}
	text, reason, err := s.check(ctx, text)
	if err != nil {
		return nil, err
	}

	depth := int32(0)
	if contextID != "" {
		if uuid.Validate(contextID) != nil {
			return nil, fmt.Errorf("%w: context_id must be a task's context_id", ErrInvalid)
		}
		in, err := s.store.AgentInContext(ctx, me.KeyID, contextID)
		if err != nil {
			return nil, err
		}
		if !in {
			return nil, fmt.Errorf("%w: context %s is not one of your conversations", ErrInvalid, contextID)
		}
		deepest, err := s.store.ContextDepth(ctx, contextID)
		if err != nil {
			return nil, err
		}
		if depth = deepest + 1; depth > s.cfg.MaxDepth {
			s.deny("max_depth")
			return nil, fmt.Errorf("%w: this conversation is %d tasks deep, the limit is %d; answer with reply instead", ErrDenied, deepest, s.cfg.MaxDepth)
		}
	} else {
		contextID = uuid.NewString()
	}
	status := proxydb.TaskSubmitted
	if reason != "" {
		status = proxydb.TaskInputRequired
	}
	t := &proxydb.AgentTask{
		ID: uuid.NewString(), ContextID: contextID, FromAgent: me.KeyID, FromName: me.Name,
		ToAgent: target.KeyID, ToName: target.Name, Status: status, StatusReason: reason, Depth: depth,
	}
	if err := s.store.CreateAgentTask(ctx, t, text, s.cfg.MaxMessages); err != nil {
		return nil, s.limitErr(err)
	}
	observability.AgentMessagesTotal.WithLabelValues("sent").Inc()
	s.record(ctx, me, target, t, "task.created", text)
	return t, nil
}

// Inbox is the open tasks sent to the caller, with their messages.
func (s *Service) Inbox(ctx context.Context, id *apikeys.Identity, all bool, limit int) ([]*proxydb.AgentTask, error) {
	me, err := s.Me(ctx, id)
	if err != nil {
		return nil, err
	}
	f := proxydb.TaskFilter{ToAgent: me.KeyID, Limit: limit}
	if !all {
		f.Statuses = []string{proxydb.TaskSubmitted, proxydb.TaskWorking, proxydb.TaskInputRequired}
	}
	tasks, err := s.store.ListAgentTasks(ctx, f)
	if err != nil {
		return nil, err
	}
	return tasks, s.store.WithAgentMessages(ctx, tasks)
}

// Outbox is the tasks the caller sent, without their messages.
func (s *Service) Outbox(ctx context.Context, id *apikeys.Identity, limit int) ([]*proxydb.AgentTask, error) {
	me, err := s.Me(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.store.ListAgentTasks(ctx, proxydb.TaskFilter{FromAgent: me.KeyID, Limit: limit})
}

// Get is a task the caller sent or received.
func (s *Service) Get(ctx context.Context, id *apikeys.Identity, taskID string) (*proxydb.AgentTask, error) {
	me, err := s.Me(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.party(ctx, me, taskID)
}

func (s *Service) party(ctx context.Context, me *proxydb.Agent, taskID string) (*proxydb.AgentTask, error) {
	if uuid.Validate(taskID) != nil {
		return nil, fmt.Errorf("%w: no task %q", ErrInvalid, taskID)
	}
	t, err := s.store.GetAgentTask(ctx, taskID)
	if errors.Is(err, proxydb.ErrNotFound) || err == nil && t.FromAgent != me.KeyID && t.ToAgent != me.KeyID {
		return nil, fmt.Errorf("%w: no task %q", ErrInvalid, taskID)
	}
	return t, err
}

// Reply answers a task. Its recipient moves it to working, input-required,
// completed or failed; its sender answers a question (back to working) or
// cancels it.
func (s *Service) Reply(ctx context.Context, id *apikeys.Identity, taskID, text, status string) (*proxydb.AgentTask, error) {
	me, err := s.Me(ctx, id)
	if err != nil {
		return nil, err
	}
	t, err := s.party(ctx, me, taskID)
	if err != nil {
		return nil, err
	}
	role, allowed := "agent", []string{proxydb.TaskWorking, proxydb.TaskInputRequired, proxydb.TaskCompleted, proxydb.TaskFailed}
	if status == "" {
		status = proxydb.TaskCompleted
	}
	if t.FromAgent == me.KeyID {
		// The sender answers only a question its recipient asked; a task a
		// guardrail holds waits for the recipient's person, so it may only cancel.
		role, allowed = "user", []string{proxydb.TaskCanceled}
		if t.Status == proxydb.TaskInputRequired && !held(t) {
			allowed = append(allowed, proxydb.TaskWorking)
		}
		if status == proxydb.TaskCompleted {
			status = proxydb.TaskWorking
		}
	}
	if !slices.Contains(allowed, status) {
		return nil, fmt.Errorf("%w: status %q: you may set %s", ErrInvalid, status, strings.Join(allowed, ", "))
	}
	if text == "" && status != proxydb.TaskCanceled && status != proxydb.TaskWorking {
		return nil, fmt.Errorf("%w: text is required", ErrInvalid)
	}
	text, reason, err := s.check(ctx, text)
	if err != nil {
		return nil, err
	}
	if reason != "" && status != proxydb.TaskCanceled {
		// Held for a person: the other side sees why before acting on it.
		status = proxydb.TaskInputRequired
	}
	out, err := s.store.AddAgentMessage(ctx, t.ID, me.KeyID, role, text, status, reason, s.cfg.MaxMessages)
	if errors.Is(err, proxydb.ErrTaskClosed) {
		return nil, fmt.Errorf("%w: task %s has already ended", ErrInvalid, t.ID)
	}
	if err != nil {
		return nil, s.limitErr(err)
	}
	observability.AgentMessagesTotal.WithLabelValues("replied").Inc()
	other := t.ToAgent
	if other == me.KeyID {
		other = t.FromAgent
	}
	peer, err := s.store.GetAgentByKey(ctx, other)
	if err != nil {
		peer = &proxydb.Agent{KeyID: other}
	}
	s.record(ctx, me, peer, out, "task."+status, text)
	return out, nil
}

// held reports whether a guardrail holds the task for a person.
func held(t *proxydb.AgentTask) bool { return strings.HasPrefix(t.StatusReason, guardReason) }

func (s *Service) limitErr(err error) error {
	if errors.Is(err, proxydb.ErrContextFull) {
		s.deny("max_messages")
		return fmt.Errorf("%w: this conversation reached its limit of %d messages; start a new task", ErrDenied, s.cfg.MaxMessages)
	}
	return err
}

func (s *Service) deny(reason string) {
	observability.AgentMessagesTotal.WithLabelValues("denied_" + reason).Inc()
}

// check bounds a message and runs the guardrails: the text to store, and why
// a person should look at it first ("" when nobody needs to).
func (s *Service) check(ctx context.Context, text string) (string, string, error) {
	text = strings.TrimSpace(text)
	if len(text) > s.cfg.MaxMessageBytes {
		return "", "", fmt.Errorf("%w: the message is %d bytes, the limit is %d", ErrInvalid, len(text), s.cfg.MaxMessageBytes)
	}
	if s.guard == nil || text == "" {
		return text, "", nil
	}
	out, reason, err := s.guard.Check(ctx, text)
	if err != nil {
		s.deny("guardrail")
		return "", "", fmt.Errorf("%w: %s", ErrDenied, err.Error())
	}
	if reason != "" {
		observability.AgentMessagesTotal.WithLabelValues("held").Inc()
	}
	return out, reason, nil
}

// level is the more detailed of the two agents' recording levels: whoever
// audits either side sees the conversation.
func (s *Service) level(a, b *proxydb.Agent) string {
	rank := map[string]int{RecordOff: 0, RecordMetadata: 1, RecordFull: 2}
	pick := func(v string) string {
		if v == "" {
			return s.cfg.Recording
		}
		return v
	}
	la, lb := pick(a.Recording), pick(b.Recording)
	if rank[lb] > rank[la] {
		return lb
	}
	return la
}

func (s *Service) record(ctx context.Context, from, to *proxydb.Agent, t *proxydb.AgentTask, kind, text string) {
	if s.journal == nil {
		return
	}
	lvl := s.level(from, to)
	if lvl == RecordOff {
		return
	}
	ev := Event{
		Type: kind, At: s.now().UTC(), TaskID: t.ID, ContextID: t.ContextID,
		From: from.Name, FromOwner: from.Owner, To: to.Name, ToOwner: to.Owner,
		Status: t.Status, Reason: t.StatusReason, Depth: t.Depth, Bytes: len(text),
	}
	if lvl == RecordFull {
		ev.Text = text
	}
	// The journal is best effort: a lost event must not fail the conversation.
	if err := s.journal.Record(ctx, ev); err != nil {
		observability.AgentJournalEventsTotal.WithLabelValues("error").Inc()
		log.Warn().Err(err).Str("task_id", t.ID).Msg("agents: journal event lost")
		return
	}
	observability.AgentJournalEventsTotal.WithLabelValues("written").Inc()
}

// Sweep deletes tasks unchanged for longer than the retention, every interval,
// until ctx ends. No retention keeps them forever.
func (s *Service) Sweep(ctx context.Context, every time.Duration) {
	if s.cfg.Retention <= 0 {
		return
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		n, err := s.store.DeleteAgentTasksBefore(ctx, s.now().Add(-s.cfg.Retention))
		if err != nil && ctx.Err() == nil {
			log.Warn().Err(err).Msg("agents: retention sweep failed")
		} else if n > 0 {
			log.Info().Int64("tasks", n).Msg("agents: old tasks deleted")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
