package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

func migrateCollaboration(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_requests (
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
			revision INTEGER NOT NULL,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id)
		);
		CREATE INDEX IF NOT EXISTS idx_agent_requests_recipient
			ON agent_requests(scope_kind, scope_id, recipient_type, recipient_id, status, updated_at DESC);
		CREATE INDEX IF NOT EXISTS idx_agent_requests_source
			ON agent_requests(scope_kind, scope_id, source_run_id, updated_at DESC);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_requests_idempotency
			ON agent_requests(scope_kind, scope_id, idempotency_key) WHERE idempotency_key <> '';
	`)
	return err
}

func (s *SQLiteStore) CreateAgentRequest(ctx context.Context, record AgentRequestCreateRecord) (*ActivityEvent, error) {
	if record.Request == nil || record.Event == nil {
		return nil, ErrAgentRequestNotFound
	}
	if err := record.Request.Validate(); err != nil {
		return nil, err
	}
	if err := record.Event.Validate(); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(record.Request)
	if err != nil {
		return nil, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	var exists int
	if err := conn.QueryRowContext(ctx, `SELECT 1 FROM agent_runs WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
		record.Request.Scope.Kind, record.Request.Scope.ID, record.Request.SourceRunID).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrRunNotFound
		}
		return nil, err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO agent_requests
		(scope_kind, scope_id, id, kind, status, requester_type, requester_id, recipient_type, recipient_id,
		 source_run_id, child_run_id, idempotency_key, revision, created_at, updated_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.Request.Scope.Kind, record.Request.Scope.ID, record.Request.ID, record.Request.Kind, record.Request.Status,
		record.Request.Requester.Type, record.Request.Requester.ID, record.Request.Recipient.Type, record.Request.Recipient.ID,
		record.Request.SourceRunID, record.Request.ChildRunID, record.Request.IdempotencyKey, record.Request.Revision,
		record.Request.CreatedAt, record.Request.UpdatedAt, string(payload))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique") {
			return nil, ErrAgentRequestIdempotency
		}
		return nil, err
	}
	persisted, err := insertSQLiteActivityConn(ctx, conn, record.Event)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return persisted, nil
}

func (s *SQLiteStore) GetAgentRequest(ctx context.Context, scope Scope, requestID string) (*AgentRequest, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM agent_requests WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
		scope.Kind, scope.ID, requestID).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeAgentRequest(payload)
}

func (s *SQLiteStore) FindAgentRequestByIdempotencyKey(ctx context.Context, scope Scope, key string) (*AgentRequest, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(key) == "" {
		return nil, nil
	}
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM agent_requests WHERE scope_kind = ? AND scope_id = ? AND idempotency_key = ?`,
		scope.Kind, scope.ID, strings.TrimSpace(key)).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeAgentRequest(payload)
}

func (s *SQLiteStore) ListAgentRequests(ctx context.Context, filter AgentRequestFilter) ([]*AgentRequest, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT payload FROM agent_requests WHERE scope_kind = ? AND scope_id = ?`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	if filter.SourceRunID != "" {
		query += ` AND source_run_id = ?`
		args = append(args, filter.SourceRunID)
	}
	if filter.Requester != nil {
		query += ` AND requester_type = ? AND requester_id = ?`
		args = append(args, filter.Requester.Type, filter.Requester.ID)
	}
	if filter.Recipient != nil {
		query += ` AND recipient_type = ? AND recipient_id = ?`
		args = append(args, filter.Recipient.Type, filter.Recipient.ID)
	}
	kinds := make([]string, 0, len(filter.Kinds))
	for _, kind := range filter.Kinds {
		kinds = append(kinds, string(kind))
	}
	query, args = appendSQLiteActivityStrings(query, args, "kind", kinds)
	statuses := make([]string, 0, len(filter.Statuses))
	for _, status := range filter.Statuses {
		statuses = append(statuses, string(status))
	}
	query, args = appendSQLiteActivityStrings(query, args, "status", statuses)
	query += ` ORDER BY updated_at DESC, id ASC LIMIT ? OFFSET ?`
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
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

func (s *SQLiteStore) RespondAgentRequest(ctx context.Context, record AgentRequestResponseRecord) ([]*ActivityEvent, error) {
	if err := validateAgentRequestResponseRecord(record); err != nil {
		return nil, err
	}
	requestPayload, err := json.Marshal(record.Request)
	if err != nil {
		return nil, err
	}
	conn, err := beginImmediateSQLite(ctx, s.db)
	if err != nil {
		return nil, err
	}
	committed := false
	defer rollbackSQLiteConn(conn, &committed)
	if record.SourceRun != nil {
		if err := updateSQLiteAgentRunConn(ctx, conn, record.SourceRun, record.ExpectedSourceRevision); err != nil {
			return nil, err
		}
		if err := insertSQLiteAgentRunConn(ctx, conn, record.ChildRun); err != nil {
			return nil, err
		}
	}
	result, err := conn.ExecContext(ctx, `UPDATE agent_requests SET status = ?, child_run_id = ?, revision = ?, updated_at = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`, record.Request.Status, record.Request.ChildRunID,
		record.Request.Revision, record.Request.UpdatedAt, string(requestPayload), record.Request.Scope.Kind, record.Request.Scope.ID,
		record.Request.ID, record.ExpectedRequestRevision)
	if err != nil {
		return nil, err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		if err != nil {
			return nil, err
		}
		return nil, ErrRevisionConflict
	}
	events := make([]*ActivityEvent, 0, 2)
	for _, event := range []*ActivityEvent{record.SourceEvent, record.ChildEvent} {
		if event == nil {
			continue
		}
		persisted, err := insertSQLiteActivityConn(ctx, conn, event)
		if err != nil {
			return nil, err
		}
		events = append(events, persisted)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return events, nil
}

func beginImmediateSQLite(ctx context.Context, db *sql.DB) (*sql.Conn, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func rollbackSQLiteConn(conn *sql.Conn, committed *bool) {
	if conn == nil {
		return
	}
	if committed == nil || !*committed {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
	}
	_ = conn.Close()
}

func insertSQLiteAgentRunConn(ctx context.Context, conn *sql.Conn, run *AgentRun) error {
	payload, err := json.Marshal(run)
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO agent_runs
		(id, scope_kind, scope_id, objective_id, parent_run_id, root_run_id, assigned_agent_id, status, priority, revision,
		 deadline, available_at, queue_entered_at, lease_owner, lease_expires_at, last_claimed_at, attempt, created_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, run.ID, run.Scope.Kind, run.Scope.ID,
		run.ObjectiveID, run.ParentRunID, run.RootRunID, run.AssignedAgentID, run.Status, run.Priority, run.Revision,
		run.Deadline, run.AvailableAt, run.QueueEnteredAt, run.LeaseOwner, run.LeaseExpiresAt, run.LastClaimedAt,
		run.Attempt, run.CreatedAt, string(payload))
	return err
}

func updateSQLiteAgentRunConn(ctx context.Context, conn *sql.Conn, run *AgentRun, expectedRevision int64) error {
	payload, err := json.Marshal(run)
	if err != nil {
		return err
	}
	result, err := conn.ExecContext(ctx, `UPDATE agent_runs SET status = ?, priority = ?, assigned_agent_id = ?, revision = ?,
		deadline = ?, available_at = ?, queue_entered_at = ?, lease_owner = ?, lease_expires_at = ?, last_claimed_at = ?, attempt = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`, run.Status, run.Priority, run.AssignedAgentID,
		run.Revision, run.Deadline, run.AvailableAt, run.QueueEnteredAt, run.LeaseOwner, run.LeaseExpiresAt, run.LastClaimedAt,
		run.Attempt, string(payload), run.Scope.Kind, run.Scope.ID, run.ID, expectedRevision)
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

func decodeAgentRequest(payload string) (*AgentRequest, error) {
	var request AgentRequest
	if err := json.Unmarshal([]byte(payload), &request); err != nil {
		return nil, fmt.Errorf("decode agent request: %w", err)
	}
	return &request, nil
}
