package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
)

func migratePortfolio(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS objectives (
			id TEXT NOT NULL,
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			owner_type TEXT NOT NULL,
			owner_id TEXT NOT NULL,
			status TEXT NOT NULL,
			priority INTEGER NOT NULL DEFAULT 0,
			revision INTEGER NOT NULL,
			updated_at DATETIME NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id)
		);
		CREATE INDEX IF NOT EXISTS idx_objectives_portfolio
			ON objectives(scope_kind, scope_id, owner_type, owner_id, status, priority, updated_at);

		CREATE TABLE IF NOT EXISTS agent_runs (
			id TEXT NOT NULL,
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			objective_id TEXT NOT NULL DEFAULT '',
			parent_run_id TEXT NOT NULL DEFAULT '',
			root_run_id TEXT NOT NULL,
			assigned_agent_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			priority INTEGER NOT NULL DEFAULT 0,
			revision INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id)
		);
		CREATE INDEX IF NOT EXISTS idx_agent_runs_objective
			ON agent_runs(scope_kind, scope_id, objective_id, status, priority, created_at);
		CREATE INDEX IF NOT EXISTS idx_agent_runs_lineage
			ON agent_runs(scope_kind, scope_id, root_run_id, parent_run_id, created_at);
	`)
	if err != nil {
		return err
	}
	if err := addMissingAgentRunColumns(db); err != nil {
		return err
	}
	return migrateActivity(db)
}

func addMissingAgentRunColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(agent_runs)`)
	if err != nil {
		return err
	}
	foundRevision := false
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return err
		}
		foundRevision = foundRevision || name == "revision"
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if foundRevision {
		return nil
	}
	_, err = db.Exec(`ALTER TABLE agent_runs ADD COLUMN revision INTEGER NOT NULL DEFAULT 1`)
	return err
}

func (s *SQLiteStore) CreateObjective(ctx context.Context, objective *Objective) error {
	if err := objective.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(objective)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO objectives
		(id, scope_kind, scope_id, owner_type, owner_id, status, priority, revision, updated_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		objective.ID, objective.Scope.Kind, objective.Scope.ID, objective.Owner.Type, objective.Owner.ID,
		objective.Status, objective.Priority, objective.Revision, objective.UpdatedAt, string(payload))
	return err
}

func (s *SQLiteStore) GetObjective(ctx context.Context, scope Scope, objectiveID string) (*Objective, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM objectives WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, objectiveID).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeObjective(payload)
}

func (s *SQLiteStore) ListObjectives(ctx context.Context, filter ObjectiveFilter) ([]*Objective, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM objectives WHERE scope_kind = ? AND scope_id = ?`, filter.Scope.Kind, filter.Scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*Objective, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		objective, err := decodeObjective(payload)
		if err != nil {
			return nil, err
		}
		if matchesObjectiveFilter(objective, filter) {
			result = append(result, objective)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Priority != result[j].Priority {
			return result[i].Priority > result[j].Priority
		}
		if !result[i].UpdatedAt.Equal(result[j].UpdatedAt) {
			return result[i].UpdatedAt.After(result[j].UpdatedAt)
		}
		return result[i].ID < result[j].ID
	})
	return pageObjectives(result, filter.Offset, filter.Limit), nil
}

func (s *SQLiteStore) UpdateObjective(ctx context.Context, objective *Objective, expectedRevision int64) error {
	if err := objective.Validate(); err != nil {
		return err
	}
	if objective.Revision != expectedRevision+1 {
		return ErrRevisionConflict
	}
	payload, err := json.Marshal(objective)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE objectives SET owner_type = ?, owner_id = ?, status = ?, priority = ?, revision = ?, updated_at = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`,
		objective.Owner.Type, objective.Owner.ID, objective.Status, objective.Priority, objective.Revision,
		objective.UpdatedAt, string(payload), objective.Scope.Kind, objective.Scope.ID, objective.ID, expectedRevision)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrRevisionConflict
	}
	return nil
}

func (s *SQLiteStore) CreateAgentRun(ctx context.Context, run *AgentRun) error {
	if err := run.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(run)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO agent_runs
		(id, scope_kind, scope_id, objective_id, parent_run_id, root_run_id, assigned_agent_id, status, priority, revision, created_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.Scope.Kind, run.Scope.ID, run.ObjectiveID, run.ParentRunID, run.RootRunID,
		run.AssignedAgentID, run.Status, run.Priority, run.Revision, run.CreatedAt, string(payload))
	return err
}

func (s *SQLiteStore) GetAgentRun(ctx context.Context, scope Scope, runID string) (*AgentRun, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM agent_runs WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, runID).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeAgentRun(payload)
}

func (s *SQLiteStore) ListAgentRuns(ctx context.Context, filter AgentRunFilter) ([]*AgentRun, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM agent_runs WHERE scope_kind = ? AND scope_id = ?`, filter.Scope.Kind, filter.Scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*AgentRun, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		run, err := decodeAgentRun(payload)
		if err != nil {
			return nil, err
		}
		if matchesRunFilter(run, filter) {
			result = append(result, run)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Priority != result[j].Priority {
			return result[i].Priority > result[j].Priority
		}
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.Before(result[j].CreatedAt)
		}
		return result[i].ID < result[j].ID
	})
	return pageAgentRuns(result, filter.Offset, filter.Limit), nil
}

func decodeObjective(payload string) (*Objective, error) {
	var objective Objective
	if err := json.Unmarshal([]byte(payload), &objective); err != nil {
		return nil, fmt.Errorf("decode objective: %w", err)
	}
	return &objective, nil
}

func decodeAgentRun(payload string) (*AgentRun, error) {
	var run AgentRun
	if err := json.Unmarshal([]byte(payload), &run); err != nil {
		return nil, fmt.Errorf("decode agent run: %w", err)
	}
	if run.Revision == 0 {
		run.Revision = 1
	}
	return &run, nil
}
