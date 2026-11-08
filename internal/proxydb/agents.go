package proxydb

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// TaskTerminal reports whether a task in this status can change no more.
func TaskTerminal(status string) bool {
	return status == TaskCompleted || status == TaskFailed || status == TaskCanceled
}

// ErrTaskClosed is a change to a task that has already ended.
var ErrTaskClosed = errors.New("proxydb: task already ended")

// ErrContextFull is a message beyond the limit of one conversation.
var ErrContextFull = errors.New("proxydb: conversation message limit reached")

// Agent is a client key's card: who it is and who may send it tasks.
type Agent struct {
	KeyID       string
	Name        string
	Description string
	Skills      []string
	Repos       []string
	AcceptFrom  []string
	Recording   string
	// From the key.
	KeyName  string
	Owner    string
	OrgID    string
	TeamID   string
	Disabled bool

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
	Depth        int32
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Messages     []*AgentMessage
}

type AgentMessage struct {
	ID        int64
	TaskID    string
	ContextID string
	FromAgent string
	FromName  string
	Role      string // "user" for the sender of the task, "agent" for its recipient
	Text      string
	CreatedAt time.Time
}

const agentColumns = `a.key_id, a.name, a.description, a.skills, a.repos, a.accept_from, a.recording,
	k.name, k.owner, COALESCE(k.org_id::text, ''), COALESCE(k.team_id::text, ''), k.disabled_at IS NOT NULL,
	a.created_at, a.updated_at`

func scanAgent(row pgx.Row) (*Agent, error) {
	var a Agent
	err := row.Scan(&a.KeyID, &a.Name, &a.Description, &a.Skills, &a.Repos, &a.AcceptFrom, &a.Recording,
		&a.KeyName, &a.Owner, &a.OrgID, &a.TeamID, &a.Disabled, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// UpsertAgent writes the card of a key, all but its recording level, which
// only SetAgentRecording changes; ErrAlreadyExists when another key's agent
// has the name.
func (d *DB) UpsertAgent(ctx context.Context, a *Agent) error {
	_, err := d.pool.Exec(ctx,
		`INSERT INTO agent (key_id, name, description, skills, repos, accept_from)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (key_id) DO UPDATE SET name = $2, description = $3, skills = $4, repos = $5,
		   accept_from = $6, updated_at = NOW()`,
		a.KeyID, a.Name, a.Description, nonNil(a.Skills), nonNil(a.Repos), nonNil(a.AcceptFrom))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrAlreadyExists
	}
	return err
}

// SetAgentRecording sets the journal level of the named agent.
func (d *DB) SetAgentRecording(ctx context.Context, name, recording string) error {
	tag, err := d.pool.Exec(ctx, `UPDATE agent SET recording = $2, updated_at = NOW() WHERE name = $1`, name, recording)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (d *DB) GetAgentByKey(ctx context.Context, keyID string) (*Agent, error) {
	return scanAgent(d.pool.QueryRow(ctx,
		`SELECT `+agentColumns+` FROM agent a JOIN api_key k ON k.id = a.key_id WHERE a.key_id = $1`, keyID))
}

func (d *DB) GetAgentByName(ctx context.Context, name string) (*Agent, error) {
	return scanAgent(d.pool.QueryRow(ctx,
		`SELECT `+agentColumns+` FROM agent a JOIN api_key k ON k.id = a.key_id WHERE a.name = $1`, name))
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
		`SELECT `+agentColumns+` FROM agent a JOIN api_key k ON k.id = a.key_id
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

// CreateAgentTask stores a task with its first message. maxMessages bounds the
// conversation, counted in the same transaction; 0 means no bound.
func (d *DB) CreateAgentTask(ctx context.Context, t *AgentTask, text string, maxMessages int64) error {
	return pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		if err := lockContext(ctx, tx, t.ContextID, maxMessages); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO agent_task (id, context_id, from_agent, to_agent, status, status_reason, reason_code, depth, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, clock_timestamp(), clock_timestamp()) RETURNING created_at, updated_at`,
			t.ID, t.ContextID, t.FromAgent, t.ToAgent, t.Status, t.StatusReason, t.ReasonCode, t.Depth).Scan(&t.CreatedAt, &t.UpdatedAt); err != nil {
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

// lockContext serialises writers of one conversation so the count holds.
func lockContext(ctx context.Context, tx pgx.Tx, contextID string, maxMessages int64) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('agent_context:' || $1))`, contextID); err != nil {
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
func (d *DB) AddAgentMessage(ctx context.Context, taskID, fromAgent, role, text, status, reason, reasonCode string, maxMessages int64) (*AgentTask, error) {
	var t *AgentTask
	err := pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		var contextID, from, to string
		if err := tx.QueryRow(ctx, `SELECT context_id, from_agent, to_agent FROM agent_task WHERE id = $1`, taskID).Scan(&contextID, &from, &to); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		// A status change without text is never refused, so a full
		// conversation can still be closed.
		if text == "" {
			maxMessages = 0
		}
		if err := lockContext(ctx, tx, contextID, maxMessages); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			`UPDATE agent_task SET status = $2, status_reason = $3, reason_code = $4, updated_at = clock_timestamp()
			 WHERE id = $1 AND status NOT IN ('completed', 'failed', 'canceled')`, taskID, status, reason, reasonCode)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrTaskClosed
		}
		if text != "" {
			if _, err := tx.Exec(ctx,
				`INSERT INTO agent_message (task_id, context_id, from_agent, role, text) VALUES ($1, $2, $3, $4, $5)`,
				taskID, contextID, fromAgent, role, text); err != nil {
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
	t.reason_code, t.depth, t.created_at, t.updated_at`

const taskJoins = ` FROM agent_task t JOIN agent fa ON fa.key_id = t.from_agent JOIN agent ta ON ta.key_id = t.to_agent`

func scanTask(row pgx.Row) (*AgentTask, error) {
	var t AgentTask
	err := row.Scan(&t.ID, &t.ContextID, &t.FromAgent, &t.FromName, &t.ToAgent, &t.ToName, &t.Status, &t.StatusReason,
		&t.ReasonCode, &t.Depth, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// GetAgentTask is a task with all its messages, oldest first.
func (d *DB) GetAgentTask(ctx context.Context, id string) (*AgentTask, error) {
	return getAgentTask(ctx, d.pool, id)
}

func getAgentTask(ctx context.Context, q querier, id string) (*AgentTask, error) {
	t, err := scanTask(q.QueryRow(ctx, `SELECT `+taskColumns+taskJoins+` WHERE t.id = $1`, id))
	if err != nil {
		return nil, err
	}
	return t, withMessages(ctx, q, []*AgentTask{t})
}

// WithAgentMessages fills in the messages of the tasks, oldest first.
func (d *DB) WithAgentMessages(ctx context.Context, tasks []*AgentTask) error {
	return withMessages(ctx, d.pool, tasks)
}

func withMessages(ctx context.Context, q querier, tasks []*AgentTask) error {
	if len(tasks) == 0 {
		return nil
	}
	byID := make(map[string]*AgentTask, len(tasks))
	ids := make([]string, 0, len(tasks))
	for _, t := range tasks {
		t.Messages = nil
		byID[t.ID] = t
		ids = append(ids, t.ID)
	}
	rows, err := q.Query(ctx,
		`SELECT m.id, m.task_id, m.context_id, m.from_agent, COALESCE(a.name, ''), m.role, m.text, m.created_at
		 FROM agent_message m LEFT JOIN agent a ON a.key_id = m.from_agent
		 WHERE m.task_id = ANY($1::uuid[]) ORDER BY m.id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var m AgentMessage
		if err := rows.Scan(&m.ID, &m.TaskID, &m.ContextID, &m.FromAgent, &m.FromName, &m.Role, &m.Text, &m.CreatedAt); err != nil {
			return err
		}
		if t := byID[m.TaskID]; t != nil {
			t.Messages = append(t.Messages, &m)
		}
	}
	return rows.Err()
}

// TaskFilter picks tasks for a listing; empty fields do not filter.
type TaskFilter struct {
	ToAgent   string
	FromAgent string
	// Either side: the agent sent or received the task.
	Agent    string
	Statuses []string
	// Only waiting for these reasons (question, guardrail_hold, needs_owner).
	ReasonCodes []string
	// Only tasks changed after this moment; zero for any.
	Since time.Time
	Limit int
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
		   AND ($3::uuid IS NULL OR t.to_agent = $3 OR t.from_agent = $3)
		   AND (cardinality($4::text[]) = 0 OR t.status = ANY($4))
		   AND (cardinality($6::text[]) = 0 OR t.reason_code = ANY($6))
		   AND ($7::timestamptz IS NULL OR t.updated_at > $7)
		 ORDER BY t.updated_at DESC LIMIT $5`,
		nullable(f.ToAgent), nullable(f.FromAgent), nullable(f.Agent), nonNil(f.Statuses), limit, nonNil(f.ReasonCodes), nullableTime(f.Since))
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
// their messages, and reports how many went.
func (d *DB) DeleteAgentTasksBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := d.pool.Exec(ctx, `DELETE FROM agent_task WHERE updated_at < $1`, cutoff)
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
