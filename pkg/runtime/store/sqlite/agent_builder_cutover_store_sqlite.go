package sqlite

import (
	"context"
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

var _ models.AgentBuilderCutoverStore = (*Store)(nil)

func sqliteAgentBuilderCutoverTablePresent(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}) (bool, error) {
	var present int
	err := query.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, sqlstore.AgentBuilderCutoverTable).Scan(&present)
	return present > 0, err
}

func (s *Store) ListAgentBuilderCutoverDrafts(ctx context.Context) ([]models.AgentBuilderCutoverDraft, error) {
	present, err := sqliteAgentBuilderCutoverTablePresent(ctx, s.db)
	if err != nil || !present {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT scope_kind, scope_id, id, idempotency_key, payload FROM `+
		sqlstore.AgentBuilderCutoverTable+` ORDER BY created_at, scope_kind, scope_id, id`)
	if err != nil {
		return nil, err
	}
	return sqlstore.ScanAgentBuilderCutoverDrafts(rows)
}

func (s *Store) DeleteAgentBuilderCutoverDraft(ctx context.Context, scope models.Scope, id string) error {
	present, err := sqliteAgentBuilderCutoverTablePresent(ctx, s.db)
	if err != nil || !present {
		return err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM `+sqlstore.AgentBuilderCutoverTable+` WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, id)
	return err
}

func (s *Store) DropAgentBuilderCutoverTable(ctx context.Context) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	present, err := sqliteAgentBuilderCutoverTablePresent(ctx, tx)
	if err != nil || !present {
		return false, err
	}
	var remaining int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM `+sqlstore.AgentBuilderCutoverTable+`)`).Scan(&remaining); err != nil {
		return false, err
	}
	if remaining != 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE `+sqlstore.AgentBuilderCutoverTable); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
