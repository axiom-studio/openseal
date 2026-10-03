package runtime

import (
	"context"
	"database/sql"
)

var _ RunDependencyReconciliationStore = (*PostgresStore)(nil)

func (s *PostgresStore) ListWaitingRunDependencyGroups(ctx context.Context, scope Scope, afterID string, limit int) ([]RunDependencyGroupWork, error) {
	if err := validateWaitingDependencyGroupPage(scope, afterID, limit); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT g.id, g.revision, g.source_run_id, r.revision,
		(r.status IN ('completed','failed','canceled') OR EXISTS (
			SELECT 1 FROM `+s.table("run_dependencies")+` d JOIN `+s.table("agent_runs")+` target
			ON target.scope_kind=d.scope_kind AND target.scope_id=d.scope_id AND target.id=d.target_run_id
			WHERE d.scope_kind=g.scope_kind AND d.scope_id=g.scope_id AND d.group_id=g.id
			AND d.kind='run' AND d.state IN ('pending','running') AND target.status IN ('completed','failed','canceled')))
		FROM `+s.table("run_dependency_groups")+` g JOIN `+s.table("agent_runs")+` r
		ON r.scope_kind=g.scope_kind AND r.scope_id=g.scope_id AND r.id=g.source_run_id
		WHERE g.scope_kind=$1 AND g.scope_id=$2 AND g.status='waiting' AND g.id>$3 ORDER BY g.id LIMIT $4`, scope.Kind, scope.ID, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]RunDependencyGroupWork, 0, limit)
	for rows.Next() {
		work := RunDependencyGroupWork{Scope: scope}
		if err := rows.Scan(&work.GroupID, &work.GroupRevision, &work.SourceRunID, &work.SourceRevision, &work.Ready); err != nil {
			return nil, err
		}
		result = append(result, work)
	}
	return result, rows.Err()
}

func (s *PostgresStore) ReconcileRunDependencyGroup(ctx context.Context, record RunDependencyGroupReconciliationRecord) (*RunDependencyResult, error) {
	if err := validateDependencyGroupReconciliationRecord(record); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	group, err := s.getPostgresDependencyGroup(ctx, tx, record.Scope, record.GroupID, "", true)
	if err != nil {
		return nil, err
	}
	if group == nil {
		return nil, ErrDependencyGroupNotFound
	}
	edges, err := s.listPostgresDependencies(ctx, tx, record.Scope, group.ID, true)
	if err != nil {
		return nil, err
	}
	source, err := s.getPostgresAgentRunTx(ctx, tx, record.Scope, group.SourceRunID, true)
	if err != nil {
		return nil, err
	}
	targets := make(map[string]*AgentRun)
	if !isTerminalAgentRunStatus(source.Status) && !isTerminalDependencyGroup(group) {
		rows, err := tx.QueryContext(ctx, `SELECT r.payload FROM `+s.table("run_dependencies")+` d JOIN `+s.table("agent_runs")+` r
			ON r.scope_kind=d.scope_kind AND r.scope_id=d.scope_id AND r.id=d.target_run_id
			WHERE d.scope_kind=$1 AND d.scope_id=$2 AND d.group_id=$3 AND d.kind='run' AND d.state IN ('pending','running')
			AND r.status IN ('completed','failed','canceled') ORDER BY r.id FOR SHARE OF r`, record.Scope.Kind, record.Scope.ID, group.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				rows.Close()
				return nil, err
			}
			target, err := decodeAgentRun(payload)
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
	result, err := applyDependencyGroupReconciliation(group, edges, source, targets, record)
	if err != nil {
		return nil, err
	}
	if !result.Replayed {
		if err := s.persistPostgresDependencyResult(ctx, tx, group, edges, source, result); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *PostgresStore) persistPostgresDependencyResult(ctx context.Context, tx *sql.Tx, group *RunDependencyGroup, edges []*RunDependency, source *AgentRun, result *RunDependencyResult) error {
	previous := make(map[string]int64, len(edges))
	for _, edge := range edges {
		previous[edge.ID] = edge.Revision
	}
	for _, edge := range result.Dependencies {
		if edge.Revision != previous[edge.ID] {
			if err := s.updatePostgresDependencyTx(ctx, tx, edge, previous[edge.ID]); err != nil {
				return err
			}
		}
	}
	if err := s.updatePostgresDependencyGroupTx(ctx, tx, result.Group, group.Revision); err != nil {
		return err
	}
	if result.Source.Revision != source.Revision {
		if err := s.updatePostgresAgentRunTx(ctx, tx, result.Source, source.Revision); err != nil {
			return err
		}
	}
	for index, event := range result.Events {
		stored, err := s.insertPostgresActivityTx(ctx, tx, event)
		if err != nil {
			return err
		}
		result.Events[index] = stored
	}
	return nil
}
