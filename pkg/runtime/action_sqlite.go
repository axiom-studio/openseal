package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
)

func migrateActions(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS action_calls (
			id TEXT NOT NULL,
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			turn_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			idempotency_key TEXT NOT NULL DEFAULT '',
			approval_id TEXT NOT NULL DEFAULT '',
			available_at DATETIME NOT NULL,
			lease_owner TEXT NOT NULL DEFAULT '',
			lease_expires_at DATETIME,
			revision INTEGER NOT NULL,
			created_at DATETIME NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id)
		);
		CREATE INDEX IF NOT EXISTS idx_action_calls_run
			ON action_calls(scope_kind, scope_id, run_id, status, created_at);
		CREATE INDEX IF NOT EXISTS idx_action_calls_runnable
			ON action_calls(scope_kind, scope_id, status, available_at, lease_expires_at);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_action_calls_idempotency
			ON action_calls(scope_kind, scope_id, run_id, idempotency_key)
			WHERE idempotency_key <> '';

		CREATE TABLE IF NOT EXISTS approval_checkpoints (
			id TEXT NOT NULL,
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			action_call_id TEXT NOT NULL,
			status TEXT NOT NULL,
			expires_at DATETIME NOT NULL,
			revision INTEGER NOT NULL,
			created_at DATETIME NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id),
			UNIQUE (scope_kind, scope_id, action_call_id)
		);
		CREATE INDEX IF NOT EXISTS idx_approval_checkpoints_run
			ON approval_checkpoints(scope_kind, scope_id, run_id, status, created_at);
		CREATE INDEX IF NOT EXISTS idx_approval_checkpoints_expiry
			ON approval_checkpoints(scope_kind, scope_id, status, expires_at);
	`)
	return err
}

func (s *SQLiteStore) CreateActionProposal(ctx context.Context, proposal ActionProposalRecord) (*ActionProposalResult, error) {
	if err := validateActionProposalRecord(proposal); err != nil {
		return nil, err
	}
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

	call := proposal.Call
	if call.IdempotencyKey != "" {
		existing, err := getActionCallFrom(ctx, conn, call.Scope, `run_id = ? AND idempotency_key = ?`, call.RunID, call.IdempotencyKey)
		if err != nil && err != ErrActionNotFound {
			return nil, err
		}
		if existing != nil {
			if existing.InvocationDigest != call.InvocationDigest {
				return nil, ErrIdempotencyConflict
			}
			var approval *ApprovalCheckpoint
			if existing.ApprovalID != "" {
				approval, err = getApprovalFrom(ctx, conn, call.Scope, `id = ?`, existing.ApprovalID)
				if err != nil {
					return nil, err
				}
			}
			if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
				return nil, err
			}
			committed = true
			return &ActionProposalResult{Call: existing, Approval: approval, Created: false}, nil
		}
	}

	var currentRevision int64
	err = conn.QueryRowContext(ctx, `SELECT revision FROM agent_runs
		WHERE scope_kind = ? AND scope_id = ? AND id = ?`, call.Scope.Kind, call.Scope.ID, call.RunID).Scan(&currentRevision)
	if err == sql.ErrNoRows {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	if currentRevision != proposal.ExpectedRunRevision || proposal.Run.Revision != proposal.ExpectedRunRevision+1 {
		return nil, ErrRevisionConflict
	}

	callPayload, err := json.Marshal(call)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO action_calls
		(id, scope_kind, scope_id, run_id, turn_id, status, idempotency_key, approval_id, available_at,
		 lease_owner, lease_expires_at, revision, created_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		call.ID, call.Scope.Kind, call.Scope.ID, call.RunID, call.TurnID, call.Status, call.IdempotencyKey,
		call.ApprovalID, call.AvailableAt, call.LeaseOwner, call.LeaseExpiresAt, call.Revision, call.CreatedAt, string(callPayload)); err != nil {
		return nil, err
	}
	if proposal.Approval != nil {
		approvalPayload, err := json.Marshal(proposal.Approval)
		if err != nil {
			return nil, err
		}
		approval := proposal.Approval
		if _, err := conn.ExecContext(ctx, `INSERT INTO approval_checkpoints
			(id, scope_kind, scope_id, run_id, action_call_id, status, expires_at, revision, created_at, payload)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, approval.ID, approval.Scope.Kind, approval.Scope.ID,
			approval.RunID, approval.ActionCallID, approval.Status, approval.ExpiresAt, approval.Revision,
			approval.CreatedAt, string(approvalPayload)); err != nil {
			return nil, err
		}
	}

	runPayload, err := json.Marshal(proposal.Run)
	if err != nil {
		return nil, err
	}
	result, err := conn.ExecContext(ctx, `UPDATE agent_runs SET status = ?, priority = ?, assigned_agent_id = ?, revision = ?,
		deadline = ?, available_at = ?, queue_entered_at = ?, lease_owner = ?, lease_expires_at = ?, last_claimed_at = ?, attempt = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`,
		proposal.Run.Status, proposal.Run.Priority, proposal.Run.AssignedAgentID, proposal.Run.Revision,
		proposal.Run.Deadline, proposal.Run.AvailableAt, proposal.Run.QueueEnteredAt, proposal.Run.LeaseOwner,
		proposal.Run.LeaseExpiresAt, proposal.Run.LastClaimedAt, proposal.Run.Attempt, string(runPayload),
		proposal.Run.Scope.Kind, proposal.Run.Scope.ID, proposal.Run.ID, proposal.ExpectedRunRevision)
	if err != nil {
		return nil, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, ErrRevisionConflict
	}

	event := cloneActivityEvent(proposal.Event)
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM run_activity
		WHERE scope_kind = ? AND scope_id = ? AND run_id = ?`, event.Scope.Kind, event.Scope.ID, event.RunID).Scan(&event.Sequence); err != nil {
		return nil, err
	}
	eventPayload, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO run_activity
		(scope_kind, scope_id, run_id, sequence, id, event_type, created_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, event.Scope.Kind, event.Scope.ID, event.RunID, event.Sequence,
		event.ID, event.EventType, event.CreatedAt, string(eventPayload)); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return &ActionProposalResult{Call: cloneActionCall(call), Approval: cloneApprovalCheckpoint(proposal.Approval), Event: event, Created: true}, nil
}

func (s *SQLiteStore) GetActionCall(ctx context.Context, scope Scope, actionID string) (*ActionCall, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return getActionCallFrom(ctx, s.db, scope, `id = ?`, actionID)
}

func (s *SQLiteStore) ListActionCalls(ctx context.Context, filter ActionFilter) ([]*ActionCall, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM action_calls
		WHERE scope_kind = ? AND scope_id = ? ORDER BY created_at ASC, id ASC`, filter.Scope.Kind, filter.Scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*ActionCall, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		call, err := decodeActionCall(payload)
		if err != nil {
			return nil, err
		}
		if (filter.RunID == "" || call.RunID == filter.RunID) && (len(filter.Status) == 0 || containsActionStatus(filter.Status, call.Status)) {
			result = append(result, call)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return pageActionCalls(result, filter.Offset, filter.Limit), nil
}

func (s *SQLiteStore) GetApproval(ctx context.Context, scope Scope, approvalID string) (*ApprovalCheckpoint, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return getApprovalFrom(ctx, s.db, scope, `id = ?`, approvalID)
}

func (s *SQLiteStore) ListApprovals(ctx context.Context, filter ApprovalFilter) ([]*ApprovalCheckpoint, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM approval_checkpoints
		WHERE scope_kind = ? AND scope_id = ? ORDER BY created_at ASC, id ASC`, filter.Scope.Kind, filter.Scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*ApprovalCheckpoint, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		approval, err := decodeApproval(payload)
		if err != nil {
			return nil, err
		}
		if (filter.RunID == "" || approval.RunID == filter.RunID) && (len(filter.Status) == 0 || containsApprovalStatus(filter.Status, approval.Status)) {
			result = append(result, approval)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return pageApprovals(result, filter.Offset, filter.Limit), nil
}

type queryRower interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

func getActionCallFrom(ctx context.Context, queryer queryRower, scope Scope, predicate string, args ...interface{}) (*ActionCall, error) {
	queryArgs := []interface{}{scope.Kind, scope.ID}
	queryArgs = append(queryArgs, args...)
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM action_calls WHERE scope_kind = ? AND scope_id = ? AND `+predicate, queryArgs...).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, ErrActionNotFound
	}
	if err != nil {
		return nil, err
	}
	return decodeActionCall(payload)
}

func getApprovalFrom(ctx context.Context, queryer queryRower, scope Scope, predicate string, args ...interface{}) (*ApprovalCheckpoint, error) {
	queryArgs := []interface{}{scope.Kind, scope.ID}
	queryArgs = append(queryArgs, args...)
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM approval_checkpoints WHERE scope_kind = ? AND scope_id = ? AND `+predicate, queryArgs...).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, ErrApprovalNotFound
	}
	if err != nil {
		return nil, err
	}
	return decodeApproval(payload)
}

func decodeActionCall(payload string) (*ActionCall, error) {
	var call ActionCall
	if err := json.Unmarshal([]byte(payload), &call); err != nil {
		return nil, err
	}
	return &call, nil
}

func decodeApproval(payload string) (*ApprovalCheckpoint, error) {
	var approval ApprovalCheckpoint
	if err := json.Unmarshal([]byte(payload), &approval); err != nil {
		return nil, err
	}
	return &approval, nil
}
