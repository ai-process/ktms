package proxydb

import (
	"context"
	"errors"
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

// UpsertAgent writes the card of a key; ErrAlreadyExists when another key's
// agent has the name.
func (d *DB) UpsertAgent(ctx context.Context, a *Agent) error {
	_, err := d.pool.Exec(ctx,
		`INSERT INTO agent (key_id, name, description, skills, repos, accept_from, recording)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (key_id) DO UPDATE SET name = $2, description = $3, skills = $4, repos = $5,
		   accept_from = $6, recording = $7, updated_at = NOW()`,
		a.KeyID, a.Name, a.Description, nonNil(a.Skills), nonNil(a.Repos), nonNil(a.AcceptFrom), a.Recording)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrAlreadyExists
	}
	return err
}

func (d *DB) GetAgentByKey(ctx context.Context, keyID string) (*Agent, error) {
	return scanAgent(d.pool.QueryRow(ctx,
		`SELECT `+agentColumns+` FROM agent a JOIN api_key k ON k.id = a.key_id WHERE a.key_id = $1`, keyID))
}

func (d *DB) GetAgentByName(ctx context.Context, name string) (*Agent, error) {
	return scanAgent(d.pool.QueryRow(ctx,
		`SELECT `+agentColumns+` FROM agent a JOIN api_key k ON k.id = a.key_id WHERE a.name = $1`, name))
}

// ListAgents is every agent whose key is active, by name.
func (d *DB) ListAgents(ctx context.Context) ([]*Agent, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT `+agentColumns+` FROM agent a JOIN api_key k ON k.id = a.key_id
		 WHERE k.disabled_at IS NULL ORDER BY a.name`)
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

// ContextStats is the deepest task and the message count of a conversation.
func (d *DB) ContextStats(ctx context.Context, contextID string) (depth int32, messages int64, err error) {
	err = d.pool.QueryRow(ctx,
		`SELECT COALESCE((SELECT MAX(depth) FROM agent_task WHERE context_id = $1), -1),
		        (SELECT COUNT(*) FROM agent_message WHERE context_id = $1)`, contextID).Scan(&depth, &messages)
	return depth, messages, err
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
			`INSERT INTO agent_task (id, context_id, from_agent, to_agent, status, status_reason, depth)
			 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING created_at, updated_at`,
			t.ID, t.ContextID, t.FromAgent, t.ToAgent, t.Status, t.StatusReason, t.Depth).Scan(&t.CreatedAt, &t.UpdatedAt); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO agent_message (task_id, context_id, from_agent, role, text) VALUES ($1, $2, $3, 'user', $4)`,
			t.ID, t.ContextID, t.FromAgent, text)
		return err
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
func (d *DB) AddAgentMessage(ctx context.Context, taskID, fromAgent, role, text, status, reason string, maxMessages int64) (*AgentTask, error) {
	var t *AgentTask
	err := pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		var contextID string
		if err := tx.QueryRow(ctx, `SELECT context_id FROM agent_task WHERE id = $1`, taskID).Scan(&contextID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if err := lockContext(ctx, tx, contextID, maxMessages); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx,
			`UPDATE agent_task SET status = $2, status_reason = $3, updated_at = NOW()
			 WHERE id = $1 AND status NOT IN ('completed', 'failed', 'canceled')`, taskID, status, reason)
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
		t, err = getAgentTask(ctx, tx, taskID)
		return err
	})
	return t, err
}

const taskColumns = `t.id, t.context_id, t.from_agent, fa.name, t.to_agent, ta.name, t.status, t.status_reason,
	t.depth, t.created_at, t.updated_at`

const taskJoins = ` FROM agent_task t JOIN agent fa ON fa.key_id = t.from_agent JOIN agent ta ON ta.key_id = t.to_agent`

func scanTask(row pgx.Row) (*AgentTask, error) {
	var t AgentTask
	err := row.Scan(&t.ID, &t.ContextID, &t.FromAgent, &t.FromName, &t.ToAgent, &t.ToName, &t.Status, &t.StatusReason,
		&t.Depth, &t.CreatedAt, &t.UpdatedAt)
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
	rows, err := q.Query(ctx,
		`SELECT m.id, m.task_id, m.context_id, m.from_agent, COALESCE(a.name, ''), m.role, m.text, m.created_at
		 FROM agent_message m LEFT JOIN agent a ON a.key_id = m.from_agent
		 WHERE m.task_id = $1 ORDER BY m.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m AgentMessage
		if err := rows.Scan(&m.ID, &m.TaskID, &m.ContextID, &m.FromAgent, &m.FromName, &m.Role, &m.Text, &m.CreatedAt); err != nil {
			return nil, err
		}
		t.Messages = append(t.Messages, &m)
	}
	return t, rows.Err()
}

// TaskFilter picks tasks for a listing; empty fields do not filter.
type TaskFilter struct {
	ToAgent   string
	FromAgent string
	// Either side: the agent sent or received the task.
	Agent    string
	Statuses []string
	Limit    int
}

// ListAgentTasks lists tasks without their messages, most recently changed first.
func (d *DB) ListAgentTasks(ctx context.Context, f TaskFilter) ([]*AgentTask, error) {
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := d.pool.Query(ctx,
		`SELECT `+taskColumns+taskJoins+`
		 WHERE ($1 = '' OR t.to_agent::text = $1)
		   AND ($2 = '' OR t.from_agent::text = $2)
		   AND ($3 = '' OR t.to_agent::text = $3 OR t.from_agent::text = $3)
		   AND (cardinality($4::text[]) = 0 OR t.status = ANY($4))
		 ORDER BY t.updated_at DESC LIMIT $5`,
		f.ToAgent, f.FromAgent, f.Agent, nonNil(f.Statuses), limit)
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

// DeleteAgentTasksBefore removes tasks unchanged since before cutoff, with
// their messages, and reports how many went.
func (d *DB) DeleteAgentTasksBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := d.pool.Exec(ctx, `DELETE FROM agent_task WHERE updated_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
