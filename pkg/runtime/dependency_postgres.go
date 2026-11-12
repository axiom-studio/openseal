package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
	"github.com/lib/pq"
)

func (s *PostgresStore) migrateRunDependencies(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("run_dependency_groups")+` (
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			id TEXT NOT NULL,
			source_run_id TEXT NOT NULL,
			status TEXT NOT NULL,
			idempotency_key TEXT NOT NULL DEFAULT '',
			revision BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id),
			CHECK (revision > 0)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("run_dependencies")+` (
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			group_id TEXT NOT NULL,
			id TEXT NOT NULL,
			source_run_id TEXT NOT NULL,
			target_run_id TEXT NOT NULL DEFAULT '',
			request_id TEXT NOT NULL DEFAULT '',
			kind TEXT NOT NULL,
			state TEXT NOT NULL,
			required BOOLEAN NOT NULL,
			revision BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, group_id, id),
			FOREIGN KEY (scope_kind, scope_id, group_id)
				REFERENCES `+s.table("run_dependency_groups")+` (scope_kind, scope_id, id) ON DELETE CASCADE,
			CHECK (revision > 0)
		)`); err != nil {
		return err
	}
	statements := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS run_dependency_groups_idempotency_idx ON ` + s.table("run_dependency_groups") + ` (scope_kind, scope_id, idempotency_key) WHERE idempotency_key <> ''`,
		`CREATE INDEX IF NOT EXISTS run_dependency_groups_source_idx ON ` + s.table("run_dependency_groups") + ` (scope_kind, scope_id, source_run_id, status, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS run_dependencies_request_idx ON ` + s.table("run_dependencies") + ` (scope_kind, scope_id, request_id) WHERE request_id <> ''`,
		`CREATE INDEX IF NOT EXISTS run_dependencies_target_idx ON ` + s.table("run_dependencies") + ` (scope_kind, scope_id, target_run_id) WHERE target_run_id <> ''`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name)
		VALUES (9, 'durable run dependency fan-in') ON CONFLICT (version) DO NOTHING`)
	return err
}

func (s *PostgresStore) CreateRunDependencyGroup(ctx context.Context, record models.RunDependencyGroupCreateRecord) (*models.RunDependencyResult, error) {
	if err := models.ValidateRunDependencyGroupCreateRecord(record); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	creationKey := record.Group.IdempotencyKey
	if creationKey == "" {
		creationKey = record.Group.ID
	}
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`,
		"openseal:dependency-group:"+record.Group.Scope.Key()+":"+creationKey); err != nil {
		return nil, err
	}
	existingGroupID := record.Group.ID
	if record.Group.IdempotencyKey != "" {
		existingGroupID = ""
	}
	existing, err := s.getPostgresDependencyGroup(ctx, tx, record.Group.Scope, existingGroupID, record.Group.IdempotencyKey, true)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		edges, err := s.listPostgresDependencies(ctx, tx, existing.Scope, existing.ID, true)
		if err != nil {
			return nil, err
		}
		if !models.SameDependencyGroupRecord(existing, edges, record) {
			return nil, models.ErrDependencyConflict
		}
		source, err := s.getPostgresAgentRunTx(ctx, tx, existing.Scope, existing.SourceRunID, true)
		if err != nil {
			return nil, err
		}
		evaluation, err := models.EvaluateRunDependencies(existing, edges)
		if err != nil {
			return nil, err
		}
		replayed := &models.RunDependencyResult{Group: existing, Dependencies: edges, Source: source, Evaluation: evaluation, Replayed: true}
		if err := s.readPostgresDependencyGroupSessionState(ctx, tx, record, replayed); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return replayed, nil
	}
	currentSource, err := s.getPostgresAgentRunTx(ctx, tx, record.SourceRun.Scope, record.SourceRun.ID, true)
	if err != nil {
		return nil, err
	}
	if err := models.CheckDependencyGroupCreateSource(currentSource, record); err != nil {
		return nil, err
	}
	if err := s.updatePostgresAgentRunTx(ctx, tx, record.SourceRun, record.ExpectedSourceRevision); err != nil {
		return nil, err
	}
	for _, target := range record.TargetRuns {
		if err := s.insertPostgresAgentRunTx(ctx, tx, target); err != nil {
			return nil, err
		}
	}
	groupPayload, err := json.Marshal(record.Group)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("run_dependency_groups")+`
		(scope_kind, scope_id, id, source_run_id, status, idempotency_key, revision, created_at, updated_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb)`, record.Group.Scope.Kind,
		record.Group.Scope.ID, record.Group.ID, record.Group.SourceRunID, record.Group.Status, record.Group.IdempotencyKey,
		record.Group.Revision, record.Group.CreatedAt, record.Group.UpdatedAt, string(groupPayload))
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			return nil, models.ErrDependencyConflict
		}
		return nil, err
	}
	for _, edge := range record.Dependencies {
		if err := s.insertPostgresDependencyTx(ctx, tx, edge); err != nil {
			return nil, err
		}
	}
	persisted, err := s.insertPostgresActivityTx(ctx, tx, record.Event)
	if err != nil {
		return nil, err
	}
	result := &models.RunDependencyResult{
		Group: models.CloneRunDependencyGroup(record.Group), Source: models.CloneAgentRun(record.SourceRun), Events: []*models.ActivityEvent{persisted},
	}
	if err := s.applyPostgresDependencyGroupSessionWrites(ctx, tx, record, currentSource.SessionID, result); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	result.Dependencies = models.CloneRunDependencies(record.Dependencies)
	result.Evaluation, err = models.EvaluateRunDependencies(record.Group, result.Dependencies)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// applyPostgresDependencyGroupSessionWrites creates the record's same-Agent
// child sessions and moves the source's control tool entry inside the
// create's transaction. The source Run row is already locked and fenced by
// the caller, so the lock order stays Run -> session -> entry.
func (s *PostgresStore) applyPostgresDependencyGroupSessionWrites(ctx context.Context, tx *sql.Tx, record models.RunDependencyGroupCreateRecord, sourceSessionID string, result *models.RunDependencyResult) error {
	for i := range record.ChildSessions {
		created, err := s.createRunAgentSessionTx(ctx, tx, models.DependencyGroupChildSession(record, i))
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
	current, err := s.getPostgresSessionEntryTx(ctx, tx, transition.Scope, transition.EntryID, false)
	if err != nil {
		return err
	}
	if current.SessionID != sourceSessionID {
		return models.ErrSessionMoved
	}
	moved, err := s.transitionToolEntryTx(ctx, tx, transition)
	if err != nil {
		return err
	}
	result.ToolEntry = moved.Entry
	result.Events = append(result.Events, moved.Events...)
	return nil
}

// readPostgresDependencyGroupSessionState fills a replay with the current
// tool entry and child sessions; a replay writes nothing.
func (s *PostgresStore) readPostgresDependencyGroupSessionState(ctx context.Context, tx *sql.Tx, record models.RunDependencyGroupCreateRecord, result *models.RunDependencyResult) error {
	if transition := record.ToolTransition; transition != nil {
		entry, err := s.getPostgresSessionEntryTx(ctx, tx, transition.Scope, transition.EntryID, false)
		if err != nil && !errors.Is(err, models.ErrSessionEntryNotFound) {
			return err
		}
		result.ToolEntry = entry
	}
	for _, child := range record.ChildSessions {
		session, err := s.getPostgresAgentSessionTx(ctx, tx, child.Scope, models.RunAgentSessionID(child.Scope, child.OwnerRunID), false)
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

func (s *PostgresStore) GetRunDependencyGroup(ctx context.Context, scope models.Scope, groupID string) (*models.RunDependencyGroup, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.getPostgresDependencyGroup(ctx, s.db, scope, strings.TrimSpace(groupID), "", false)
}

func (s *PostgresStore) FindRunDependencyGroupByIdempotencyKey(ctx context.Context, scope models.Scope, key string) (*models.RunDependencyGroup, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	return s.getPostgresDependencyGroup(ctx, s.db, scope, "", strings.TrimSpace(key), false)
}

func (s *PostgresStore) ListRunDependencies(ctx context.Context, scope models.Scope, groupID string) ([]*models.RunDependency, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	group, err := s.getPostgresDependencyGroup(ctx, s.db, scope, strings.TrimSpace(groupID), "", false)
	if err != nil {
		return nil, err
	}
	if group == nil {
		return nil, models.ErrDependencyGroupNotFound
	}
	return s.listPostgresDependencies(ctx, s.db, scope, group.ID, false)
}

func (s *PostgresStore) ResolveRunDependency(ctx context.Context, record models.RunDependencyResolutionRecord) (*models.RunDependencyResult, error) {
	if err := models.ValidateRunDependencyResolutionRecord(record); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := s.resolvePostgresDependencyTx(ctx, tx, record)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *PostgresStore) resolvePostgresDependencyTx(ctx context.Context, tx *sql.Tx, record models.RunDependencyResolutionRecord) (*models.RunDependencyResult, error) {
	group, err := s.getPostgresDependencyGroup(ctx, tx, record.Scope, record.GroupID, "", true)
	if err != nil {
		return nil, err
	}
	if group == nil {
		return nil, models.ErrDependencyGroupNotFound
	}
	edges, err := s.listPostgresDependencies(ctx, tx, record.Scope, group.ID, true)
	if err != nil {
		return nil, err
	}
	source, err := s.getPostgresAgentRunTx(ctx, tx, record.Scope, group.SourceRunID, true)
	if err != nil {
		return nil, err
	}
	result, err := models.ApplyRunDependencyResolution(group, edges, source, record)
	if err != nil || result.Replayed {
		return result, err
	}
	if err := s.persistPostgresDependencyResult(ctx, tx, group, edges, source, result); err != nil {
		return nil, err
	}
	return result, nil
}

type postgresDependencyQueryer interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
}

func (s *PostgresStore) getPostgresDependencyGroup(ctx context.Context, queryer postgresDependencyQueryer, scope models.Scope, groupID, idempotencyKey string, lock bool) (*models.RunDependencyGroup, error) {
	query := `SELECT payload FROM ` + s.table("run_dependency_groups") + ` WHERE scope_kind = $1 AND scope_id = $2`
	args := []interface{}{scope.Kind, scope.ID}
	if strings.TrimSpace(groupID) != "" {
		query += ` AND id = $3`
		args = append(args, strings.TrimSpace(groupID))
	} else {
		query += ` AND idempotency_key = $3`
		args = append(args, strings.TrimSpace(idempotencyKey))
	}
	if lock {
		query += ` FOR UPDATE`
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

func (s *PostgresStore) listPostgresDependencies(ctx context.Context, queryer postgresDependencyQueryer, scope models.Scope, groupID string, lock bool) ([]*models.RunDependency, error) {
	query := `SELECT payload FROM ` + s.table("run_dependencies") + ` WHERE scope_kind = $1 AND scope_id = $2 AND group_id = $3 ORDER BY id`
	if lock {
		query += ` FOR UPDATE`
	}
	rows, err := queryer.QueryContext(ctx, query, scope.Kind, scope.ID, groupID)
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

func (s *PostgresStore) getPostgresAgentRunTx(ctx context.Context, queryer postgresDependencyQueryer, scope models.Scope, runID string, lock bool) (*models.AgentRun, error) {
	query := `SELECT payload FROM ` + s.table("agent_runs") + ` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`
	if lock {
		query += ` FOR UPDATE`
	}
	var payload string
	err := queryer.QueryRowContext(ctx, query, scope.Kind, scope.ID, runID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, models.ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	return sqlstore.DecodeAgentRun(payload)
}

func (s *PostgresStore) insertPostgresDependencyTx(ctx context.Context, tx *sql.Tx, edge *models.RunDependency) error {
	payload, err := json.Marshal(edge)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("run_dependencies")+`
		(scope_kind, scope_id, group_id, id, source_run_id, target_run_id, request_id, kind, state, required,
		 revision, created_at, updated_at, payload) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14::jsonb)`,
		edge.Scope.Kind, edge.Scope.ID, edge.GroupID, edge.ID, edge.SourceRunID, edge.TargetRunID, edge.RequestID,
		edge.Kind, edge.State, edge.Required, edge.Revision, edge.CreatedAt, edge.UpdatedAt, string(payload))
	return err
}

func (s *PostgresStore) updatePostgresDependencyTx(ctx context.Context, tx *sql.Tx, edge *models.RunDependency, expectedRevision int64) error {
	payload, err := json.Marshal(edge)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("run_dependencies")+` SET target_run_id = $1, request_id = $2,
		state = $3, revision = $4, updated_at = $5, payload = $6::jsonb WHERE scope_kind = $7 AND scope_id = $8
		AND group_id = $9 AND id = $10 AND revision = $11`, edge.TargetRunID, edge.RequestID, edge.State, edge.Revision,
		edge.UpdatedAt, string(payload), edge.Scope.Kind, edge.Scope.ID, edge.GroupID, edge.ID, expectedRevision)
	return expectPostgresDependencyRow(result, err)
}

func (s *PostgresStore) updatePostgresDependencyGroupTx(ctx context.Context, tx *sql.Tx, group *models.RunDependencyGroup, expectedRevision int64) error {
	payload, err := json.Marshal(group)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("run_dependency_groups")+` SET status = $1, revision = $2,
		updated_at = $3, payload = $4::jsonb WHERE scope_kind = $5 AND scope_id = $6 AND id = $7 AND revision = $8`,
		group.Status, group.Revision, group.UpdatedAt, string(payload), group.Scope.Kind, group.Scope.ID, group.ID, expectedRevision)
	return expectPostgresDependencyRow(result, err)
}

func expectPostgresDependencyRow(result sql.Result, err error) error {
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
