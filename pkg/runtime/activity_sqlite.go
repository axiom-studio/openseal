package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

func (s *SQLiteStore) CreateObjectiveWithEvent(ctx context.Context, objective *Objective, event *ActivityEvent) (*ActivityEvent, error) {
	if err := validateObjectiveActivity(objective, event); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(objective)
	if err != nil {
		return nil, err
	}
	return s.withImmediateActivity(ctx, event, func(conn *sql.Conn) error {
		_, insertErr := conn.ExecContext(ctx, `INSERT INTO objectives
			(id, scope_kind, scope_id, owner_type, owner_id, status, priority, revision, updated_at, payload)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, objective.ID, objective.Scope.Kind, objective.Scope.ID,
			objective.Owner.Type, objective.Owner.ID, objective.Status, objective.Priority, objective.Revision,
			objective.UpdatedAt, string(payload))
		return insertErr
	})
}

func (s *SQLiteStore) UpdateObjectiveWithEvent(ctx context.Context, objective *Objective, expectedRevision int64, event *ActivityEvent) (*ActivityEvent, error) {
	if err := validateObjectiveActivity(objective, event); err != nil {
		return nil, err
	}
	if objective.Revision != expectedRevision+1 {
		return nil, ErrRevisionConflict
	}
	payload, err := json.Marshal(objective)
	if err != nil {
		return nil, err
	}
	return s.withImmediateActivity(ctx, event, func(conn *sql.Conn) error {
		result, updateErr := conn.ExecContext(ctx, `UPDATE objectives SET owner_type = ?, owner_id = ?, status = ?, priority = ?, revision = ?, updated_at = ?, payload = ?
			WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`, objective.Owner.Type, objective.Owner.ID,
			objective.Status, objective.Priority, objective.Revision, objective.UpdatedAt, string(payload), objective.Scope.Kind,
			objective.Scope.ID, objective.ID, expectedRevision)
		if updateErr != nil {
			return updateErr
		}
		affected, updateErr := result.RowsAffected()
		if updateErr != nil {
			return updateErr
		}
		if affected != 1 {
			return ErrRevisionConflict
		}
		return nil
	})
}

func validateObjectiveActivity(objective *Objective, event *ActivityEvent) error {
	if err := objective.Validate(); err != nil {
		return err
	}
	if err := event.Validate(); err != nil {
		return err
	}
	if objective.Scope != event.Scope || objective.ID != event.ObjectiveID || event.RunID != "" {
		return ErrInvalidScope
	}
	return nil
}

func (s *SQLiteStore) CreateAgentRunWithEvent(ctx context.Context, run *AgentRun, event *ActivityEvent) (*ActivityEvent, error) {
	if err := run.Validate(); err != nil {
		return nil, err
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	if run.Scope != event.Scope || run.ID != event.RunID {
		return nil, ErrInvalidScope
	}
	return s.withImmediateActivity(ctx, event, func(conn *sql.Conn) error {
		if err := allocateSQLiteObjectiveRunConn(ctx, conn, run); err != nil {
			return err
		}
		return insertSQLiteAgentRunConn(ctx, conn, run)
	})
}

func migrateActivity(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS run_activity (
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			agent_id TEXT NOT NULL DEFAULT '',
			objective_id TEXT NOT NULL DEFAULT '',
			project_id TEXT NOT NULL DEFAULT '',
			team_id TEXT NOT NULL DEFAULT '',
			severity TEXT NOT NULL DEFAULT 'info',
			visibility TEXT NOT NULL DEFAULT 'scope',
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
	if err != nil {
		return err
	}
	if err := addMissingActivityColumns(db); err != nil {
		return err
	}
	_, err = db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_run_activity_agent_feed
			ON run_activity(scope_kind, scope_id, agent_id, created_at DESC, id DESC);
		CREATE INDEX IF NOT EXISTS idx_run_activity_objective_feed
			ON run_activity(scope_kind, scope_id, objective_id, created_at DESC, id DESC);
		CREATE INDEX IF NOT EXISTS idx_run_activity_project_feed
			ON run_activity(scope_kind, scope_id, project_id, created_at DESC, id DESC);
		CREATE INDEX IF NOT EXISTS idx_run_activity_team_feed
			ON run_activity(scope_kind, scope_id, team_id, created_at DESC, id DESC);
	`)
	return err
}

func addMissingActivityColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(run_activity)`)
	if err != nil {
		return err
	}
	columns := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	additions := []struct{ name, statement string }{
		{"agent_id", `ALTER TABLE run_activity ADD COLUMN agent_id TEXT NOT NULL DEFAULT ''`},
		{"objective_id", `ALTER TABLE run_activity ADD COLUMN objective_id TEXT NOT NULL DEFAULT ''`},
		{"project_id", `ALTER TABLE run_activity ADD COLUMN project_id TEXT NOT NULL DEFAULT ''`},
		{"team_id", `ALTER TABLE run_activity ADD COLUMN team_id TEXT NOT NULL DEFAULT ''`},
		{"severity", `ALTER TABLE run_activity ADD COLUMN severity TEXT NOT NULL DEFAULT 'info'`},
		{"visibility", `ALTER TABLE run_activity ADD COLUMN visibility TEXT NOT NULL DEFAULT 'scope'`},
	}
	for _, addition := range additions {
		if columns[addition.name] {
			continue
		}
		if _, err := db.Exec(addition.statement); err != nil {
			return fmt.Errorf("add run_activity.%s: %w", addition.name, err)
		}
	}
	_, err = db.Exec(`UPDATE run_activity SET
		agent_id = COALESCE(NULLIF(agent_id, ''), json_extract(payload, '$.agentId'), ''),
		objective_id = COALESCE(NULLIF(objective_id, ''), json_extract(payload, '$.objectiveId'), ''),
		project_id = COALESCE(NULLIF(project_id, ''), json_extract(payload, '$.projectId'), ''),
		team_id = COALESCE(NULLIF(team_id, ''), json_extract(payload, '$.teamId'), ''),
		severity = COALESCE(NULLIF(severity, ''), json_extract(payload, '$.severity'), 'info'),
		visibility = COALESCE(NULLIF(visibility, ''), json_extract(payload, '$.visibility'), 'scope')`)
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
		table, id := "agent_runs", event.RunID
		if event.RunID == "" {
			table, id = "objectives", event.ObjectiveID
		}
		err := conn.QueryRowContext(ctx, `SELECT 1 FROM `+table+` WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
			event.Scope.Kind, event.Scope.ID, id).Scan(&exists)
		if err == sql.ErrNoRows {
			if event.RunID == "" {
				return ErrObjectiveNotFound
			}
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
	persisted, err := insertSQLiteActivityConn(ctx, conn, event)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return cloneActivityEvent(persisted), nil
}

func insertSQLiteActivityConn(ctx context.Context, conn *sql.Conn, event *ActivityEvent) (*ActivityEvent, error) {
	persisted := cloneActivityEvent(event)
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM run_activity
		WHERE scope_kind = ? AND scope_id = ? AND run_id = ?`,
		event.Scope.Kind, event.Scope.ID, activityStreamID(event)).Scan(&persisted.Sequence); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(persisted)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO run_activity
		(scope_kind, scope_id, run_id, agent_id, objective_id, project_id, team_id, severity, visibility, sequence, id, event_type, created_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, persisted.Scope.Kind, persisted.Scope.ID, activityStreamID(persisted),
		persisted.AgentID, persisted.ObjectiveID, persisted.ProjectID, persisted.TeamID, persisted.Severity, persisted.Visibility,
		persisted.Sequence, persisted.ID, persisted.EventType, persisted.CreatedAt, string(payload)); err != nil {
		return nil, err
	}
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
	if !filter.Descending {
		rows, err := s.db.QueryContext(ctx, `SELECT payload FROM run_activity
			WHERE scope_kind = ? AND scope_id = ? AND run_id = ? AND sequence > ?
			ORDER BY sequence ASC LIMIT ?`, filter.Scope.Kind, filter.Scope.ID, filter.RunID, filter.AfterSequence, limit)
		if err != nil {
			return nil, err
		}
		return scanSQLiteActivity(rows, limit)
	}
	query := `SELECT payload FROM run_activity WHERE scope_kind = ? AND scope_id = ?`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	selectors := []struct{ column, value string }{{"run_id", filter.RunID}, {"agent_id", filter.AgentID}, {"objective_id", filter.ObjectiveID}, {"project_id", filter.ProjectID}, {"team_id", filter.TeamID}}
	for _, selector := range selectors {
		if selector.value != "" {
			query += " AND " + selector.column + " = ?"
			args = append(args, selector.value)
		}
	}
	query, args = appendSQLiteActivityStrings(query, args, "run_id", filter.RunIDs)
	query, args = appendSQLiteActivityStrings(query, args, "event_type", filter.EventTypes)
	severityValues := make([]string, 0, len(filter.Severities))
	for _, severity := range filter.Severities {
		severityValues = append(severityValues, string(severity))
	}
	query, args = appendSQLiteActivityStrings(query, args, "severity", severityValues)
	visibilityValues := make([]string, 0, len(filter.Visibilities))
	for _, visibility := range filter.Visibilities {
		visibilityValues = append(visibilityValues, string(visibility))
	}
	query, args = appendSQLiteActivityStrings(query, args, "visibility", visibilityValues)
	if filter.BeforeCreatedAt != nil {
		query += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, filter.BeforeCreatedAt, filter.BeforeCreatedAt, filter.BeforeID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return scanSQLiteActivity(rows, limit)
}

func appendSQLiteActivityStrings(query string, args []interface{}, column string, values []string) (string, []interface{}) {
	if len(values) == 0 {
		return query, args
	}
	query += " AND " + column + " IN (" + strings.TrimRight(strings.Repeat("?,", len(values)), ",") + ")"
	for _, value := range values {
		args = append(args, value)
	}
	return query, args
}

func scanSQLiteActivity(rows *sql.Rows, capacity int) ([]*ActivityEvent, error) {
	defer rows.Close()
	result := make([]*ActivityEvent, 0, capacity)
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
