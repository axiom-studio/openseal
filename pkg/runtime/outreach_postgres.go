package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func (s *PostgresStore) migrateOutreach(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("outreach_threads")+` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			project_id TEXT NOT NULL, source_observation_id TEXT NOT NULL,
			identity_profile_ref TEXT NOT NULL, status TEXT NOT NULL, revision BIGINT NOT NULL CHECK(revision>0),
			updated_at TIMESTAMPTZ NOT NULL, idempotency_key_hash TEXT NOT NULL DEFAULT '', payload JSONB NOT NULL,
			PRIMARY KEY(scope_kind,scope_id,id)
		);
		CREATE UNIQUE INDEX IF NOT EXISTS outreach_idempotency_idx ON `+s.table("outreach_threads")+`(scope_kind,scope_id,idempotency_key_hash) WHERE idempotency_key_hash<>'';
		CREATE INDEX IF NOT EXISTS outreach_project_idx ON `+s.table("outreach_threads")+`(scope_kind,scope_id,project_id,status,updated_at DESC);
		CREATE INDEX IF NOT EXISTS outreach_observation_idx ON `+s.table("outreach_threads")+`(scope_kind,scope_id,source_observation_id,updated_at DESC);
		CREATE INDEX IF NOT EXISTS outreach_identity_idx ON `+s.table("outreach_threads")+`(scope_kind,scope_id,identity_profile_ref,updated_at DESC);
	`); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name)VALUES(18,'governed outreach threads')ON CONFLICT(version)DO NOTHING`)
	return err
}

func (s *PostgresStore) CreateOutreachThreadWithEvent(ctx context.Context, thread *OutreachThread, event *ActivityEvent) (*ActivityEvent, error) {
	if err := thread.Validate(); err != nil || event == nil || event.Validate() != nil {
		return nil, ErrInvalidOutreachThread
	}
	payload, err := json.Marshal(thread)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("outreach_threads")+`(scope_kind,scope_id,id,project_id,source_observation_id,identity_profile_ref,status,revision,updated_at,idempotency_key_hash,payload)VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11::jsonb)`,
		thread.Scope.Kind, thread.Scope.ID, thread.ID, thread.ProjectID, thread.SourceObservationID, thread.Identity.ProfileRef,
		thread.Status, thread.Revision, thread.UpdatedAt, thread.IdempotencyKeyHash, string(payload)); err != nil {
		return nil, err
	}
	persisted, err := s.insertPostgresActivityTx(ctx, tx, event)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return persisted, nil
}

func (s *PostgresStore) GetOutreachThread(ctx context.Context, scope Scope, id string) (*OutreachThread, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.getPostgresOutreachThread(ctx, `scope_kind=$1 AND scope_id=$2 AND id=$3`, scope.Kind, scope.ID, strings.TrimSpace(id))
}

func (s *PostgresStore) GetOutreachThreadByIdempotency(ctx context.Context, scope Scope, hash string) (*OutreachThread, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.getPostgresOutreachThread(ctx, `scope_kind=$1 AND scope_id=$2 AND idempotency_key_hash=$3`, scope.Kind, scope.ID, strings.TrimSpace(hash))
}

func (s *PostgresStore) getPostgresOutreachThread(ctx context.Context, predicate string, args ...interface{}) (*OutreachThread, error) {
	var payload []byte
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("outreach_threads")+` WHERE `+predicate, args...).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOutreachThreadNotFound
	}
	if err != nil {
		return nil, err
	}
	var thread OutreachThread
	if err := json.Unmarshal(payload, &thread); err != nil {
		return nil, err
	}
	return &thread, nil
}

func (s *PostgresStore) ListOutreachThreads(ctx context.Context, filter OutreachThreadFilter) ([]*OutreachThread, error) {
	if err := filter.Scope.Validate(); err != nil || filter.Limit > 100 || filter.Offset < 0 {
		return nil, ErrInvalidOutreachThread
	}
	query := `SELECT payload FROM ` + s.table("outreach_threads") + ` WHERE scope_kind=$1 AND scope_id=$2`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	next := 3
	for _, selector := range []struct{ column, value string }{{"project_id", filter.ProjectID}, {"source_observation_id", filter.SourceObservationID}} {
		if strings.TrimSpace(selector.value) != "" {
			query += fmt.Sprintf(` AND %s=$%d`, selector.column, next)
			args = append(args, strings.TrimSpace(selector.value))
			next++
		}
	}
	if len(filter.Statuses) > 0 {
		marks := make([]string, len(filter.Statuses))
		for index, status := range filter.Statuses {
			marks[index] = fmt.Sprintf("$%d", next)
			args = append(args, status)
			next++
		}
		query += ` AND status IN (` + strings.Join(marks, ",") + `)`
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	query += fmt.Sprintf(` ORDER BY updated_at DESC,id ASC LIMIT $%d OFFSET $%d`, next, next+1)
	args = append(args, limit, filter.Offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]*OutreachThread, 0)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var thread OutreachThread
		if err := json.Unmarshal(payload, &thread); err != nil {
			return nil, err
		}
		values = append(values, &thread)
	}
	return values, rows.Err()
}

func (s *PostgresStore) UpdateOutreachThreadWithEvent(ctx context.Context, thread *OutreachThread, expected int64, event *ActivityEvent) (*ActivityEvent, error) {
	if err := thread.Validate(); err != nil || event == nil || event.Validate() != nil || thread.Revision != expected+1 {
		return nil, ErrInvalidOutreachThread
	}
	payload, err := json.Marshal(thread)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("outreach_threads")+` SET status=$1,revision=$2,updated_at=$3,payload=$4::jsonb WHERE scope_kind=$5 AND scope_id=$6 AND id=$7 AND revision=$8`,
		thread.Status, thread.Revision, thread.UpdatedAt, string(payload), thread.Scope.Kind, thread.Scope.ID, thread.ID, expected)
	if err != nil {
		return nil, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, ErrOutreachThreadConflict
	}
	persisted, err := s.insertPostgresActivityTx(ctx, tx, event)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return persisted, nil
}

var _ OutreachStore = (*PostgresStore)(nil)
