package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
)

func migrateActivity(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS run_activity (
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			sequence INTEGER NOT NULL,
			id TEXT NOT NULL,
			event_type TEXT NOT NULL,
			created_at DATETIME NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, run_id, sequence),
			UNIQUE (scope_kind, scope_id, id)
		);
		CREATE INDEX IF NOT EXISTS idx_run_activity_stream
			ON run_activity(scope_kind, scope_id, run_id, sequence);
	`)
	return err
}

func (s *SQLiteStore) UpdateAgentRunWithEvent(ctx context.Context, run *AgentRun, expectedRevision int64, event *ActivityEvent, lease *AgentRunLeaseGuard) (*ActivityEvent, error) {
	if err := run.Validate(); err != nil {
		return nil, err
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	if run.Scope != event.Scope || run.ID != event.RunID {
		return nil, ErrInvalidScope
	}
	if run.Revision != expectedRevision+1 {
		return nil, ErrRevisionConflict
	}
	runPayload, err := json.Marshal(run)
	if err != nil {
		return nil, err
	}
	return s.withImmediateActivity(ctx, event, func(conn *sql.Conn) error {
		query := `UPDATE agent_runs SET status = ?, priority = ?, assigned_agent_id = ?, revision = ?,
			deadline = ?, available_at = ?, queue_entered_at = ?, lease_owner = ?, lease_expires_at = ?, last_claimed_at = ?, attempt = ?, payload = ?
			WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`
		args := []interface{}{
			run.Status, run.Priority, run.AssignedAgentID, run.Revision, run.Deadline, run.AvailableAt,
			run.QueueEnteredAt, run.LeaseOwner, run.LeaseExpiresAt, run.LastClaimedAt, run.Attempt, string(runPayload),
			run.Scope.Kind, run.Scope.ID, run.ID, expectedRevision,
		}
		if lease != nil {
			query += ` AND lease_owner = ? AND lease_expires_at > ?`
			args = append(args, lease.WorkerID, lease.Now)
		}
		result, err := conn.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			if lease != nil {
				return ErrLeaseLost
			}
			return ErrRevisionConflict
		}
		return nil
	})
}

func (s *SQLiteStore) AppendActivity(ctx context.Context, event *ActivityEvent) (*ActivityEvent, error) {
	if err := event.Validate(); err != nil {
		return nil, err
	}
	return s.withImmediateActivity(ctx, event, func(conn *sql.Conn) error {
		var exists int
		err := conn.QueryRowContext(ctx, `SELECT 1 FROM agent_runs WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
			event.Scope.Kind, event.Scope.ID, event.RunID).Scan(&exists)
		if err == sql.ErrNoRows {
			return ErrRunNotFound
		}
		return err
	})
}

func (s *SQLiteStore) withImmediateActivity(ctx context.Context, event *ActivityEvent, beforeInsert func(*sql.Conn) error) (*ActivityEvent, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err := beforeInsert(conn); err != nil {
		return nil, err
	}
	persisted := cloneActivityEvent(event)
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM run_activity
		WHERE scope_kind = ? AND scope_id = ? AND run_id = ?`,
		event.Scope.Kind, event.Scope.ID, event.RunID).Scan(&persisted.Sequence); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(persisted)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO run_activity
		(scope_kind, scope_id, run_id, sequence, id, event_type, created_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, persisted.Scope.Kind, persisted.Scope.ID, persisted.RunID,
		persisted.Sequence, persisted.ID, persisted.EventType, persisted.CreatedAt, string(payload)); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return cloneActivityEvent(persisted), nil
}

func (s *SQLiteStore) ListActivity(ctx context.Context, filter ActivityFilter) ([]*ActivityEvent, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM run_activity
		WHERE scope_kind = ? AND scope_id = ? AND run_id = ? AND sequence > ?
		ORDER BY sequence ASC LIMIT ?`, filter.Scope.Kind, filter.Scope.ID, filter.RunID, filter.AfterSequence, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*ActivityEvent, 0, limit)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var event ActivityEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return nil, err
		}
		result = append(result, &event)
	}
	return result, rows.Err()
}
