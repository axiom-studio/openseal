package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lib/pq"
)

func (s *PostgresStore) migrateCollaboration(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("agent_requests")+` (
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			id TEXT NOT NULL,
			kind TEXT NOT NULL,
			status TEXT NOT NULL,
			requester_type TEXT NOT NULL,
			requester_id TEXT NOT NULL,
			recipient_type TEXT NOT NULL,
			recipient_id TEXT NOT NULL,
			source_run_id TEXT NOT NULL,
			child_run_id TEXT NOT NULL DEFAULT '',
			idempotency_key TEXT NOT NULL DEFAULT '',
			revision BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id),
			CHECK (revision > 0)
		)`); err != nil {
		return err
	}
	statements := []string{
		`CREATE INDEX IF NOT EXISTS agent_requests_recipient_idx ON ` + s.table("agent_requests") + ` (scope_kind, scope_id, recipient_type, recipient_id, status, updated_at DESC)`,
		`CREATE INDEX IF NOT EXISTS agent_requests_source_idx ON ` + s.table("agent_requests") + ` (scope_kind, scope_id, source_run_id, updated_at DESC)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS agent_requests_idempotency_idx ON ` + s.table("agent_requests") + ` (scope_kind, scope_id, idempotency_key) WHERE idempotency_key <> ''`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name) VALUES (7, 'agent requests and handoffs') ON CONFLICT (version) DO NOTHING`)
	return err
}

func (s *PostgresStore) CreateAgentRequest(ctx context.Context, record AgentRequestCreateRecord) (*ActivityEvent, error) {
	if err := validateAgentRequestCreateRecord(record); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(record.Request)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if record.MaximumConcurrent > 0 {
		lockKey := record.Request.Scope.Kind + ":" + record.Request.Scope.ID + ":" + record.DelegationTeamID
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
			return nil, err
		}
		var active int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+s.table("agent_requests")+`
			WHERE scope_kind = $1 AND scope_id = $2 AND status = ANY($3)
			AND payload->'delegationPolicy'->>'teamDeploymentId' = $4`,
			record.Request.Scope.Kind, record.Request.Scope.ID,
			pq.Array([]string{string(AgentRequestStatusPending), string(AgentRequestStatusClarificationRequested), string(AgentRequestStatusAccepted)}),
			record.DelegationTeamID).Scan(&active); err != nil {
			return nil, err
		}
		if active >= record.MaximumConcurrent {
			return nil, fmt.Errorf("%w: Team has %d active delegations, reaching maximum %d", ErrAgentRequestAssignment, active, record.MaximumConcurrent)
		}
	}
	if record.SourceRun != nil {
		if err := s.updatePostgresAgentRunTx(ctx, tx, record.SourceRun, record.ExpectedSourceRevision); err != nil {
			return nil, err
		}
	} else {
		var exists int
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM `+s.table("agent_runs")+` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3 FOR UPDATE`,
			record.Request.Scope.Kind, record.Request.Scope.ID, record.Request.SourceRunID).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRunNotFound
		}
		if err != nil {
			return nil, err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("agent_requests")+`
		(scope_kind, scope_id, id, kind, status, requester_type, requester_id, recipient_type, recipient_id,
		 source_run_id, child_run_id, idempotency_key, revision, created_at, updated_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16::jsonb)`,
		record.Request.Scope.Kind, record.Request.Scope.ID, record.Request.ID, record.Request.Kind, record.Request.Status,
		record.Request.Requester.Type, record.Request.Requester.ID, record.Request.Recipient.Type, record.Request.Recipient.ID,
		record.Request.SourceRunID, record.Request.ChildRunID, record.Request.IdempotencyKey, record.Request.Revision,
		record.Request.CreatedAt, record.Request.UpdatedAt, string(payload))
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == "23505" {
			return nil, ErrAgentRequestIdempotency
		}
		return nil, err
	}
	persisted, err := s.insertPostgresActivityTx(ctx, tx, record.Event)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return persisted, nil
}

func (s *PostgresStore) GetAgentRequest(ctx context.Context, scope Scope, requestID string) (*AgentRequest, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("agent_requests")+` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`,
		scope.Kind, scope.ID, requestID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeAgentRequest(payload)
}

func (s *PostgresStore) FindAgentRequestByIdempotencyKey(ctx context.Context, scope Scope, key string) (*AgentRequest, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("agent_requests")+` WHERE scope_kind = $1 AND scope_id = $2 AND idempotency_key = $3`,
		scope.Kind, scope.ID, strings.TrimSpace(key)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeAgentRequest(payload)
}

func (s *PostgresStore) ListAgentRequests(ctx context.Context, filter AgentRequestFilter) ([]*AgentRequest, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT payload FROM ` + s.table("agent_requests") + ` WHERE scope_kind = $1 AND scope_id = $2`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	placeholder := 3
	addOne := func(fragment string, value interface{}) {
		query += fmt.Sprintf(fragment, placeholder)
		args = append(args, value)
		placeholder++
	}
	addTwo := func(fragment string, first, second interface{}) {
		query += fmt.Sprintf(fragment, placeholder, placeholder+1)
		args = append(args, first, second)
		placeholder += 2
	}
	if filter.SourceRunID != "" {
		addOne(` AND source_run_id = $%d`, filter.SourceRunID)
	}
	if filter.Requester != nil {
		addTwo(` AND requester_type = $%d AND requester_id = $%d`, filter.Requester.Type, filter.Requester.ID)
	}
	if filter.Recipient != nil {
		addTwo(` AND recipient_type = $%d AND recipient_id = $%d`, filter.Recipient.Type, filter.Recipient.ID)
	}
	if len(filter.Kinds) > 0 {
		values := make([]string, 0, len(filter.Kinds))
		for _, kind := range filter.Kinds {
			values = append(values, string(kind))
		}
		addOne(` AND kind = ANY($%d)`, pq.Array(values))
	}
	if len(filter.Statuses) > 0 {
		values := make([]string, 0, len(filter.Statuses))
		for _, status := range filter.Statuses {
			values = append(values, string(status))
		}
		addOne(` AND status = ANY($%d)`, pq.Array(values))
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	query += fmt.Sprintf(` ORDER BY updated_at DESC, id ASC LIMIT $%d OFFSET $%d`, placeholder, placeholder+1)
	args = append(args, limit, offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*AgentRequest, 0, limit)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		request, err := decodeAgentRequest(payload)
		if err != nil {
			return nil, err
		}
		result = append(result, request)
	}
	return result, rows.Err()
}

func (s *PostgresStore) RespondAgentRequest(ctx context.Context, record AgentRequestResponseRecord) ([]*ActivityEvent, error) {
	if err := validateAgentRequestResponseRecord(record); err != nil {
		return nil, err
	}
	requestPayload, err := json.Marshal(record.Request)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var currentRevision int64
	err = tx.QueryRowContext(ctx, `SELECT revision FROM `+s.table("agent_requests")+` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3 FOR UPDATE`,
		record.Request.Scope.Kind, record.Request.Scope.ID, record.Request.ID).Scan(&currentRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAgentRequestNotFound
	}
	if err != nil {
		return nil, err
	}
	if currentRevision != record.ExpectedRequestRevision {
		return nil, ErrRevisionConflict
	}
	if record.SourceRun != nil {
		if err := s.updatePostgresAgentRunTx(ctx, tx, record.SourceRun, record.ExpectedSourceRevision); err != nil {
			return nil, err
		}
	}
	if record.ChildRun != nil {
		if err := s.insertPostgresAgentRunTx(ctx, tx, record.ChildRun); err != nil {
			return nil, err
		}
	}
	var dependencyEvents []*ActivityEvent
	if record.DependencyResolution != nil {
		dependencyResult, err := s.resolvePostgresDependencyTx(ctx, tx, *record.DependencyResolution)
		if err != nil {
			return nil, err
		}
		if dependencyResult.Replayed {
			return nil, ErrInvalidAgentRequestState
		}
		dependencyEvents = dependencyResult.Events
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("agent_requests")+` SET status = $1, child_run_id = $2, revision = $3,
		updated_at = $4, payload = $5::jsonb WHERE scope_kind = $6 AND scope_id = $7 AND id = $8 AND revision = $9`,
		record.Request.Status, record.Request.ChildRunID, record.Request.Revision, record.Request.UpdatedAt, string(requestPayload),
		record.Request.Scope.Kind, record.Request.Scope.ID, record.Request.ID, record.ExpectedRequestRevision)
	if err != nil {
		return nil, err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return nil, err
		}
		return nil, ErrRevisionConflict
	}
	events := make([]*ActivityEvent, 0, 2+len(dependencyEvents))
	for _, event := range []*ActivityEvent{record.SourceEvent, record.ChildEvent} {
		if event == nil {
			continue
		}
		persisted, err := s.insertPostgresActivityTx(ctx, tx, event)
		if err != nil {
			return nil, err
		}
		events = append(events, persisted)
	}
	events = append(events, dependencyEvents...)
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return events, nil
}

func (s *PostgresStore) CompleteAgentRequest(ctx context.Context, record AgentRequestCompletionRecord) ([]*ActivityEvent, error) {
	if err := validateAgentRequestCompletionRecord(record); err != nil {
		return nil, err
	}
	requestPayload, err := json.Marshal(record.Request)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var currentRevision int64
	err = tx.QueryRowContext(ctx, `SELECT revision FROM `+s.table("agent_requests")+` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3 FOR UPDATE`,
		record.Request.Scope.Kind, record.Request.Scope.ID, record.Request.ID).Scan(&currentRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrAgentRequestNotFound
	}
	if err != nil {
		return nil, err
	}
	if currentRevision != record.ExpectedRequestRevision {
		return nil, ErrRevisionConflict
	}
	var dependencyEvents []*ActivityEvent
	if record.DependencyResolution != nil {
		dependencyResult, err := s.resolvePostgresDependencyTx(ctx, tx, *record.DependencyResolution)
		if err != nil {
			return nil, err
		}
		if dependencyResult.Replayed {
			return nil, ErrInvalidAgentRequestState
		}
		dependencyEvents = dependencyResult.Events
	} else {
		if err := s.updatePostgresAgentRunTx(ctx, tx, record.SourceRun, record.ExpectedSourceRevision); err != nil {
			return nil, err
		}
	}
	if err := s.updatePostgresAgentRunTx(ctx, tx, record.ChildRun, record.ExpectedChildRevision); err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("agent_requests")+` SET status = $1, revision = $2, updated_at = $3,
		payload = $4::jsonb WHERE scope_kind = $5 AND scope_id = $6 AND id = $7 AND revision = $8`, record.Request.Status,
		record.Request.Revision, record.Request.UpdatedAt, string(requestPayload), record.Request.Scope.Kind, record.Request.Scope.ID,
		record.Request.ID, record.ExpectedRequestRevision)
	if err != nil {
		return nil, err
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr != nil || affected != 1 {
		if rowsErr != nil {
			return nil, rowsErr
		}
		return nil, ErrRevisionConflict
	}
	events := make([]*ActivityEvent, 0, 2+len(dependencyEvents))
	for _, event := range []*ActivityEvent{record.SourceEvent, record.ChildEvent} {
		persisted, insertErr := s.insertPostgresActivityTx(ctx, tx, event)
		if insertErr != nil {
			return nil, insertErr
		}
		events = append(events, persisted)
	}
	events = append(events, dependencyEvents...)
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return events, nil
}

func (s *PostgresStore) insertPostgresAgentRunTx(ctx context.Context, tx *sql.Tx, run *AgentRun) error {
	payload, err := json.Marshal(run)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("agent_runs")+`
		(id, scope_kind, scope_id, objective_id, parent_run_id, root_run_id, assigned_agent_id, status, priority, revision,
		 deadline, available_at, queue_entered_at, lease_owner, lease_expires_at, last_claimed_at, attempt, created_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19::jsonb)`,
		run.ID, run.Scope.Kind, run.Scope.ID, run.ObjectiveID, run.ParentRunID, run.RootRunID, run.AssignedAgentID,
		run.Status, run.Priority, run.Revision, run.Deadline, run.AvailableAt, run.QueueEnteredAt, run.LeaseOwner,
		run.LeaseExpiresAt, run.LastClaimedAt, run.Attempt, run.CreatedAt, string(payload))
	return err
}

func (s *PostgresStore) updatePostgresAgentRunTx(ctx context.Context, tx *sql.Tx, run *AgentRun, expectedRevision int64) error {
	payload, err := json.Marshal(run)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("agent_runs")+` SET status = $1, priority = $2, assigned_agent_id = $3,
		revision = $4, deadline = $5, available_at = $6, queue_entered_at = $7, lease_owner = $8, lease_expires_at = $9,
		last_claimed_at = $10, attempt = $11, payload = $12::jsonb WHERE scope_kind = $13 AND scope_id = $14 AND id = $15 AND revision = $16`,
		run.Status, run.Priority, run.AssignedAgentID, run.Revision, run.Deadline, run.AvailableAt, run.QueueEnteredAt,
		run.LeaseOwner, run.LeaseExpiresAt, run.LastClaimedAt, run.Attempt, string(payload), run.Scope.Kind, run.Scope.ID,
		run.ID, expectedRevision)
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
