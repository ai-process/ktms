package proxydb

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// Task statuses, as A2A names them.
const (
	TaskSubmitted     = "submitted"
	TaskWorking       = "working"
	TaskInputRequired = "input-required"
	TaskCompleted     = "completed"
	TaskFailed        = "failed"
	TaskCanceled      = "canceled"
)

// TaskStatuses are all task statuses, in the order a task goes through them.
var TaskStatuses = []string{TaskSubmitted, TaskWorking, TaskInputRequired, TaskCompleted, TaskFailed, TaskCanceled}

// ValidTaskStatus reports whether status is one of TaskStatuses.
func ValidTaskStatus(status string) bool { return slices.Contains(TaskStatuses, status) }

// Why a task is input-required.
const (
	ReasonQuestion  = "question"       // the recipient asks the sender
	ReasonGuardHold = "guardrail_hold" // a guardrail holds it for the recipient's person
	ReasonOwner     = "needs_owner"    // the recipient's agent asks its own person
)

// ReasonCodes are all reasons a task waits.
var ReasonCodes = []string{ReasonQuestion, ReasonGuardHold, ReasonOwner}

// TaskOpenStatuses are the statuses of a task that has not ended.
var TaskOpenStatuses = []string{TaskSubmitted, TaskWorking, TaskInputRequired}

// TaskTerminal reports whether a task in this status can change no more.
func TaskTerminal(status string) bool { return !slices.Contains(TaskOpenStatuses, status) }

// ErrTaskClosed is a change to a task that has already ended.
var ErrTaskClosed = errors.New("proxydb: task already ended")

// ErrTaskOpen is a correction to a task that has not ended: reply instead.
var ErrTaskOpen = errors.New("proxydb: task has not ended")

// ErrContextFull is a message beyond the limit of one conversation.
var ErrContextFull = errors.New("proxydb: conversation message limit reached")

// ErrPairBusy is a task beyond the hourly limit of tasks one agent sends another.
var ErrPairBusy = errors.New("proxydb: task limit between two agents reached")

// Agent is a client key's card: who it is and who may send it tasks.
type Agent struct {
	KeyID       string
	Name        string
	Description string
	Skills      []string
	Repos       []string
	AcceptFrom  []string
	Recording   string
	// Released: the key was revoked and the name is free for another key.
	Released bool
	// From the key and its team.
	KeyName        string
	Owner          string
	OrgID          string
	TeamID         string
	Disabled       bool
	Scopes         []string
	KeyAllowedMCP  []string
	TeamAllowedMCP []string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// AgentTask is one request from one agent to another and its conversation.
type AgentTask struct {
	ID           string
	ContextID    string
	FromAgent    string
	FromName     string
	ToAgent      string
	ToName       string
	Status       string
	StatusReason string
	ReasonCode   string // question, guardrail_hold or needs_owner while input-required
	// ExternalRef ties the task to an issue elsewhere, such as PROJ-123 or a URL.
	ExternalRef string
	// Each side pins for itself; a task pinned by either outlives the retention sweep.
	PinnedByFrom bool
	PinnedByTo   bool
	Depth        int32
	CreatedAt    time.Time
	UpdatedAt    time.Time
	// LastAuthor wrote the newest message on the recipient's side, read with
	// the task so answered_by holds when its messages are not loaded.
	LastAuthor string
	Messages   []*AgentMessage
	// Omitted counts the messages a bounded load left out.
	Omitted int
}

type AgentMessage struct {
	ID        int64
	TaskID    string
	ContextID string
	FromAgent string
	FromName  string
	Role      string // "user" for the sender of the task, "agent" for its recipient
	Author    string // AuthorAgent, or AuthorOwner when the agent's person wrote it
	// Correction amends an answer after the task ended.
	Correction bool
	Text       string
	// Truncated: Text is the start of a longer message, cut by a bounded load.
	Truncated bool
	CreatedAt time.Time
}

// Who wrote a message on its side of the task.
const (
	AuthorAgent = "agent"
	AuthorOwner = "owner"
)

const agentColumns = `a.key_id, a.name, a.description, a.skills, a.repos, a.accept_from, a.recording, a.released,
	k.name, k.owner, COALESCE(k.org_id::text, ''), COALESCE(k.team_id::text, ''), k.disabled_at IS NOT NULL,
	k.scopes, k.allowed_mcp, COALESCE(t.allowed_mcp, '{}'), a.created_at, a.updated_at`

const agentJoins = ` FROM agent a JOIN api_key k ON k.id = a.key_id LEFT JOIN team t ON t.id = k.team_id`

func scanAgent(row pgx.Row) (*Agent, error) {
	var a Agent
	err := row.Scan(&a.KeyID, &a.Name, &a.Description, &a.Skills, &a.Repos, &a.AcceptFrom, &a.Recording, &a.Released,
		&a.KeyName, &a.Owner, &a.OrgID, &a.TeamID, &a.Disabled, &a.Scopes, &a.KeyAllowedMCP, &a.TeamAllowedMCP,
		&a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// InsertAgent gives a key its agent named name, unless it has one. A name kept
// by a revoked key's agent is taken over; ErrAlreadyExists when a live key's
// agent has it.
func (d *DB) InsertAgent(ctx context.Context, keyID, name string) error {
	if err := pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT a.key_id::text FROM agent a JOIN api_key k ON k.id = a.key_id
			 WHERE a.name = $1 AND NOT a.released AND k.disabled_at IS NOT NULL`, name)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		return releaseAgents(ctx, tx, ids)
	}); err != nil {
		return err
	}
	// DO NOTHING: a concurrent first call must not overwrite a card just written.
	tag, err := d.pool.Exec(ctx, `INSERT INTO agent (key_id, name) VALUES ($1, $2) ON CONFLICT DO NOTHING`, keyID, name)
	if err != nil || tag.RowsAffected() == 1 {
		return err
	}
	var has bool
	if err := d.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent WHERE key_id = $1)`, keyID).Scan(&has); err != nil {
		return err
	}
	if !has {
		return ErrAlreadyExists
	}
	return nil
}

// releaseAgents frees the names of the keys' agents. Entries naming them in
// accept_from become revoked:NAME, which matches no one: a newer key of the
// name must not inherit their trust, and dropping them could empty a list.
func releaseAgents(ctx context.Context, tx pgx.Tx, keyIDs []string) error {
	if len(keyIDs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx,
		`UPDATE agent SET released = TRUE WHERE key_id = ANY($1::uuid[]) AND NOT released RETURNING name`, keyIDs)
	if err != nil {
		return err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(names) == 0 {
		return err
	}
	_, err = tx.Exec(ctx,
		`UPDATE agent SET updated_at = NOW(), accept_from = ARRAY(
		   SELECT CASE WHEN e = ANY($1::text[]) THEN '`+AcceptRevoked+`' || e ELSE e END
		   FROM unnest(accept_from) WITH ORDINALITY AS u(e, i) ORDER BY i)
		 WHERE accept_from && $1::text[]`, names)
	return err
}

// AcceptRevoked marks an accept_from entry whose agent's key was revoked.
const AcceptRevoked = "revoked:"

// AgentKeysByName is the keys whose agents have ever had the name, a revoked
// key's included, so an audit finds the tasks of each.
func (d *DB) AgentKeysByName(ctx context.Context, name string) ([]string, error) {
	rows, err := d.pool.Query(ctx, `SELECT key_id::text FROM agent WHERE name = $1`, name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// AgentCard is a change to an agent's card; a nil field keeps its value.
type AgentCard struct {
	Description *string
	Skills      []string
	Repos       []string
	AcceptFrom  []string
}

// UpdateAgentCard changes the fields of c on the key's card.
func (d *DB) UpdateAgentCard(ctx context.Context, keyID string, c AgentCard) error {
	tag, err := d.pool.Exec(ctx,
		`UPDATE agent SET description = COALESCE($2, description), skills = COALESCE($3::text[], skills),
		   repos = COALESCE($4::text[], repos), accept_from = COALESCE($5::text[], accept_from), updated_at = NOW()
		 WHERE key_id = $1`,
		keyID, c.Description, c.Skills, c.Repos, c.AcceptFrom)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetAgentRecording sets the journal level of the named agent.
func (d *DB) SetAgentRecording(ctx context.Context, name, recording string) error {
	tag, err := d.pool.Exec(ctx,
		`UPDATE agent SET recording = $2, updated_at = NOW()
		 WHERE key_id = (SELECT key_id FROM agent WHERE name = $1 ORDER BY released, created_at DESC LIMIT 1)`, name, recording)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (d *DB) GetAgentByKey(ctx context.Context, keyID string) (*Agent, error) {
	return scanAgent(d.pool.QueryRow(ctx, `SELECT `+agentColumns+agentJoins+` WHERE a.key_id = $1`, keyID))
}

// GetAgentByName is the named agent of a live key, else the latest released one.
func (d *DB) GetAgentByName(ctx context.Context, name string) (*Agent, error) {
	return scanAgent(d.pool.QueryRow(ctx,
		`SELECT `+agentColumns+agentJoins+` WHERE a.name = $1 ORDER BY a.released, a.created_at DESC LIMIT 1`, name))
}

// AgentFilter picks agents for a listing; an empty owner does not filter.
type AgentFilter struct {
	Owner          string
	IncludeRevoked bool
	// Org, when set, keeps the agents of that organization; "" in it means none.
	Org *string
}

// ListAgents lists agents by name, by default only those whose key is active.
func (d *DB) ListAgents(ctx context.Context, f AgentFilter) ([]*Agent, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT `+agentColumns+agentJoins+`
		 WHERE ($1 OR k.disabled_at IS NULL) AND ($2 = '' OR k.owner = $2)
		   AND ($3::text IS NULL OR COALESCE(k.org_id::text, '') = $3) ORDER BY a.name`,
		f.IncludeRevoked, f.Owner, f.Org)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ContextDepth is the depth of the deepest task of a conversation sent to
// the agent, -1 for none.
func (d *DB) ContextDepth(ctx context.Context, contextID, toAgent string) (int32, error) {
	var depth int32
	err := d.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(depth), -1) FROM agent_task WHERE context_id = $1 AND to_agent = $2`, contextID, toAgent).Scan(&depth)
	return depth, err
}

// AgentInContext reports whether the agent sent or received a task of the conversation.
func (d *DB) AgentInContext(ctx context.Context, keyID, contextID string) (bool, error) {
	var ok bool
	err := d.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM agent_task WHERE context_id = $1 AND (from_agent = $2 OR to_agent = $2))`,
		contextID, keyID).Scan(&ok)
	return ok, err
}

// TaskLimits bound new tasks; 0 means no bound.
type TaskLimits struct {
	Messages    int64 // in one conversation
	PairPerHour int64 // tasks one agent sends another within an hour
}

// CreateAgentTask stores a task with its first message, counting against the
// limits in the same transaction.
func (d *DB) CreateAgentTask(ctx context.Context, t *AgentTask, text string, lim TaskLimits) error {
	return pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		if lim.PairPerHour > 0 {
			if err := lockPair(ctx, tx, t.FromAgent, t.ToAgent, lim.PairPerHour); err != nil {
				return err
			}
		}
		if err := lockContext(ctx, tx, t.ContextID, lim.Messages); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO agent_task (id, context_id, from_agent, to_agent, status, status_reason, reason_code, external_ref, depth, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, clock_timestamp(), clock_timestamp()) RETURNING created_at, updated_at`,
			t.ID, t.ContextID, t.FromAgent, t.ToAgent, t.Status, t.StatusReason, t.ReasonCode, t.ExternalRef, t.Depth).Scan(&t.CreatedAt, &t.UpdatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO agent_message (task_id, context_id, from_agent, role, text) VALUES ($1, $2, $3, 'user', $4)`,
			t.ID, t.ContextID, t.FromAgent, text); err != nil {
			return err
		}
		return notifyAgents(ctx, tx, t.FromAgent, t.ToAgent)
	})
}

// lockPair serialises the tasks one agent sends another so the count holds;
// it is taken before lockContext, always in that order.
func lockPair(ctx context.Context, tx pgx.Tx, from, to string, perHour int64) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('agent_pair:' || $1::uuid::text || ':' || $2::uuid::text))`, from, to); err != nil {
		return err
	}
	var n int64
	if err := tx.QueryRow(ctx,
		`SELECT COUNT(*) FROM agent_task WHERE from_agent = $1 AND to_agent = $2 AND created_at > NOW() - INTERVAL '1 hour'`,
		from, to).Scan(&n); err != nil {
		return err
	}
	if n >= perHour {
		return ErrPairBusy
	}
	return nil
}

// lockContext serialises writers of one conversation so the count holds. The
// key is the canonical uuid, so no spelling of it slips past the lock.
func lockContext(ctx context.Context, tx pgx.Tx, contextID string, maxMessages int64) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('agent_context:' || $1::uuid::text))`, contextID); err != nil {
		return err
	}
	if maxMessages <= 0 {
		return nil
	}
	var n int64
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM agent_message WHERE context_id = $1`, contextID).Scan(&n); err != nil {
		return err
	}
	if n >= maxMessages {
		return ErrContextFull
	}
	return nil
}

// AddAgentMessage appends a message to a task that has not ended and moves it
// to status; reason is kept with the status. ErrTaskClosed once it has ended.
// It returns the task without its messages.
func (d *DB) AddAgentMessage(ctx context.Context, taskID, fromAgent, role, author, text, status, reason, reasonCode string, maxMessages int64) (*AgentTask, error) {
	var t *AgentTask
	err := pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		var contextID, from, to string
		if err := tx.QueryRow(ctx, `SELECT context_id, from_agent, to_agent FROM agent_task WHERE id = $1`, taskID).Scan(&contextID, &from, &to); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		// A status change without text, or one that ends the task, is never
		// refused, so a full conversation can still be closed.
		if text == "" || TaskTerminal(status) {
			maxMessages = 0
		}
		if err := lockContext(ctx, tx, contextID, maxMessages); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			`UPDATE agent_task SET status = $2, status_reason = $3, reason_code = $4, updated_at = clock_timestamp()
			 WHERE id = $1 AND status = ANY($5)`, taskID, status, reason, reasonCode, TaskOpenStatuses)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrTaskClosed
		}
		if text != "" {
			if _, err := tx.Exec(ctx,
				`INSERT INTO agent_message (task_id, context_id, from_agent, role, author, text) VALUES ($1, $2, $3, $4, $5, $6)`,
				taskID, contextID, fromAgent, role, author, text); err != nil {
				return err
			}
		}
		if err := notifyAgents(ctx, tx, from, to); err != nil {
			return err
		}
		t, err = getAgentTask(ctx, tx, taskID)
		return err
	})
	return t, err
}

// ErrCorrectionLate is a correction to a task that ended longer ago than the
// window allows.
var ErrCorrectionLate = errors.New("proxydb: the task ended too long ago to correct")

// AddAgentCorrection appends a correction to a task that has ended, within
// window of its last change; the task keeps its status, and its change wakes
// both agents. ErrTaskOpen while it has not ended.
func (d *DB) AddAgentCorrection(ctx context.Context, taskID, fromAgent, author, text string, window time.Duration, maxMessages int64) (*AgentTask, error) {
	var t *AgentTask
	err := pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		var contextID, from, to, status string
		var late bool
		if err := tx.QueryRow(ctx,
			`SELECT context_id, from_agent, to_agent, status, updated_at < clock_timestamp() - make_interval(secs => $2) FROM agent_task WHERE id = $1`,
			taskID, window.Seconds()).Scan(&contextID, &from, &to, &status, &late); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if !TaskTerminal(status) {
			return ErrTaskOpen
		}
		if late {
			return ErrCorrectionLate
		}
		if err := lockContext(ctx, tx, contextID, maxMessages); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO agent_message (task_id, context_id, from_agent, role, author, correction, text) VALUES ($1, $2, $3, 'agent', $4, true, $5)`,
			taskID, contextID, fromAgent, author, text); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_task SET updated_at = clock_timestamp() WHERE id = $1`, taskID); err != nil {
			return err
		}
		if err := notifyAgents(ctx, tx, from, to); err != nil {
			return err
		}
		var err error
		t, err = getAgentTask(ctx, tx, taskID)
		return err
	})
	return t, err
}

// Sides of a task for TaskFilter.Pinned.
const (
	PinnedFrom   = "from"
	PinnedTo     = "to"
	PinnedEither = "either"
)

// ErrTooManyPins is a pin beyond the limit of one agent.
var ErrTooManyPins = errors.New("proxydb: too many pinned tasks")

// SetAgentTaskPinned pins or unpins a task on byAgent's side; ErrNotFound when
// byAgent is neither its sender nor its recipient. maxPinned bounds the tasks
// byAgent has pinned, counted under a lock on that agent; 0 means no bound.
// It leaves updated_at alone, so a pin neither renews the retention of a task
// nor reopens its correction window.
func (d *DB) SetAgentTaskPinned(ctx context.Context, taskID, byAgent string, pinned bool, maxPinned int64) (*AgentTask, error) {
	var t *AgentTask
	err := pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		var from, to string
		var byFrom, byTo bool
		if err := tx.QueryRow(ctx, `SELECT from_agent, to_agent, pinned_by_from, pinned_by_to FROM agent_task WHERE id = $1`, taskID).
			Scan(&from, &to, &byFrom, &byTo); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		col, was := "pinned_by_from", byFrom
		switch byAgent {
		case from:
		case to:
			col, was = "pinned_by_to", byTo
		default:
			return ErrNotFound
		}
		if was != pinned {
			if pinned && maxPinned > 0 {
				if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('agent_pins:' || $1))`, byAgent); err != nil {
					return err
				}
				var n int64
				if err := tx.QueryRow(ctx,
					`SELECT count(*) FROM agent_task WHERE (from_agent = $1 AND pinned_by_from) OR (to_agent = $1 AND pinned_by_to)`, byAgent).Scan(&n); err != nil {
					return err
				}
				if n >= maxPinned {
					return ErrTooManyPins
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE agent_task SET `+col+` = $2 WHERE id = $1`, taskID, pinned); err != nil {
				return err
			}
		}
		var err error
		t, err = getAgentTask(ctx, tx, taskID)
		return err
	})
	return t, err
}

// ContextRef is the external_ref of the first task of a conversation, the one
// that opened it; "" for none.
func (d *DB) ContextRef(ctx context.Context, contextID string) (string, error) {
	var ref string
	err := d.pool.QueryRow(ctx,
		`SELECT external_ref FROM agent_task WHERE context_id = $1 ORDER BY created_at, id LIMIT 1`, contextID).Scan(&ref)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return ref, err
}

// AgentsChannel is the Postgres channel that names, after each commit, the
// agents whose tasks changed; waiting inbox calls of every replica listen.
const AgentsChannel = "ktms_agents"

func notifyAgents(ctx context.Context, tx pgx.Tx, keyIDs ...string) error {
	for _, id := range keyIDs {
		if _, err := tx.Exec(ctx, `SELECT pg_notify($1, $2)`, AgentsChannel, id); err != nil {
			return err
		}
	}
	return nil
}

// ListenAgents calls wake with the key id of each agent whose tasks changed,
// until ctx ends or the connection fails; the caller listens again.
func (d *DB) ListenAgents(ctx context.Context, wake func(keyID string)) error {
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	// A listening connection must not go back to the pool still listening.
	defer conn.Release()
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), "UNLISTEN *"); err != nil {
			conn.Conn().Close(context.WithoutCancel(ctx))
		}
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+AgentsChannel); err != nil {
		return err
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		wake(n.Payload)
	}
}

const taskColumns = `t.id, t.context_id, t.from_agent, fa.name, t.to_agent, ta.name, t.status, t.status_reason,
	t.reason_code, t.external_ref, t.pinned_by_from, t.pinned_by_to, t.depth, t.created_at, t.updated_at,
	COALESCE((SELECT m.author FROM agent_message m WHERE m.task_id = t.id AND m.role = 'agent' ORDER BY m.id DESC LIMIT 1), '')`

const taskJoins = ` FROM agent_task t JOIN agent fa ON fa.key_id = t.from_agent JOIN agent ta ON ta.key_id = t.to_agent`

func scanTask(row pgx.Row) (*AgentTask, error) {
	var t AgentTask
	err := row.Scan(&t.ID, &t.ContextID, &t.FromAgent, &t.FromName, &t.ToAgent, &t.ToName, &t.Status, &t.StatusReason,
		&t.ReasonCode, &t.ExternalRef, &t.PinnedByFrom, &t.PinnedByTo, &t.Depth, &t.CreatedAt, &t.UpdatedAt, &t.LastAuthor)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// GetAgentTask is a task without its messages.
func (d *DB) GetAgentTask(ctx context.Context, id string) (*AgentTask, error) {
	return getAgentTask(ctx, d.pool, id)
}

func getAgentTask(ctx context.Context, q querier, id string) (*AgentTask, error) {
	return scanTask(q.QueryRow(ctx, `SELECT `+taskColumns+taskJoins+` WHERE t.id = $1`, id))
}

// WithAgentMessages fills in the messages of the tasks, oldest first. With a
// window above 0 each task keeps its first message, cut to window bytes, and
// its newest within window bytes; Omitted counts the others.
func (d *DB) WithAgentMessages(ctx context.Context, tasks []*AgentTask, window int) error {
	if len(tasks) == 0 {
		return nil
	}
	// One snapshot: a message added between the two reads must not skew Omitted.
	return pgx.BeginTxFunc(ctx, d.pool, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		return withMessages(ctx, tx, tasks, window)
	})
}

func withMessages(ctx context.Context, tx pgx.Tx, tasks []*AgentTask, window int) error {
	byID := make(map[string]*AgentTask, len(tasks))
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		t.Messages, t.Omitted = nil, 0
		byID[t.ID] = t
		ids = append(ids, t.ID)
	}
	rows, err := tx.Query(ctx,
		`SELECT id, task_id, context_id, from_agent, from_name, role, author, correction,
		   CASE WHEN $2 > 0 AND octet_length(text) > $2 THEN left(text, $2) ELSE text END,
		   $2 > 0 AND octet_length(text) > $2, created_at FROM (
		   SELECT m.id, m.task_id, m.context_id, m.from_agent, COALESCE(a.name, '') AS from_name, m.role, m.author, m.correction,
		     m.text, m.created_at,
		     row_number() OVER (PARTITION BY m.task_id ORDER BY m.id) AS n,
		     SUM(octet_length(m.text)) OVER (PARTITION BY m.task_id ORDER BY m.id DESC) AS tail
		   FROM agent_message m LEFT JOIN agent a ON a.key_id = m.from_agent
		   WHERE m.task_id = ANY($1::uuid[])
		 ) w WHERE $2 <= 0 OR n = 1 OR tail <= $2 ORDER BY id`, ids, window)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var m AgentMessage
		if err := rows.Scan(&m.ID, &m.TaskID, &m.ContextID, &m.FromAgent, &m.FromName, &m.Role, &m.Author, &m.Correction,
			&m.Text, &m.Truncated, &m.CreatedAt); err != nil {
			return err
		}
		// left() counts characters: cut the rest to window bytes.
		if m.Truncated {
			m.Text = cutUTF8(m.Text, window)
		}
		if t := byID[m.TaskID]; t != nil {
			t.Messages = append(t.Messages, &m)
		}
	}
	if err := rows.Err(); err != nil || window <= 0 {
		return err
	}
	rows, err = tx.Query(ctx, `SELECT task_id, COUNT(*) FROM agent_message WHERE task_id = ANY($1::uuid[]) GROUP BY task_id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var total int
		if err := rows.Scan(&id, &total); err != nil {
			return err
		}
		if t := byID[id]; t != nil {
			t.Omitted = total - len(t.Messages)
		}
	}
	return rows.Err()
}

// cutUTF8 shortens s to at most n bytes without splitting a character.
func cutUTF8(s string, n int) string {
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:min(n, len(s))]
}

// TaskFilter picks tasks for a listing; empty fields do not filter.
type TaskFilter struct {
	ToAgent   string
	FromAgent string
	// Either side: one of the agents sent or received the task.
	Agents   []string
	Statuses []string
	// Only waiting for these reasons (question, guardrail_hold, needs_owner).
	ReasonCodes []string
	// Only tasks changed after this moment; zero for any.
	Since       time.Time
	ExternalRef string
	// Only pinned tasks: PinnedFrom (by their sender), PinnedTo (by their
	// recipient) or PinnedEither.
	Pinned string
	Limit  int
}

// ListAgentTasks lists tasks without their messages, most recently changed first.
func (d *DB) ListAgentTasks(ctx context.Context, f TaskFilter) ([]*AgentTask, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 500)
	// Typed uuids, NULL for no filter, so the indexes on the columns apply.
	rows, err := d.pool.Query(ctx,
		`SELECT `+taskColumns+taskJoins+`
		 WHERE ($1::uuid IS NULL OR t.to_agent = $1)
		   AND ($2::uuid IS NULL OR t.from_agent = $2)
		   AND (cardinality($3::uuid[]) = 0 OR t.to_agent = ANY($3) OR t.from_agent = ANY($3))
		   AND (cardinality($4::text[]) = 0 OR t.status = ANY($4))
		   AND (cardinality($6::text[]) = 0 OR t.reason_code = ANY($6))
		   AND ($7::timestamptz IS NULL OR t.updated_at > $7)
		   AND ($8 = '' OR t.external_ref = $8)
		   AND ($9 = '' OR ($9 IN ('from', 'either') AND t.pinned_by_from) OR ($9 IN ('to', 'either') AND t.pinned_by_to))
		 ORDER BY t.updated_at DESC LIMIT $5`,
		nullable(f.ToAgent), nullable(f.FromAgent), nonNil(f.Agents), nonNil(f.Statuses), limit, nonNil(f.ReasonCodes), nullableTime(f.Since),
		f.ExternalRef, f.Pinned)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AgentTask
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CountAgentTasks is the number of tasks in each status; a status with no
// tasks is absent.
func (d *DB) CountAgentTasks(ctx context.Context) (map[string]int64, error) {
	rows, err := d.pool.Query(ctx, `SELECT status, count(*) FROM agent_task GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}

// DeleteAgentTasksBefore removes tasks unchanged since before cutoff, with
// their messages, and reports how many went. Pinned tasks stay.
func (d *DB) DeleteAgentTasksBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := d.pool.Exec(ctx, `DELETE FROM agent_task WHERE updated_at < $1 AND NOT (pinned_by_from OR pinned_by_to)`, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
