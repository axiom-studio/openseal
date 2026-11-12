package sqlite

import (
	"context"
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

var _ models.RunDependencyReconciliationStore = (*SQLiteStore)(nil)

func (s *SQLiteStore) ListWaitingRunDependencyGroups(ctx context.Context, scope models.Scope, afterID string, limit int) ([]models.RunDependencyGroupWork, error) {
	if err := models.ValidateWaitingDependencyGroupPage(scope, afterID, limit); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT g.id, g.revision, g.source_run_id, r.revision,
		(r.status IN ('completed','failed','canceled') OR EXISTS (
			SELECT 1 FROM run_dependencies d JOIN agent_runs target
			ON target.scope_kind=d.scope_kind AND target.scope_id=d.scope_id AND target.id=d.target_run_id
			WHERE d.scope_kind=g.scope_kind AND d.scope_id=g.scope_id AND d.group_id=g.id
			AND d.kind='run' AND d.state IN ('pending','running') AND target.status IN ('completed','failed','canceled')))
		FROM run_dependency_groups g JOIN agent_runs r ON r.scope_kind=g.scope_kind AND r.scope_id=g.scope_id AND r.id=g.source_run_id
		WHERE g.scope_kind=? AND g.scope_id=? AND g.status='waiting' AND g.id>?
		ORDER BY g.id LIMIT ?`, scope.Kind, scope.ID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]models.RunDependencyGroupWork, 0, limit)
	for rows.Next() {
		work := models.RunDependencyGroupWork{Scope: scope}
		if err := rows.Scan(&work.GroupID, &work.GroupRevision, &work.SourceRunID, &work.SourceRevision, &work.Ready); err != nil {
			return nil, err
		}
		result = append(result, work)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) ReconcileRunDependencyGroup(ctx context.Context, record models.RunDependencyGroupReconciliationRecord) (*models.RunDependencyResult, error) {
	if err := models.ValidateDependencyGroupReconciliationRecord(record); err != nil {
		return nil, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	group, err := getSQLiteDependencyGroup(ctx, conn, record.Scope, record.GroupID, "")
	if err != nil {
		return nil, err
	}
	if group == nil {
		return nil, models.ErrDependencyGroupNotFound
	}
	edges, err := listSQLiteDependencies(ctx, conn, record.Scope, group.ID)
	if err != nil {
		return nil, err
	}
	source, err := getSQLiteAgentRun(ctx, conn, record.Scope, group.SourceRunID)
	if err != nil {
		return nil, err
	}
	targets := make(map[string]*models.AgentRun)
	if !models.IsTerminalAgentRunStatus(source.Status) && !models.IsTerminalDependencyGroup(group) {
		rows, err := conn.QueryContext(ctx, `SELECT r.payload FROM run_dependencies d JOIN agent_runs r
			ON r.scope_kind=d.scope_kind AND r.scope_id=d.scope_id AND r.id=d.target_run_id
			WHERE d.scope_kind=? AND d.scope_id=? AND d.group_id=? AND d.kind='run' AND d.state IN ('pending','running')
			AND r.status IN ('completed','failed','canceled') ORDER BY r.id`, record.Scope.Kind, record.Scope.ID, group.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				rows.Close()
				return nil, err
			}
			target, err := sqlstore.DecodeAgentRun(payload)
			if err != nil {
				rows.Close()
				return nil, err
			}
			targets[target.ID] = target
		}
		rowErr := rows.Err()
		rows.Close()
		if rowErr != nil {
			return nil, rowErr
		}
	}
	result, err := models.ApplyDependencyGroupReconciliation(group, edges, source, targets, record)
	if err != nil {
		return nil, err
	}
	if !result.Replayed {
		if err := persistSQLiteDependencyResult(ctx, conn, group, edges, source, result); err != nil {
			return nil, err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return result, nil
}

func persistSQLiteDependencyResult(ctx context.Context, conn *sql.Conn, group *models.RunDependencyGroup, edges []*models.RunDependency, source *models.AgentRun, result *models.RunDependencyResult) error {
	previous := make(map[string]int64, len(edges))
	for _, edge := range edges {
		previous[edge.ID] = edge.Revision
	}
	for _, edge := range result.Dependencies {
		if edge.Revision != previous[edge.ID] {
			if err := updateSQLiteDependency(ctx, conn, edge, previous[edge.ID]); err != nil {
				return err
			}
		}
	}
	if err := updateSQLiteDependencyGroup(ctx, conn, result.Group, group.Revision); err != nil {
		return err
	}
	if result.Source.Revision != source.Revision {
		if err := updateSQLiteAgentRunConn(ctx, conn, result.Source, source.Revision); err != nil {
			return err
		}
	}
	for index, event := range result.Events {
		stored, err := insertSQLiteActivityConn(ctx, conn, event)
		if err != nil {
			return err
		}
		result.Events[index] = stored
	}
	return nil
}
