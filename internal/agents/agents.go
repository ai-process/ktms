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
	"unicode"

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
	GetAPIKeyByLowerName(ctx context.Context, name string) (*proxydb.APIKey, error)
	ListAgents(ctx context.Context, f proxydb.AgentFilter) ([]*proxydb.Agent, error)
	ContextDepth(ctx context.Context, contextID, toAgent string) (int32, error)
	WithAgentMessages(ctx context.Context, tasks []*proxydb.AgentTask) error
	AgentInContext(ctx context.Context, keyID, contextID string) (bool, error)
	CreateAgentTask(ctx context.Context, t *proxydb.AgentTask, text string, maxMessages int64) error
	AddAgentMessage(ctx context.Context, taskID, fromAgent, role, author, text, status, reason, reasonCode string, maxMessages int64) (*proxydb.AgentTask, error)
	AddAgentCorrection(ctx context.Context, taskID, fromAgent, author, text string, window time.Duration, maxMessages int64) (*proxydb.AgentTask, error)
	GetAgentTask(ctx context.Context, id string) (*proxydb.AgentTask, error)
	ListAgentTasks(ctx context.Context, f proxydb.TaskFilter) ([]*proxydb.AgentTask, error)
	DeleteAgentTasksBefore(ctx context.Context, cutoff time.Time) (int64, error)
	CountAgentTasks(ctx context.Context) (map[string]int64, error)
	ListenAgents(ctx context.Context, wake func(keyID string)) error
	SetAgentTaskPinned(ctx context.Context, taskID, byAgent string, pinned bool, maxPinned int64) (*proxydb.AgentTask, error)
	ContextRef(ctx context.Context, contextID string) (string, error)
}

// Config bounds conversations; zero values take the defaults.
type Config struct {
	MaxDepth        int32 // tasks one task may cause in a chain
	MaxMessages     int64 // messages in one conversation (context id)
	MaxMessageBytes int   // one message
	Recording       string
	Retention       time.Duration
	MaxWait         time.Duration // longest wait of inbox and outbox; keep it under the HTTP timeout
	// How long after its last change an ended task takes a correction.
	CorrectionWindow time.Duration
	MaxPinned        int64 // tasks one agent may pin
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
	if c.CorrectionWindow <= 0 {
		c.CorrectionWindow = 7 * 24 * time.Hour
	}
	if c.MaxWait <= 0 {
		c.MaxWait = 50 * time.Second
	}
	if c.MaxPinned <= 0 {
		c.MaxPinned = 200
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
	hub     *hub
}

func New(store Store, cfg Config, guard *Guard, journal Journal) *Service {
	return &Service{store: store, cfg: cfg.withDefaults(), guard: guard, journal: journal, now: time.Now, hub: newHub()}
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
	if a.AcceptFrom, err = acceptList(c.AcceptFrom); err != nil {
		return nil, err
	}
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

// acceptList normalises accept_from: agent names, owner:EMAIL and team:ID.
func acceptList(in []string) ([]string, error) {
	out := clean(in)
	for i, e := range out {
		e = strings.ToLower(e)
		switch {
		case strings.HasPrefix(e, "owner:") && strings.Count(e, "@") == 1 && len(e) <= 6+254:
		case strings.HasPrefix(e, "team:") && uuid.Validate(e[5:]) == nil:
		case nameRe.MatchString(e):
		default:
			return nil, fmt.Errorf(`%w: accept_from entry %q: want an agent name, "owner:EMAIL" or "team:ID"`, ErrInvalid, out[i])
		}
		out[i] = e
	}
	return out, nil
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
		if e == from.Name || from.TeamID != "" && e == "team:"+from.TeamID || from.Owner != "" && e == "owner:"+from.Owner {
			return true
		}
	}
	return false
}

// target is the named agent. A key that may use the agents server but has not
// called it yet gets its card now, so a person can be sent tasks as soon as
// their key is minted, before their agent first connects.
// The card is written only after the caller is known to see it, so no one
// registers another organization's keys or learns their names.
func (s *Service) target(ctx context.Context, me *proxydb.Agent, name string) (*proxydb.Agent, error) {
	a, err := s.store.GetAgentByName(ctx, name)
	if err == nil && !visible(me, a) {
		return nil, proxydb.ErrNotFound
	}
	if !errors.Is(err, proxydb.ErrNotFound) {
		return a, err
	}
	row, err := s.store.GetAPIKeyByLowerName(ctx, name)
	if err != nil {
		return nil, err
	}
	id := apikeys.IdentityFor(row)
	if !row.Active() || !id.HasScope(apikeys.ScopeMCP) || !id.CanUseTool(Server, "") || id.OrgID != me.OrgID {
		return nil, proxydb.ErrNotFound
	}
	a, err = s.Me(ctx, id)
	if errors.Is(err, ErrDenied) {
		return nil, proxydb.ErrNotFound // its key cannot be an agent: not the sender's to fix
	}
	return a, err
}

// List is the agents the caller can see, itself included; with query, those
// whose card holds every word of it.
func (s *Service) List(ctx context.Context, id *apikeys.Identity, query string) (me *proxydb.Agent, out []*proxydb.Agent, err error) {
	if me, err = s.Me(ctx, id); err != nil {
		return nil, nil, err
	}
	if len(query) > maxQuery || len(strings.Fields(query)) > maxQueryWords {
		return nil, nil, fmt.Errorf("%w: query takes up to %d words and %d bytes", ErrInvalid, maxQueryWords, maxQuery)
	}
	all, err := s.store.ListAgents(ctx, proxydb.AgentFilter{Org: &me.OrgID})
	if err != nil {
		return nil, nil, err
	}
	words := strings.Fields(strings.ToLower(query))
	for _, a := range all {
		if matches(a, words) {
			out = append(out, a)
		}
	}
	return me, out, nil
}

const maxQuery, maxQueryWords, maxRef = 200, 10, 200

// CheckRef validates an external_ref: printable, up to maxRef bytes.
func CheckRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if len(ref) > maxRef {
		return "", fmt.Errorf("%w: external_ref takes up to %d bytes", ErrInvalid, maxRef)
	}
	for _, r := range ref {
		if !unicode.IsPrint(r) {
			return "", fmt.Errorf("%w: external_ref must be printable, such as PROJ-123 or a URL", ErrInvalid)
		}
	}
	return ref, nil
}

// matches: every word is in the agent's name, owner, description, skills or repos.
func matches(a *proxydb.Agent, words []string) bool {
	if len(words) == 0 {
		return true
	}
	card := strings.ToLower(strings.Join(append([]string{a.Name, a.Owner, a.Description}, append(a.Skills, a.Repos...)...), "\n"))
	for _, w := range words {
		if !strings.Contains(card, w) {
			return false
		}
	}
	return true
}

// Send opens a task for agent to. With contextID it continues a conversation
// the caller takes part in, one level deeper. ref ties it to an issue
// elsewhere; a continuation without one takes the conversation's.
func (s *Service) Send(ctx context.Context, id *apikeys.Identity, to, text, contextID, ref string) (*proxydb.AgentTask, error) {
	me, err := s.Me(ctx, id)
	if err != nil {
		return nil, err
	}
	if ref, err = CheckRef(ref); err != nil {
		return nil, err
	}
	target, err := s.target(ctx, me, strings.ToLower(strings.TrimSpace(to)))
	if errors.Is(err, proxydb.ErrNotFound) {
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
		// One level below the task the caller is working on; a sender's
		// follow-up stays at the top.
		parent, err := s.store.ContextDepth(ctx, contextID, me.KeyID)
		if err != nil {
			return nil, err
		}
		if depth = parent + 1; depth > s.cfg.MaxDepth {
			s.deny("max_depth")
			return nil, fmt.Errorf("%w: this task would be delegated %d times, the limit is %d; answer with reply instead", ErrDenied, depth, s.cfg.MaxDepth)
		}
		if ref == "" {
			if ref, err = s.store.ContextRef(ctx, contextID); err != nil {
				return nil, err
			}
		}
	} else {
		contextID = uuid.NewString()
	}
	status, code := proxydb.TaskSubmitted, ""
	if reason != "" {
		status, code = proxydb.TaskInputRequired, proxydb.ReasonGuardHold
	}
	t := &proxydb.AgentTask{
		ID: uuid.NewString(), ContextID: contextID, FromAgent: me.KeyID, FromName: me.Name,
		ToAgent: target.KeyID, ToName: target.Name, Status: status, StatusReason: reason, ReasonCode: code, ExternalRef: ref, Depth: depth,
	}
	if err := s.store.CreateAgentTask(ctx, t, text, s.cfg.MaxMessages); err != nil {
		return nil, s.limitErr(err)
	}
	observability.AgentMessagesTotal.WithLabelValues("sent").Inc()
	s.record(ctx, me, target, t, "task.created", text)
	return t, nil
}

// Query narrows inbox and outbox: tasks changed after Since, and, with Wait,
// a wait of up to Wait for the first such task when there is none yet.
type Query struct {
	All         bool // inbox: ended tasks too
	Limit       int
	Since       time.Time
	Wait        time.Duration
	ExternalRef string
	Pinned      bool // only tasks pinned by either side
}

func (q Query) filter() proxydb.TaskFilter {
	return proxydb.TaskFilter{Limit: q.Limit, Since: q.Since, ExternalRef: strings.TrimSpace(q.ExternalRef), PinnedOnly: q.Pinned}
}

// Inbox is the open tasks sent to the caller, with their messages.
func (s *Service) Inbox(ctx context.Context, id *apikeys.Identity, q Query) ([]*proxydb.AgentTask, error) {
	me, err := s.Me(ctx, id)
	if err != nil {
		return nil, err
	}
	f := q.filter()
	f.ToAgent = me.KeyID
	if !q.All {
		f.Statuses = []string{proxydb.TaskSubmitted, proxydb.TaskWorking, proxydb.TaskInputRequired}
	}
	tasks, err := s.wait(ctx, me.KeyID, q.Wait, func() ([]*proxydb.AgentTask, error) { return s.store.ListAgentTasks(ctx, f) })
	if err != nil {
		return nil, err
	}
	return tasks, s.store.WithAgentMessages(ctx, tasks)
}

// Outbox is the tasks the caller sent, without their messages.
func (s *Service) Outbox(ctx context.Context, id *apikeys.Identity, q Query) ([]*proxydb.AgentTask, error) {
	me, err := s.Me(ctx, id)
	if err != nil {
		return nil, err
	}
	f := q.filter()
	f.FromAgent = me.KeyID
	return s.wait(ctx, me.KeyID, q.Wait, func() ([]*proxydb.AgentTask, error) { return s.store.ListAgentTasks(ctx, f) })
}

// recheck bounds a wait on a lost notification, as when the listening
// connection is being replaced.
const recheck = 5 * time.Second

// wait runs list, and while it finds nothing and the wait lasts, again each
// time a task of the agent changes.
func (s *Service) wait(ctx context.Context, keyID string, d time.Duration, list func() ([]*proxydb.AgentTask, error)) ([]*proxydb.AgentTask, error) {
	if d <= 0 {
		return list()
	}
	d = min(d, s.cfg.MaxWait)
	// Subscribed before the first read, so a change between them still wakes.
	woke, stop := s.hub.subscribe(keyID)
	defer stop()
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	for {
		tasks, err := list()
		if err != nil || len(tasks) > 0 {
			return tasks, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return tasks, nil
		case <-woke:
		case <-time.After(recheck):
		}
	}
}

// Listen wakes waiting inbox and outbox calls when tasks change, on this
// replica or another, until ctx ends.
func (s *Service) Listen(ctx context.Context) {
	for ctx.Err() == nil {
		err := s.store.ListenAgents(ctx, s.hub.wake)
		if ctx.Err() != nil {
			return
		}
		log.Warn().Err(err).Msg("agents: listening for task changes failed; waits fall back to rechecks")
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
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
func (s *Service) Reply(ctx context.Context, id *apikeys.Identity, taskID, text, status string, needsOwner bool) (*proxydb.AgentTask, error) {
	me, err := s.Me(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.reply(ctx, me, proxydb.AuthorAgent, taskID, text, status, needsOwner)
}

// AsOwner is the agent named name, for its owner to act as: owner must be
// the owner of its key. Anyone else's agent is not found, as a missing one.
func (s *Service) AsOwner(ctx context.Context, name, owner string) (*proxydb.Agent, error) {
	a, err := s.store.GetAgentByName(ctx, strings.ToLower(strings.TrimSpace(name)))
	if errors.Is(err, proxydb.ErrNotFound) || err == nil && (a.Owner == "" || !strings.EqualFold(a.Owner, strings.TrimSpace(owner))) {
		return nil, fmt.Errorf("%w: no agent %q of %s", ErrInvalid, name, owner)
	}
	return a, err
}

// OwnerReply answers a task as the owner of agent me (from AsOwner), on
// whichever side of it the agent is; the message is marked as the owner's.
func (s *Service) OwnerReply(ctx context.Context, me *proxydb.Agent, taskID, text, status string) (*proxydb.AgentTask, error) {
	return s.reply(ctx, me, proxydb.AuthorOwner, taskID, text, status, false)
}

func (s *Service) reply(ctx context.Context, me *proxydb.Agent, author, taskID, text, status string, needsOwner bool) (*proxydb.AgentTask, error) {
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
		if t.Status == proxydb.TaskInputRequired && !held(t) && t.ReasonCode != proxydb.ReasonOwner {
			allowed = append(allowed, proxydb.TaskWorking)
		}
		if status == proxydb.TaskCompleted {
			status = proxydb.TaskWorking
		}
	}
	code := ""
	if status == proxydb.TaskInputRequired {
		code = proxydb.ReasonQuestion
		if needsOwner {
			code = proxydb.ReasonOwner
		}
	}
	if needsOwner && (role != "agent" || status != proxydb.TaskInputRequired) {
		return nil, fmt.Errorf("%w: needs_owner goes with status input-required, from the task's recipient", ErrInvalid)
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
	if reason != "" {
		// A reply keeps its status: the reason tells the other side to ask its
		// person first, and holding it would leave neither side able to go on.
		reason = guardNote + strings.TrimPrefix(reason, guardReason)
	}
	out, err := s.store.AddAgentMessage(ctx, t.ID, me.KeyID, role, author, text, status, reason, code, s.cfg.MaxMessages)
	if errors.Is(err, proxydb.ErrTaskClosed) {
		return nil, fmt.Errorf("%w: task %s has already ended", ErrInvalid, t.ID)
	}
	if err != nil {
		return nil, s.limitErr(err)
	}
	observability.AgentMessagesTotal.WithLabelValues("replied").Inc()
	observability.AgentReplySeconds.WithLabelValues(role, status).Observe(s.now().Sub(t.UpdatedAt).Seconds())
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

// Correct amends the answer of an ended task the caller received, within
// CorrectionWindow of its last change. The task keeps its status; its change
// wakes the sender's waiting outbox.
func (s *Service) Correct(ctx context.Context, id *apikeys.Identity, taskID, text string) (*proxydb.AgentTask, error) {
	me, err := s.Me(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.correct(ctx, me, proxydb.AuthorAgent, taskID, text)
}

// OwnerCorrect is Correct by the owner of agent me (from AsOwner).
func (s *Service) OwnerCorrect(ctx context.Context, me *proxydb.Agent, taskID, text string) (*proxydb.AgentTask, error) {
	return s.correct(ctx, me, proxydb.AuthorOwner, taskID, text)
}

func (s *Service) correct(ctx context.Context, me *proxydb.Agent, author, taskID, text string) (*proxydb.AgentTask, error) {
	t, err := s.party(ctx, me, taskID)
	if err != nil {
		return nil, err
	}
	if t.ToAgent != me.KeyID {
		return nil, fmt.Errorf("%w: only the task's recipient corrects its answer; send a new task instead", ErrInvalid)
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%w: text is required", ErrInvalid)
	}
	text, reason, err := s.check(ctx, text)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		// Like a reply, a flagged correction goes on with the reason in it.
		text = guardNote + strings.TrimPrefix(reason, guardReason) + "\n\n" + text
	}
	out, err := s.store.AddAgentCorrection(ctx, t.ID, me.KeyID, author, text, s.cfg.CorrectionWindow, s.cfg.MaxMessages)
	switch {
	case errors.Is(err, proxydb.ErrTaskOpen):
		return nil, fmt.Errorf("%w: task %s has not ended: answer it with reply", ErrInvalid, t.ID)
	case errors.Is(err, proxydb.ErrCorrectionLate):
		return nil, fmt.Errorf("%w: task %s ended more than %s ago; send a new task instead", ErrInvalid, t.ID, s.cfg.CorrectionWindow)
	case err != nil:
		return nil, s.limitErr(err)
	}
	observability.AgentMessagesTotal.WithLabelValues("corrected").Inc()
	peer, err := s.store.GetAgentByKey(ctx, t.FromAgent)
	if err != nil {
		peer = &proxydb.Agent{KeyID: t.FromAgent}
	}
	s.record(ctx, me, peer, out, "task.corrected", text)
	return out, nil
}

// held reports whether a guardrail holds the task for a person.
func held(t *proxydb.AgentTask) bool {
	return t.ReasonCode == proxydb.ReasonGuardHold || strings.HasPrefix(t.StatusReason, guardReason)
}

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
	if n := len(t.Messages); n > 0 && t.Messages[n-1].Author == proxydb.AuthorOwner {
		ev.Author = proxydb.AuthorOwner
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

// Pin keeps a task the caller sent or received past the retention sweep, or
// with pinned false lets it go; each side pins for itself.
func (s *Service) Pin(ctx context.Context, id *apikeys.Identity, taskID string, pinned bool) (*proxydb.AgentTask, error) {
	me, err := s.Me(ctx, id)
	if err != nil {
		return nil, err
	}
	if uuid.Validate(taskID) != nil {
		return nil, fmt.Errorf("%w: task_id must be a task's id", ErrInvalid)
	}
	t, err := s.store.SetAgentTaskPinned(ctx, taskID, me.KeyID, pinned, s.cfg.MaxPinned)
	switch {
	case errors.Is(err, proxydb.ErrNotFound):
		return nil, fmt.Errorf("%w: no task %s of yours", ErrInvalid, taskID)
	case errors.Is(err, proxydb.ErrTooManyPins):
		s.deny("max_pinned")
		return nil, fmt.Errorf("%w: you have pinned %d tasks, the limit; unpin one first", ErrDenied, s.cfg.MaxPinned)
	case err != nil:
		return nil, err
	}
	other, kind := t.FromAgent, "task.unpinned"
	if other == me.KeyID {
		other = t.ToAgent
	}
	if pinned {
		kind = "task.pinned"
	}
	if oa, err := s.store.GetAgentByKey(ctx, other); err == nil {
		from, to := me, oa
		if t.FromAgent != me.KeyID {
			from, to = oa, me
		}
		s.record(ctx, from, to, t, kind, "")
	}
	return t, nil
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

// Count sets ktms_agent_tasks every interval until ctx ends. Every status is
// set, zero included, so a status that empties does not keep its last value.
func (s *Service) Count(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		s.count(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) count(ctx context.Context) {
	n, err := s.store.CountAgentTasks(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn().Err(err).Msg("agents: counting tasks failed")
		}
		return
	}
	for _, st := range proxydb.TaskStatuses {
		observability.AgentTasks.WithLabelValues(st).Set(float64(n[st]))
	}
}
