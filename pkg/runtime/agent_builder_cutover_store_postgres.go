package runtime

import (
	"context"
	"database/sql"
	"errors"

	"github.com/lib/pq"
)

var _ AgentBuilderCutoverStore = (*PostgresStore)(nil)

func (s *PostgresStore) agentBuilderCutoverTablePresent(ctx context.Context, query postgresMigrationQuerier) (bool, error) {
	var relation sql.NullString
	if err := query.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, s.schema+"."+agentBuilderCutoverTable).Scan(&relation); err != nil {
		return false, err
	}
	return relation.Valid, nil
}

func (s *PostgresStore) ListAgentBuilderCutoverDrafts(ctx context.Context) ([]AgentBuilderCutoverDraft, error) {
	present, err := s.agentBuilderCutoverTablePresent(ctx, s.db)
	if err != nil || !present {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT scope_kind, scope_id, id, idempotency_key, payload::text FROM `+
		s.table(agentBuilderCutoverTable)+` ORDER BY created_at, scope_kind, scope_id, id`)
	if err != nil {
		if postgresUndefinedTable(err) {
			return nil, nil
		}
		return nil, err
	}
	return scanAgentBuilderCutoverDrafts(rows)
}

func (s *PostgresStore) DeleteAgentBuilderCutoverDraft(ctx context.Context, scope Scope, id string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM `+s.table(agentBuilderCutoverTable)+` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`,
		scope.Kind, scope.ID, id)
	if postgresUndefinedTable(err) {
		return nil
	}
	return err
}

func (s *PostgresStore) DropAgentBuilderCutoverTable(ctx context.Context) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	present, err := s.agentBuilderCutoverTablePresent(ctx, tx)
	if err != nil || !present {
		return false, err
	}
	// The lock serializes concurrent drops: a waiter finds the table gone.
	if _, err := tx.ExecContext(ctx, `LOCK TABLE `+s.table(agentBuilderCutoverTable)+` IN ACCESS EXCLUSIVE MODE`); err != nil {
		if postgresUndefinedTable(err) {
			return false, nil
		}
		return false, err
	}
	var remaining bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+s.table(agentBuilderCutoverTable)+`)`).Scan(&remaining); err != nil {
		return false, err
	}
	if remaining {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE `+s.table(agentBuilderCutoverTable)); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func postgresUndefinedTable(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "42P01"
}
