package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
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
	var currentLeaseOwner string
	var currentLeaseExpiry sql.NullTime
	err = conn.QueryRowContext(ctx, `SELECT revision, lease_owner, lease_expires_at FROM agent_runs
		WHERE scope_kind = ? AND scope_id = ? AND id = ?`, call.Scope.Kind, call.Scope.ID, call.RunID).
		Scan(&currentRevision, &currentLeaseOwner, &currentLeaseExpiry)
	if err == sql.ErrNoRows {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	if currentRevision != proposal.ExpectedRunRevision || proposal.Run.Revision != proposal.ExpectedRunRevision+1 {
		return nil, ErrRevisionConflict
	}
	if proposal.Lease != nil && (currentLeaseOwner != proposal.Lease.WorkerID || !currentLeaseExpiry.Valid || !currentLeaseExpiry.Time.After(proposal.Lease.Now)) {
		return nil, ErrLeaseLost
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
	return &ActionProposalResult{Call: cloneActionCall(call), Approval: cloneApprovalCheckpoint(proposal.Approval), Run: cloneAgentRun(proposal.Run), Event: event, Created: true}, nil
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
	query := `SELECT a.payload FROM approval_checkpoints a`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	if filter.Owner != nil {
		if err := filter.Owner.Validate(); err != nil {
			return nil, err
		}
		query += ` JOIN agent_runs r ON r.scope_kind = a.scope_kind AND r.scope_id = a.scope_id AND r.id = a.run_id`
	}
	query += ` WHERE a.scope_kind = ? AND a.scope_id = ?`
	if filter.Owner != nil {
		query += ` AND json_extract(r.payload, '$.owner.type') = ? AND json_extract(r.payload, '$.owner.id') = ?`
		args = append(args, filter.Owner.Type, filter.Owner.ID)
	}
	query += ` ORDER BY a.created_at ASC, a.id ASC`
	rows, err := s.db.QueryContext(ctx, query, args...)
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

func (s *SQLiteStore) ResolveApproval(ctx context.Context, resolution ApprovalResolutionRecord) (*ApprovalResolutionResult, error) {
	if err := validateApprovalResolutionRecord(resolution); err != nil {
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
	currentApproval, err := getApprovalFrom(ctx, conn, resolution.Approval.Scope, `id = ?`, resolution.Approval.ID)
	if err != nil {
		return nil, err
	}
	currentCall, err := getActionCallFrom(ctx, conn, resolution.Call.Scope, `id = ?`, resolution.Call.ID)
	if err != nil {
		return nil, err
	}
	var runPayload string
	err = conn.QueryRowContext(ctx, `SELECT payload FROM agent_runs WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
		resolution.Run.Scope.Kind, resolution.Run.Scope.ID, resolution.Run.ID).Scan(&runPayload)
	if err == sql.ErrNoRows {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	currentRun, err := decodeAgentRun(runPayload)
	if err != nil {
		return nil, err
	}
	if currentApproval.Status != ApprovalStatusPending {
		if currentApproval.DecisionID != "" && currentApproval.DecisionID == resolution.Approval.DecisionID {
			if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
				return nil, err
			}
			committed = true
			return &ApprovalResolutionResult{Approval: currentApproval, Call: currentCall, Run: currentRun, Resolved: false}, nil
		}
		return nil, ErrApprovalResolved
	}
	if currentApproval.Revision != resolution.ExpectedApprovalRevision || currentCall.Revision != resolution.ExpectedCallRevision || currentRun.Revision != resolution.ExpectedRunRevision ||
		resolution.Approval.Revision != resolution.ExpectedApprovalRevision+1 || resolution.Call.Revision != resolution.ExpectedCallRevision+1 || resolution.Run.Revision != resolution.ExpectedRunRevision+1 {
		return nil, ErrRevisionConflict
	}
	approvalPayload, err := json.Marshal(resolution.Approval)
	if err != nil {
		return nil, err
	}
	result, err := conn.ExecContext(ctx, `UPDATE approval_checkpoints SET status = ?, expires_at = ?, revision = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ? AND status = ?`,
		resolution.Approval.Status, resolution.Approval.ExpiresAt, resolution.Approval.Revision, string(approvalPayload),
		resolution.Approval.Scope.Kind, resolution.Approval.Scope.ID, resolution.Approval.ID, resolution.ExpectedApprovalRevision, ApprovalStatusPending)
	if err != nil {
		return nil, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return nil, ErrRevisionConflict
	}
	callPayload, err := json.Marshal(resolution.Call)
	if err != nil {
		return nil, err
	}
	result, err = conn.ExecContext(ctx, `UPDATE action_calls SET status = ?, available_at = ?, lease_owner = ?, lease_expires_at = ?, revision = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`,
		resolution.Call.Status, resolution.Call.AvailableAt, resolution.Call.LeaseOwner, resolution.Call.LeaseExpiresAt,
		resolution.Call.Revision, string(callPayload), resolution.Call.Scope.Kind, resolution.Call.Scope.ID,
		resolution.Call.ID, resolution.ExpectedCallRevision)
	if err != nil {
		return nil, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return nil, ErrRevisionConflict
	}
	updatedRunPayload, err := json.Marshal(resolution.Run)
	if err != nil {
		return nil, err
	}
	result, err = conn.ExecContext(ctx, `UPDATE agent_runs SET status = ?, priority = ?, assigned_agent_id = ?, revision = ?,
		deadline = ?, available_at = ?, queue_entered_at = ?, lease_owner = ?, lease_expires_at = ?, last_claimed_at = ?, attempt = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`,
		resolution.Run.Status, resolution.Run.Priority, resolution.Run.AssignedAgentID, resolution.Run.Revision,
		resolution.Run.Deadline, resolution.Run.AvailableAt, resolution.Run.QueueEnteredAt, resolution.Run.LeaseOwner,
		resolution.Run.LeaseExpiresAt, resolution.Run.LastClaimedAt, resolution.Run.Attempt, string(updatedRunPayload),
		resolution.Run.Scope.Kind, resolution.Run.Scope.ID, resolution.Run.ID, resolution.ExpectedRunRevision)
	if err != nil {
		return nil, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return nil, ErrRevisionConflict
	}
	event := cloneActivityEvent(resolution.Event)
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
	return &ApprovalResolutionResult{Approval: cloneApprovalCheckpoint(resolution.Approval), Call: cloneActionCall(resolution.Call), Run: cloneAgentRun(resolution.Run), Event: event, Resolved: true}, nil
}

func (s *SQLiteStore) ClaimNextAction(ctx context.Context, claim ActionClaim) (*ActionCall, error) {
	if err := claim.Validate(); err != nil {
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
	rows, err := conn.QueryContext(ctx, `SELECT payload FROM action_calls
		WHERE scope_kind = ? AND scope_id = ? AND status IN (?, ?)`, claim.Scope.Kind, claim.Scope.ID, ActionCallStatusReady, ActionCallStatusRunning)
	if err != nil {
		return nil, err
	}
	var selected *ActionCall
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			rows.Close()
			return nil, err
		}
		call, err := decodeActionCall(payload)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if actionEligible(call, claim.Now) && (selected == nil || actionSchedulesBefore(call, selected)) {
			selected = call
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if selected == nil {
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, err
		}
		committed = true
		return nil, nil
	}
	previousRevision := selected.Revision
	expires := claim.Now.Add(claim.LeaseDuration)
	selected.Status = ActionCallStatusRunning
	selected.LeaseOwner = claim.WorkerID
	selected.LeaseExpiresAt = &expires
	selected.Attempt++
	selected.Revision++
	selected.UpdatedAt = claim.Now
	if selected.StartedAt == nil {
		selected.StartedAt = &claim.Now
	}
	payload, err := json.Marshal(selected)
	if err != nil {
		return nil, err
	}
	result, err := conn.ExecContext(ctx, `UPDATE action_calls SET status = ?, lease_owner = ?, lease_expires_at = ?, revision = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`, selected.Status, selected.LeaseOwner,
		selected.LeaseExpiresAt, selected.Revision, string(payload), selected.Scope.Kind, selected.Scope.ID, selected.ID, previousRevision)
	if err != nil {
		return nil, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return nil, ErrRevisionConflict
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return selected, nil
}

func (s *SQLiteStore) RenewActionLease(ctx context.Context, scope Scope, actionID, workerID string, now time.Time, leaseDuration time.Duration) (*ActionCall, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(workerID) == "" || now.IsZero() || leaseDuration <= 0 {
		return nil, errors.New("worker, current time, and lease duration are required")
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
	current, err := getActionCallFrom(ctx, conn, scope, `id = ?`, actionID)
	if err != nil {
		return nil, err
	}
	if current.Status != ActionCallStatusRunning || current.LeaseOwner != workerID || current.LeaseExpiresAt == nil || !current.LeaseExpiresAt.After(now) {
		return nil, ErrLeaseLost
	}
	previousRevision := current.Revision
	expires := now.Add(leaseDuration)
	current.LeaseExpiresAt = &expires
	current.UpdatedAt = now
	current.Revision++
	payload, err := json.Marshal(current)
	if err != nil {
		return nil, err
	}
	result, err := conn.ExecContext(ctx, `UPDATE action_calls SET lease_expires_at = ?, revision = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ? AND lease_owner = ?`,
		expires, current.Revision, string(payload), scope.Kind, scope.ID, actionID, previousRevision, workerID)
	if err != nil {
		return nil, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return nil, ErrLeaseLost
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return current, nil
}

func (s *SQLiteStore) PersistActionExecution(ctx context.Context, execution ActionExecutionRecord) (*ActionExecutionResult, error) {
	if err := validateActionExecutionRecord(execution); err != nil {
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
	currentCall, err := getActionCallFrom(ctx, conn, execution.Call.Scope, `id = ?`, execution.Call.ID)
	if err != nil {
		return nil, err
	}
	if currentCall.Revision != execution.ExpectedCallRevision || execution.Call.Revision != execution.ExpectedCallRevision+1 {
		return nil, ErrRevisionConflict
	}
	if currentCall.Status != ActionCallStatusRunning || currentCall.LeaseOwner != execution.WorkerID || currentCall.LeaseExpiresAt == nil || !currentCall.LeaseExpiresAt.After(execution.Now) {
		return nil, ErrLeaseLost
	}
	callPayload, err := json.Marshal(execution.Call)
	if err != nil {
		return nil, err
	}
	result, err := conn.ExecContext(ctx, `UPDATE action_calls SET status = ?, available_at = ?, lease_owner = ?, lease_expires_at = ?, revision = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ? AND lease_owner = ?`,
		execution.Call.Status, execution.Call.AvailableAt, execution.Call.LeaseOwner, execution.Call.LeaseExpiresAt,
		execution.Call.Revision, string(callPayload), execution.Call.Scope.Kind, execution.Call.Scope.ID,
		execution.Call.ID, execution.ExpectedCallRevision, execution.WorkerID)
	if err != nil {
		return nil, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return nil, ErrLeaseLost
	}
	if execution.Run != nil {
		var currentRunPayload string
		err := conn.QueryRowContext(ctx, `SELECT payload FROM agent_runs WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
			execution.Run.Scope.Kind, execution.Run.Scope.ID, execution.Run.ID).Scan(&currentRunPayload)
		if err == sql.ErrNoRows {
			return nil, ErrRunNotFound
		}
		if err != nil {
			return nil, err
		}
		currentRun, err := decodeAgentRun(currentRunPayload)
		if err != nil {
			return nil, err
		}
		if currentRun.Revision != execution.ExpectedRunRevision || execution.Run.Revision != execution.ExpectedRunRevision+1 {
			return nil, ErrRevisionConflict
		}
		runPayload, err := json.Marshal(execution.Run)
		if err != nil {
			return nil, err
		}
		result, err = conn.ExecContext(ctx, `UPDATE agent_runs SET status = ?, priority = ?, assigned_agent_id = ?, revision = ?,
			deadline = ?, available_at = ?, queue_entered_at = ?, lease_owner = ?, lease_expires_at = ?, last_claimed_at = ?, attempt = ?, payload = ?
			WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`,
			execution.Run.Status, execution.Run.Priority, execution.Run.AssignedAgentID, execution.Run.Revision,
			execution.Run.Deadline, execution.Run.AvailableAt, execution.Run.QueueEnteredAt, execution.Run.LeaseOwner,
			execution.Run.LeaseExpiresAt, execution.Run.LastClaimedAt, execution.Run.Attempt, string(runPayload),
			execution.Run.Scope.Kind, execution.Run.Scope.ID, execution.Run.ID, execution.ExpectedRunRevision)
		if err != nil {
			return nil, err
		}
		if affected, _ := result.RowsAffected(); affected != 1 {
			return nil, ErrRevisionConflict
		}
	}
	event := cloneActivityEvent(execution.Event)
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
	return &ActionExecutionResult{Call: cloneActionCall(execution.Call), Run: cloneAgentRun(execution.Run), Event: event}, nil
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
