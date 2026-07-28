package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

func migrateOutreach(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS outreach_threads (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			project_id TEXT NOT NULL, source_observation_id TEXT NOT NULL,
			identity_profile_ref TEXT NOT NULL, status TEXT NOT NULL, revision INTEGER NOT NULL,
			updated_at DATETIME NOT NULL, idempotency_key_hash TEXT NOT NULL DEFAULT '', payload TEXT NOT NULL,
			PRIMARY KEY(scope_kind,scope_id,id)
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_outreach_idempotency ON outreach_threads(scope_kind,scope_id,idempotency_key_hash) WHERE idempotency_key_hash<>'';
		CREATE INDEX IF NOT EXISTS idx_outreach_project ON outreach_threads(scope_kind,scope_id,project_id,status,updated_at DESC);
		CREATE INDEX IF NOT EXISTS idx_outreach_observation ON outreach_threads(scope_kind,scope_id,source_observation_id,updated_at DESC);
		CREATE INDEX IF NOT EXISTS idx_outreach_identity ON outreach_threads(scope_kind,scope_id,identity_profile_ref,updated_at DESC);
	`)
	return err
}

func (s *SQLiteStore) CreateOutreachThreadWithEvent(ctx context.Context, thread *OutreachThread, event *ActivityEvent) (*ActivityEvent, error) {
	if err := thread.Validate(); err != nil || event == nil || event.Validate() != nil {
		return nil, ErrInvalidOutreachThread
	}
	payload, err := json.Marshal(thread)
	if err != nil {
		return nil, err
	}
	return s.withImmediateActivity(ctx, event, func(conn *sql.Conn) error {
		_, err := conn.ExecContext(ctx, `INSERT INTO outreach_threads(scope_kind,scope_id,id,project_id,source_observation_id,identity_profile_ref,status,revision,updated_at,idempotency_key_hash,payload)VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			thread.Scope.Kind, thread.Scope.ID, thread.ID, thread.ProjectID, thread.SourceObservationID, thread.Identity.ProfileRef,
			thread.Status, thread.Revision, thread.UpdatedAt, thread.IdempotencyKeyHash, string(payload))
		return err
	})
}

func (s *SQLiteStore) GetOutreachThread(ctx context.Context, scope Scope, id string) (*OutreachThread, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return getSQLiteOutreachThread(ctx, s.db, `scope_kind=? AND scope_id=? AND id=?`, scope.Kind, scope.ID, strings.TrimSpace(id))
}

func (s *SQLiteStore) GetOutreachThreadByIdempotency(ctx context.Context, scope Scope, hash string) (*OutreachThread, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return getSQLiteOutreachThread(ctx, s.db, `scope_kind=? AND scope_id=? AND idempotency_key_hash=?`, scope.Kind, scope.ID, strings.TrimSpace(hash))
}

func getSQLiteOutreachThread(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}, predicate string, args ...interface{}) (*OutreachThread, error) {
	var payload string
	err := query.QueryRowContext(ctx, `SELECT payload FROM outreach_threads WHERE `+predicate, args...).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOutreachThreadNotFound
	}
	if err != nil {
		return nil, err
	}
	var thread OutreachThread
	if err := json.Unmarshal([]byte(payload), &thread); err != nil {
		return nil, err
	}
	return &thread, nil
}

func (s *SQLiteStore) ListOutreachThreads(ctx context.Context, filter OutreachThreadFilter) ([]*OutreachThread, error) {
	if err := filter.Scope.Validate(); err != nil || filter.Limit > 100 || filter.Offset < 0 {
		return nil, ErrInvalidOutreachThread
	}
	query := `SELECT payload FROM outreach_threads WHERE scope_kind=? AND scope_id=?`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	for _, selector := range []struct{ column, value string }{{"project_id", filter.ProjectID}, {"source_observation_id", filter.SourceObservationID}} {
		if strings.TrimSpace(selector.value) != "" {
			query += ` AND ` + selector.column + `=?`
			args = append(args, strings.TrimSpace(selector.value))
		}
	}
	if len(filter.Statuses) > 0 {
		query += ` AND status IN (` + strings.TrimRight(strings.Repeat("?,", len(filter.Statuses)), ",") + `)`
		for _, status := range filter.Statuses {
			args = append(args, status)
		}
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	query += ` ORDER BY updated_at DESC,id ASC LIMIT ? OFFSET ?`
	args = append(args, limit, filter.Offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]*OutreachThread, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var thread OutreachThread
		if err := json.Unmarshal([]byte(payload), &thread); err != nil {
			return nil, err
		}
		values = append(values, &thread)
	}
	return values, rows.Err()
}

func (s *SQLiteStore) UpdateOutreachThreadWithEvent(ctx context.Context, thread *OutreachThread, expected int64, event *ActivityEvent) (*ActivityEvent, error) {
	if err := thread.Validate(); err != nil || event == nil || event.Validate() != nil || thread.Revision != expected+1 {
		return nil, ErrInvalidOutreachThread
	}
	payload, err := json.Marshal(thread)
	if err != nil {
		return nil, err
	}
	return s.withImmediateActivity(ctx, event, func(conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, `UPDATE outreach_threads SET status=?,revision=?,updated_at=?,payload=? WHERE scope_kind=? AND scope_id=? AND id=? AND revision=?`,
			thread.Status, thread.Revision, thread.UpdatedAt, string(payload), thread.Scope.Kind, thread.Scope.ID, thread.ID, expected)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			return ErrOutreachThreadConflict
		}
		return nil
	})
}

var _ OutreachStore = (*SQLiteStore)(nil)
