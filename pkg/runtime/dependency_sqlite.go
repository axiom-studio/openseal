package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

func migrateRunDependencies(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS run_dependency_groups (
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			id TEXT NOT NULL,
			source_run_id TEXT NOT NULL,
			status TEXT NOT NULL,
			idempotency_key TEXT NOT NULL DEFAULT '',
			revision INTEGER NOT NULL,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id),
			CHECK (revision > 0)
		);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_run_dependency_groups_idempotency
			ON run_dependency_groups(scope_kind, scope_id, idempotency_key) WHERE idempotency_key <> '';
		CREATE INDEX IF NOT EXISTS idx_run_dependency_groups_source
			ON run_dependency_groups(scope_kind, scope_id, source_run_id, status, updated_at DESC);
		CREATE INDEX IF NOT EXISTS idx_run_dependency_groups_waiting_page
			ON run_dependency_groups(scope_kind, scope_id, id) WHERE status = 'waiting';

		CREATE TABLE IF NOT EXISTS run_dependencies (
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			group_id TEXT NOT NULL,
			id TEXT NOT NULL,
			source_run_id TEXT NOT NULL,
			target_run_id TEXT NOT NULL DEFAULT '',
			request_id TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL,
			state TEXT NOT NULL,
			required INTEGER NOT NULL,
			revision INTEGER NOT NULL,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, group_id, id),
			FOREIGN KEY (scope_kind, scope_id, group_id)
				REFERENCES run_dependency_groups(scope_kind, scope_id, id),
			CHECK (revision > 0),
			CHECK (required IN (0, 1))
		);
		CREATE INDEX IF NOT EXISTS idx_run_dependencies_request
			ON run_dependencies(scope_kind, scope_id, request_id) WHERE request_id <> '';
		CREATE INDEX IF NOT EXISTS idx_run_dependencies_target
			ON run_dependencies(scope_kind, scope_id, target_run_id) WHERE target_run_id <> '';
	`)
	return err
}

func (s *SQLiteStore) CreateRunDependencyGroup(ctx context.Context, record models.RunDependencyGroupCreateRecord) (*models.RunDependencyResult, error) {
	if err := models.ValidateRunDependencyGroupCreateRecord(record); err != nil {
		return nil, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	existingGroupID := record.Group.ID
	if record.Group.IdempotencyKey != "" {
		existingGroupID = ""
	}
	existing, err := getSQLiteDependencyGroup(ctx, conn, record.Group.Scope, existingGroupID, record.Group.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		edges, err := listSQLiteDependencies(ctx, conn, existing.Scope, existing.ID)
		if err != nil {
			return nil, err
		}
		if !models.SameDependencyGroupRecord(existing, edges, record) {
			return nil, models.ErrDependencyConflict
		}
		source, err := getSQLiteAgentRun(ctx, conn, existing.Scope, existing.SourceRunID)
		if err != nil {
			return nil, err
		}
		evaluation, err := models.EvaluateRunDependencies(existing, edges)
		if err != nil {
			return nil, err
		}
		replayed := &models.RunDependencyResult{Group: existing, Dependencies: edges, Source: source, Evaluation: evaluation, Replayed: true}
		if err := readSQLiteDependencyGroupSessionState(ctx, conn, record, replayed); err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, err
		}
		committed = true
		return replayed, nil
	}
	currentSource, err := getSQLiteAgentRun(ctx, conn, record.SourceRun.Scope, record.SourceRun.ID)
	if err != nil {
		return nil, err
	}
	if err := models.CheckDependencyGroupCreateSource(currentSource, record); err != nil {
		return nil, err
	}
	if err := updateSQLiteAgentRunConn(ctx, conn, record.SourceRun, record.ExpectedSourceRevision); err != nil {
		return nil, err
	}
	for _, target := range record.TargetRuns {
		if err := insertSQLiteAgentRunConn(ctx, conn, target); err != nil {
			return nil, err
		}
	}
	groupPayload, err := json.Marshal(record.Group)
	if err != nil {
		return nil, err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO run_dependency_groups
		(scope_kind, scope_id, id, source_run_id, status, idempotency_key, revision, created_at, updated_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, record.Group.Scope.Kind, record.Group.Scope.ID, record.Group.ID,
		record.Group.SourceRunID, record.Group.Status, record.Group.IdempotencyKey, record.Group.Revision,
		record.Group.CreatedAt, record.Group.UpdatedAt, string(groupPayload))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, models.ErrDependencyConflict
		}
		return nil, err
	}
	for _, edge := range record.Dependencies {
		if err := insertSQLiteDependency(ctx, conn, edge); err != nil {
			return nil, err
		}
	}
	persisted, err := insertSQLiteActivityConn(ctx, conn, record.Event)
	if err != nil {
		return nil, err
	}
	result := &models.RunDependencyResult{
		Group: models.CloneRunDependencyGroup(record.Group), Source: models.CloneAgentRun(record.SourceRun), Events: []*models.ActivityEvent{persisted},
	}
	if err := s.applySQLiteDependencyGroupSessionWrites(ctx, conn, record, currentSource.SessionID, result); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	result.Dependencies = models.CloneRunDependencies(record.Dependencies)
	result.Evaluation, err = models.EvaluateRunDependencies(record.Group, result.Dependencies)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// applySQLiteDependencyGroupSessionWrites creates the record's same-Agent
// child sessions and moves the source's control tool entry, inside the
// create's BEGIN IMMEDIATE. The source fence was checked by the caller.
func (s *SQLiteStore) applySQLiteDependencyGroupSessionWrites(ctx context.Context, conn *sql.Conn, record models.RunDependencyGroupCreateRecord, sourceSessionID string, result *models.RunDependencyResult) error {
	for i := range record.ChildSessions {
		created, err := s.createRunAgentSessionTx(ctx, conn, models.DependencyGroupChildSession(record, i))
		if err != nil {
			return err
		}
		if !created.Created {
			return models.ErrAgentSessionConflict
		}
		result.ChildSessions = append(result.ChildSessions, created.Session)
	}
	if record.ToolTransition == nil {
		return nil
	}
	transition := models.DependencyGroupToolTransition(record)
	current, err := getSQLiteSessionEntry(ctx, conn, transition.Scope, transition.EntryID)
	if err != nil {
		return err
	}
	if current.SessionID != sourceSessionID {
		return models.ErrSessionMoved
	}
	moved, err := s.transitionToolEntryTx(ctx, conn, transition)
	if err != nil {
		return err
	}
	result.ToolEntry = moved.Entry
	result.Events = append(result.Events, moved.Events...)
	return nil
}

// readSQLiteDependencyGroupSessionState fills a replay with the current
// tool entry and child sessions; a replay writes nothing.
func readSQLiteDependencyGroupSessionState(ctx context.Context, conn *sql.Conn, record models.RunDependencyGroupCreateRecord, result *models.RunDependencyResult) error {
	if transition := record.ToolTransition; transition != nil {
		entry, err := getSQLiteSessionEntry(ctx, conn, transition.Scope, transition.EntryID)
		if err != nil && !errors.Is(err, models.ErrSessionEntryNotFound) {
			return err
		}
		result.ToolEntry = entry
	}
	for _, child := range record.ChildSessions {
		session, err := getSQLiteAgentSession(ctx, conn, child.Scope, models.RunAgentSessionID(child.Scope, child.OwnerRunID))
		if errors.Is(err, models.ErrAgentSessionNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		result.ChildSessions = append(result.ChildSessions, session)
	}
	return nil
}

func (s *SQLiteStore) GetRunDependencyGroup(ctx context.Context, scope models.Scope, groupID string) (*models.RunDependencyGroup, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return getSQLiteDependencyGroup(ctx, s.db, scope, strings.TrimSpace(groupID), "")
}

func (s *SQLiteStore) FindRunDependencyGroupByIdempotencyKey(ctx context.Context, scope models.Scope, key string) (*models.RunDependencyGroup, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	return getSQLiteDependencyGroup(ctx, s.db, scope, "", strings.TrimSpace(key))
}

func (s *SQLiteStore) ListRunDependencies(ctx context.Context, scope models.Scope, groupID string) ([]*models.RunDependency, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	group, err := getSQLiteDependencyGroup(ctx, s.db, scope, strings.TrimSpace(groupID), "")
	if err != nil {
		return nil, err
	}
	if group == nil {
		return nil, models.ErrDependencyGroupNotFound
	}
	return listSQLiteDependencies(ctx, s.db, scope, group.ID)
}

func (s *SQLiteStore) ResolveRunDependency(ctx context.Context, record models.RunDependencyResolutionRecord) (*models.RunDependencyResult, error) {
	if err := models.ValidateRunDependencyResolutionRecord(record); err != nil {
		return nil, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	result, err := s.resolveSQLiteDependencyConn(ctx, conn, record)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return result, nil
}

func (s *SQLiteStore) resolveSQLiteDependencyConn(ctx context.Context, conn *sql.Conn, record models.RunDependencyResolutionRecord) (*models.RunDependencyResult, error) {
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
	result, err := models.ApplyRunDependencyResolution(group, edges, source, record)
	if err != nil || result.Replayed {
		return result, err
	}
	if err := persistSQLiteDependencyResult(ctx, conn, group, edges, source, result); err != nil {
		return nil, err
	}
	return result, nil
}

type sqliteDependencyQueryer interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
}

func getSQLiteDependencyGroup(ctx context.Context, queryer sqliteDependencyQueryer, scope models.Scope, groupID, idempotencyKey string) (*models.RunDependencyGroup, error) {
	query := `SELECT payload FROM run_dependency_groups WHERE scope_kind = ? AND scope_id = ?`
	args := []interface{}{scope.Kind, scope.ID}
	if strings.TrimSpace(groupID) != "" {
		query += ` AND id = ?`
		args = append(args, strings.TrimSpace(groupID))
	} else {
		query += ` AND idempotency_key = ?`
		args = append(args, strings.TrimSpace(idempotencyKey))
	}
	var payload string
	err := queryer.QueryRowContext(ctx, query, args...).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return sqlstore.DecodeRunDependencyGroup(payload)
}

func listSQLiteDependencies(ctx context.Context, queryer sqliteDependencyQueryer, scope models.Scope, groupID string) ([]*models.RunDependency, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT payload FROM run_dependencies
		WHERE scope_kind = ? AND scope_id = ? AND group_id = ? ORDER BY id`, scope.Kind, scope.ID, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*models.RunDependency, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		edge, err := sqlstore.DecodeRunDependency(payload)
		if err != nil {
			return nil, err
		}
		result = append(result, edge)
	}
	return result, rows.Err()
}

func getSQLiteAgentRun(ctx context.Context, queryer sqliteDependencyQueryer, scope models.Scope, runID string) (*models.AgentRun, error) {
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM agent_runs WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
		scope.Kind, scope.ID, runID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, models.ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	return sqlstore.DecodeAgentRun(payload)
}

func insertSQLiteDependency(ctx context.Context, conn *sql.Conn, edge *models.RunDependency) error {
	payload, err := json.Marshal(edge)
	if err != nil {
		return err
	}
	required := 0
	if edge.Required {
		required = 1
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO run_dependencies
		(scope_kind, scope_id, group_id, id, source_run_id, target_run_id, request_id, kind, state, required,
		 revision, created_at, updated_at, payload) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		edge.Scope.Kind, edge.Scope.ID, edge.GroupID, edge.ID, edge.SourceRunID, edge.TargetRunID, edge.RequestID,
		edge.Kind, edge.State, required, edge.Revision, edge.CreatedAt, edge.UpdatedAt, string(payload))
	return err
}

func updateSQLiteDependency(ctx context.Context, conn *sql.Conn, edge *models.RunDependency, expectedRevision int64) error {
	payload, err := json.Marshal(edge)
	if err != nil {
		return err
	}
	result, err := conn.ExecContext(ctx, `UPDATE run_dependencies SET target_run_id = ?, request_id = ?, state = ?, revision = ?, updated_at = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND group_id = ? AND id = ? AND revision = ?`, edge.TargetRunID,
		edge.RequestID, edge.State, edge.Revision, edge.UpdatedAt, string(payload), edge.Scope.Kind, edge.Scope.ID,
		edge.GroupID, edge.ID, expectedRevision)
	return expectDependencyRow(result, err)
}

func updateSQLiteDependencyGroup(ctx context.Context, conn *sql.Conn, group *models.RunDependencyGroup, expectedRevision int64) error {
	payload, err := json.Marshal(group)
	if err != nil {
		return err
	}
	result, err := conn.ExecContext(ctx, `UPDATE run_dependency_groups SET status = ?, revision = ?, updated_at = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`, group.Status, group.Revision, group.UpdatedAt,
		string(payload), group.Scope.Kind, group.Scope.ID, group.ID, expectedRevision)
	return expectDependencyRow(result, err)
}

func expectDependencyRow(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return models.ErrRevisionConflict
	}
	return nil
}
