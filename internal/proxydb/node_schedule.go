package proxydb

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// NodeSchedule is the schedule KTMS sends a node that takes its levels from it.
type NodeSchedule struct {
	NodeID        string
	Text          string
	Version       int64
	OverrideLevel *int32
	OverrideUntil *time.Time
	UpdatedAt     time.Time
}

const nodeScheduleCols = `node_id, schedule, version, override_level, override_until, updated_at`

func scanNodeSchedule(row pgx.Row) (*NodeSchedule, error) {
	var s NodeSchedule
	err := row.Scan(&s.NodeID, &s.Text, &s.Version, &s.OverrideLevel, &s.OverrideUntil, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

func (d *DB) GetNodeSchedule(ctx context.Context, nodeID string) (*NodeSchedule, error) {
	return scanNodeSchedule(d.pool.QueryRow(ctx, `SELECT `+nodeScheduleCols+` FROM node_schedule WHERE node_id = $1`, nodeID))
}

func (d *DB) ListNodeSchedules(ctx context.Context) ([]*NodeSchedule, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+nodeScheduleCols+` FROM node_schedule ORDER BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*NodeSchedule
	for rows.Next() {
		s, err := scanNodeSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// SetNodeScheduleText stores the rules and bumps the version, keeping any override.
func (d *DB) SetNodeScheduleText(ctx context.Context, nodeID, text string) (*NodeSchedule, error) {
	return scanNodeSchedule(d.pool.QueryRow(ctx,
		`INSERT INTO node_schedule (node_id, schedule) VALUES ($1, $2)
		 ON CONFLICT (node_id) DO UPDATE SET schedule = EXCLUDED.schedule,
		     version = node_schedule.version + 1, updated_at = NOW()
		 RETURNING `+nodeScheduleCols, nodeID, text))
}

// SetNodeOverride sets (level and until) or, with both nil, clears the
// override and bumps the version. A node with no row gets one with no rules.
func (d *DB) SetNodeOverride(ctx context.Context, nodeID string, level *int32, until *time.Time) (*NodeSchedule, error) {
	return scanNodeSchedule(d.pool.QueryRow(ctx,
		`INSERT INTO node_schedule (node_id, override_level, override_until) VALUES ($1, $2, $3)
		 ON CONFLICT (node_id) DO UPDATE SET override_level = EXCLUDED.override_level,
		     override_until = EXCLUDED.override_until,
		     version = node_schedule.version + 1, updated_at = NOW()
		 RETURNING `+nodeScheduleCols, nodeID, level, until))
}

func (d *DB) DeleteNodeSchedule(ctx context.Context, nodeID string) error {
	tag, err := d.pool.Exec(ctx, `DELETE FROM node_schedule WHERE node_id = $1`, nodeID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
