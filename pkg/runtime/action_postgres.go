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

func (s *PostgresStore) migrateActions(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("action_calls")+` (
			id TEXT NOT NULL, scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL,
			run_id TEXT NOT NULL, turn_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL,
			idempotency_key TEXT NOT NULL DEFAULT '', external_operation_digest TEXT NOT NULL DEFAULT '', approval_id TEXT NOT NULL DEFAULT '',
			available_at TIMESTAMPTZ NOT NULL, lease_owner TEXT NOT NULL DEFAULT '',
			lease_expires_at TIMESTAMPTZ, revision BIGINT NOT NULL, created_at TIMESTAMPTZ NOT NULL,
			payload JSONB NOT NULL, PRIMARY KEY (scope_kind, scope_id, id), CHECK (revision > 0)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("approval_checkpoints")+` (
			id TEXT NOT NULL, scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL,
			run_id TEXT NOT NULL, action_call_id TEXT NOT NULL, status TEXT NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL, revision BIGINT NOT NULL, created_at TIMESTAMPTZ NOT NULL,
			payload JSONB NOT NULL, PRIMARY KEY (scope_kind, scope_id, id),
			UNIQUE (scope_kind, scope_id, action_call_id), CHECK (revision > 0)
		)`); err != nil {
		return err
	}
	statements := []string{
		`CREATE INDEX IF NOT EXISTS action_calls_run_idx ON ` + s.table("action_calls") + ` (scope_kind, scope_id, run_id, status, created_at)`,
		`CREATE INDEX IF NOT EXISTS action_calls_runnable_idx ON ` + s.table("action_calls") + ` (scope_kind, scope_id, status, available_at, lease_expires_at)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS action_calls_idempotency_idx ON ` + s.table("action_calls") + ` (scope_kind, scope_id, run_id, idempotency_key) WHERE idempotency_key <> ''`,
		`CREATE INDEX IF NOT EXISTS approval_checkpoints_run_idx ON ` + s.table("approval_checkpoints") + ` (scope_kind, scope_id, run_id, status, created_at)`,
		`CREATE INDEX IF NOT EXISTS approval_checkpoints_expiry_idx ON ` + s.table("approval_checkpoints") + ` (scope_kind, scope_id, status, expires_at)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name) VALUES (5, 'governed actions and approvals') ON CONFLICT (version) DO NOTHING`)
	return err
}

func (s *PostgresStore) CreateActionProposal(ctx context.Context, proposal ActionProposalRecord) (*ActionProposalResult, error) {
	if err := validateActionProposalRecord(proposal); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	call := proposal.Call
	var currentRevision int64
	var currentLeaseOwner string
	var currentLeaseExpiry sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT revision, lease_owner, lease_expires_at FROM `+s.table("agent_runs")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND id = $3 FOR UPDATE`, call.Scope.Kind, call.Scope.ID, call.RunID).
		Scan(&currentRevision, &currentLeaseOwner, &currentLeaseExpiry)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	if call.IdempotencyKey != "" {
		existing, err := s.getActionByIdempotency(ctx, tx, call.Scope, call.RunID, call.IdempotencyKey)
		if err != nil && !errors.Is(err, ErrActionNotFound) {
			return nil, err
		}
		if existing != nil {
			if existing.InvocationDigest != call.InvocationDigest {
				return nil, ErrIdempotencyConflict
			}
			var approval *ApprovalCheckpoint
			if existing.ApprovalID != "" {
				approval, err = s.getApprovalByID(ctx, tx, call.Scope, existing.ApprovalID, false)
				if err != nil {
					return nil, err
				}
			}
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			return &ActionProposalResult{Call: existing, Approval: approval, Created: false}, nil
		}
	}
	if call.ExternalOperationDigest != "" && call.DuplicateOfActionCallID == "" && externalOperationProtects(call.Status) {
		if _, lockErr := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
			externalOperationLockKey(call.Scope, call.ExternalOperationDigest)); lockErr != nil {
			return nil, lockErr
		}
		existing, lookupErr := s.getActionByExternalOperation(ctx, tx, call.Scope, call.ExternalOperationDigest, true)
		if lookupErr != nil && !errors.Is(lookupErr, ErrActionNotFound) {
			return nil, lookupErr
		}
		if existing != nil {
			return nil, &ExternalOperationConflictError{Prior: existing}
		}
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("action_calls")+`
		(id, scope_kind, scope_id, run_id, turn_id, status, idempotency_key, external_operation_digest, approval_id, available_at,
		 lease_owner, lease_expires_at, revision, created_at, payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15::jsonb)`, call.ID, call.Scope.Kind, call.Scope.ID,
		call.RunID, call.TurnID, call.Status, call.IdempotencyKey, externalOperationClaimDigest(call), call.ApprovalID, call.AvailableAt, call.LeaseOwner,
		call.LeaseExpiresAt, call.Revision, call.CreatedAt, string(callPayload)); err != nil {
		return nil, err
	}
	if proposal.Approval != nil {
		approvalPayload, err := json.Marshal(proposal.Approval)
		if err != nil {
			return nil, err
		}
		approval := proposal.Approval
		if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("approval_checkpoints")+`
			(id, scope_kind, scope_id, run_id, action_call_id, status, expires_at, revision, created_at, payload)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb)`, approval.ID, approval.Scope.Kind, approval.Scope.ID,
			approval.RunID, approval.ActionCallID, approval.Status, approval.ExpiresAt, approval.Revision, approval.CreatedAt, string(approvalPayload)); err != nil {
			return nil, err
		}
	}
	if err := s.updateAgentRunTx(ctx, tx, proposal.Run, proposal.ExpectedRunRevision); err != nil {
		return nil, err
	}
	event, err := s.insertActivityTx(ctx, tx, proposal.Event)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ActionProposalResult{Call: cloneActionCall(call), Approval: cloneApprovalCheckpoint(proposal.Approval), Run: cloneAgentRun(proposal.Run), Event: event, Created: true}, nil
}

func (s *PostgresStore) GetActionCall(ctx context.Context, scope Scope, actionID string) (*ActionCall, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.getActionByID(ctx, s.db, scope, actionID, false)
}

func (s *PostgresStore) ListActionCalls(ctx context.Context, filter ActionFilter) ([]*ActionCall, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("action_calls")+` WHERE scope_kind = $1 AND scope_id = $2 ORDER BY created_at, id`, filter.Scope.Kind, filter.Scope.ID)
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
	return pageActionCalls(result, filter.Offset, filter.Limit), rows.Err()
}

func (s *PostgresStore) GetActionCallByExternalOperation(ctx context.Context, scope Scope, digest string) (*ActionCall, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := validateSHA256Digest(digest); err != nil {
		return nil, err
	}
	return s.getActionByExternalOperation(ctx, s.db, scope, digest, false)
}

func (s *PostgresStore) GetApproval(ctx context.Context, scope Scope, approvalID string) (*ApprovalCheckpoint, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.getApprovalByID(ctx, s.db, scope, approvalID, false)
}

func (s *PostgresStore) ListApprovals(ctx context.Context, filter ApprovalFilter) ([]*ApprovalCheckpoint, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT a.payload FROM ` + s.table("approval_checkpoints") + ` a`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	if filter.Owner != nil {
		if err := filter.Owner.Validate(); err != nil {
			return nil, err
		}
		query += ` JOIN ` + s.table("agent_runs") + ` r ON r.scope_kind = a.scope_kind AND r.scope_id = a.scope_id AND r.id = a.run_id`
	}
	query += ` WHERE a.scope_kind = $1 AND a.scope_id = $2`
	if filter.Owner != nil {
		query += ` AND r.payload->'owner'->>'type' = $3 AND r.payload->'owner'->>'id' = $4`
		args = append(args, filter.Owner.Type, filter.Owner.ID)
	}
	query += ` ORDER BY a.created_at, a.id`
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

func (s *PostgresStore) ResolveApproval(ctx context.Context, resolution ApprovalResolutionRecord) (*ApprovalResolutionResult, error) {
	if err := validateApprovalResolutionRecord(resolution); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var runPayload string
	err = tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("agent_runs")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 FOR UPDATE`, resolution.Run.Scope.Kind, resolution.Run.Scope.ID, resolution.Run.ID).Scan(&runPayload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	currentRun, err := decodeAgentRun(runPayload)
	if err != nil {
		return nil, err
	}
	currentApproval, err := s.getApprovalByID(ctx, tx, resolution.Approval.Scope, resolution.Approval.ID, true)
	if err != nil {
		return nil, err
	}
	currentCall, err := s.getActionByID(ctx, tx, resolution.Call.Scope, resolution.Call.ID, true)
	if err != nil {
		return nil, err
	}
	if currentApproval.Status != ApprovalStatusPending {
		if currentApproval.DecisionID != "" && currentApproval.DecisionID == resolution.Approval.DecisionID {
			if err := tx.Commit(); err != nil {
				return nil, err
			}
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
	callPayload, err := json.Marshal(resolution.Call)
	if err != nil {
		return nil, err
	}
	if err := rowsOne(tx.ExecContext(ctx, `UPDATE `+s.table("approval_checkpoints")+` SET status=$1, expires_at=$2, revision=$3, payload=$4::jsonb
		WHERE scope_kind=$5 AND scope_id=$6 AND id=$7 AND revision=$8 AND status=$9`, resolution.Approval.Status, resolution.Approval.ExpiresAt,
		resolution.Approval.Revision, string(approvalPayload), resolution.Approval.Scope.Kind, resolution.Approval.Scope.ID, resolution.Approval.ID,
		resolution.ExpectedApprovalRevision, ApprovalStatusPending)); err != nil {
		return nil, err
	}
	if err := rowsOne(tx.ExecContext(ctx, `UPDATE `+s.table("action_calls")+` SET status=$1, available_at=$2, lease_owner=$3, lease_expires_at=$4, revision=$5, payload=$6::jsonb
		WHERE scope_kind=$7 AND scope_id=$8 AND id=$9 AND revision=$10`, resolution.Call.Status, resolution.Call.AvailableAt,
		resolution.Call.LeaseOwner, resolution.Call.LeaseExpiresAt, resolution.Call.Revision, string(callPayload), resolution.Call.Scope.Kind,
		resolution.Call.Scope.ID, resolution.Call.ID, resolution.ExpectedCallRevision)); err != nil {
		return nil, err
	}
	if err := s.updateAgentRunTx(ctx, tx, resolution.Run, resolution.ExpectedRunRevision); err != nil {
		return nil, err
	}
	event, err := s.insertActivityTx(ctx, tx, resolution.Event)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ApprovalResolutionResult{Approval: cloneApprovalCheckpoint(resolution.Approval), Call: cloneActionCall(resolution.Call), Run: cloneAgentRun(resolution.Run), Event: event, Resolved: true}, nil
}

func (s *PostgresStore) ClaimNextAction(ctx context.Context, claim ActionClaim) (*ActionCall, error) {
	if err := claim.Validate(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var payload string
	err = tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("action_calls")+`
		WHERE scope_kind=$1 AND scope_id=$2 AND ((status=$3 AND available_at <= $5) OR (status=$4 AND (lease_expires_at IS NULL OR lease_expires_at <= $5)))
		ORDER BY available_at, created_at, id FOR UPDATE SKIP LOCKED LIMIT 1`, claim.Scope.Kind, claim.Scope.ID,
		ActionCallStatusReady, ActionCallStatusRunning, claim.Now).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	selected, err := decodeActionCall(payload)
	if err != nil {
		return nil, err
	}
	previousRevision := selected.Revision
	expires := claim.Now.Add(claim.LeaseDuration)
	selected.Status, selected.LeaseOwner, selected.LeaseExpiresAt = ActionCallStatusRunning, claim.WorkerID, &expires
	selected.Attempt++
	selected.Revision++
	selected.UpdatedAt = claim.Now
	if selected.StartedAt == nil {
		selected.StartedAt = &claim.Now
	}
	updatedPayload, err := json.Marshal(selected)
	if err != nil {
		return nil, err
	}
	if err := rowsOne(tx.ExecContext(ctx, `UPDATE `+s.table("action_calls")+` SET status=$1, lease_owner=$2, lease_expires_at=$3, revision=$4, payload=$5::jsonb
		WHERE scope_kind=$6 AND scope_id=$7 AND id=$8 AND revision=$9`, selected.Status, selected.LeaseOwner, selected.LeaseExpiresAt,
		selected.Revision, string(updatedPayload), selected.Scope.Kind, selected.Scope.ID, selected.ID, previousRevision)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return selected, nil
}

func (s *PostgresStore) RenewActionLease(ctx context.Context, scope Scope, actionID, workerID string, now time.Time, leaseDuration time.Duration) (*ActionCall, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(workerID) == "" || now.IsZero() || leaseDuration <= 0 {
		return nil, errors.New("worker, current time, and lease duration are required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	current, err := s.getActionByID(ctx, tx, scope, actionID, true)
	if err != nil {
		return nil, err
	}
	if current.Status != ActionCallStatusRunning || current.LeaseOwner != workerID || current.LeaseExpiresAt == nil || !current.LeaseExpiresAt.After(now) {
		return nil, ErrLeaseLost
	}
	previousRevision := current.Revision
	expires := now.Add(leaseDuration)
	current.LeaseExpiresAt, current.UpdatedAt, current.Revision = &expires, now, current.Revision+1
	payload, err := json.Marshal(current)
	if err != nil {
		return nil, err
	}
	if err := rowsOne(tx.ExecContext(ctx, `UPDATE `+s.table("action_calls")+` SET lease_expires_at=$1, revision=$2, payload=$3::jsonb
		WHERE scope_kind=$4 AND scope_id=$5 AND id=$6 AND revision=$7 AND lease_owner=$8`, expires, current.Revision,
		string(payload), scope.Kind, scope.ID, actionID, previousRevision, workerID)); err != nil {
		if errors.Is(err, ErrRevisionConflict) {
			return nil, ErrLeaseLost
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return current, nil
}

func (s *PostgresStore) PersistActionExecution(ctx context.Context, execution ActionExecutionRecord) (*ActionExecutionResult, error) {
	if err := validateActionExecutionRecord(execution); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	currentCall, err := s.getActionByID(ctx, tx, execution.Call.Scope, execution.Call.ID, true)
	if err != nil {
		return nil, err
	}
	if currentCall.Revision != execution.ExpectedCallRevision || execution.Call.Revision != execution.ExpectedCallRevision+1 {
		return nil, ErrRevisionConflict
	}
	if currentCall.Status != ActionCallStatusRunning || currentCall.LeaseOwner != execution.WorkerID || currentCall.LeaseExpiresAt == nil || !currentCall.LeaseExpiresAt.After(execution.Now) {
		return nil, ErrLeaseLost
	}
	if execution.Run != nil {
		var currentRunRevision int64
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM `+s.table("agent_runs")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 FOR UPDATE`, execution.Run.Scope.Kind, execution.Run.Scope.ID, execution.Run.ID).Scan(&currentRunRevision); errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRunNotFound
		} else if err != nil {
			return nil, err
		}
		if currentRunRevision != execution.ExpectedRunRevision || execution.Run.Revision != execution.ExpectedRunRevision+1 {
			return nil, ErrRevisionConflict
		}
	}
	callPayload, err := json.Marshal(execution.Call)
	if err != nil {
		return nil, err
	}
	if err := rowsOne(tx.ExecContext(ctx, `UPDATE `+s.table("action_calls")+` SET status=$1, available_at=$2, lease_owner=$3, lease_expires_at=$4, revision=$5, payload=$6::jsonb
		WHERE scope_kind=$7 AND scope_id=$8 AND id=$9 AND revision=$10 AND lease_owner=$11`, execution.Call.Status,
		execution.Call.AvailableAt, execution.Call.LeaseOwner, execution.Call.LeaseExpiresAt, execution.Call.Revision, string(callPayload),
		execution.Call.Scope.Kind, execution.Call.Scope.ID, execution.Call.ID, execution.ExpectedCallRevision, execution.WorkerID)); err != nil {
		if errors.Is(err, ErrRevisionConflict) {
			return nil, ErrLeaseLost
		}
		return nil, err
	}
	if execution.Run != nil {
		if err := s.updateAgentRunTx(ctx, tx, execution.Run, execution.ExpectedRunRevision); err != nil {
			return nil, err
		}
	}
	event, err := s.insertActivityTx(ctx, tx, execution.Event)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &ActionExecutionResult{Call: cloneActionCall(execution.Call), Run: cloneAgentRun(execution.Run), Event: event}, nil
}

type postgresQueryRower interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

func (s *PostgresStore) getActionByID(ctx context.Context, queryer postgresQueryRower, scope Scope, id string, lock bool) (*ActionCall, error) {
	query := `SELECT payload FROM ` + s.table("action_calls") + ` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3`
	if lock {
		query += ` FOR UPDATE`
	}
	var payload string
	if err := queryer.QueryRowContext(ctx, query, scope.Kind, scope.ID, id).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrActionNotFound
		}
		return nil, err
	}
	return decodeActionCall(payload)
}

func (s *PostgresStore) getActionByIdempotency(ctx context.Context, queryer postgresQueryRower, scope Scope, runID, key string) (*ActionCall, error) {
	var payload string
	if err := queryer.QueryRowContext(ctx, `SELECT payload FROM `+s.table("action_calls")+` WHERE scope_kind=$1 AND scope_id=$2 AND run_id=$3 AND idempotency_key=$4`, scope.Kind, scope.ID, runID, key).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrActionNotFound
		}
		return nil, err
	}
	return decodeActionCall(payload)
}

func (s *PostgresStore) getActionByExternalOperation(ctx context.Context, queryer postgresQueryRower, scope Scope, digest string, lock bool) (*ActionCall, error) {
	query := `SELECT payload FROM ` + s.table("action_calls") + `
		WHERE scope_kind=$1 AND scope_id=$2 AND external_operation_digest=$3
		AND status IN ('ready','waiting_for_approval','running','succeeded','compensating','compensated')`
	if lock {
		query += ` FOR UPDATE`
	}
	var payload string
	if err := queryer.QueryRowContext(ctx, query, scope.Kind, scope.ID, digest).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrActionNotFound
		}
		return nil, err
	}
	return decodeActionCall(payload)
}

func (s *PostgresStore) getApprovalByID(ctx context.Context, queryer postgresQueryRower, scope Scope, id string, lock bool) (*ApprovalCheckpoint, error) {
	query := `SELECT payload FROM ` + s.table("approval_checkpoints") + ` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3`
	if lock {
		query += ` FOR UPDATE`
	}
	var payload string
	if err := queryer.QueryRowContext(ctx, query, scope.Kind, scope.ID, id).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrApprovalNotFound
		}
		return nil, err
	}
	return decodeApproval(payload)
}

func (s *PostgresStore) updateAgentRunTx(ctx context.Context, tx *sql.Tx, run *AgentRun, expectedRevision int64) error {
	payload, err := json.Marshal(run)
	if err != nil {
		return err
	}
	if err := rowsOne(tx.ExecContext(ctx, `UPDATE `+s.table("agent_runs")+` SET status=$1, priority=$2, assigned_agent_id=$3, revision=$4,
		deadline=$5, available_at=$6, queue_entered_at=$7, lease_owner=$8, lease_expires_at=$9, last_claimed_at=$10, attempt=$11, payload=$12::jsonb
		WHERE scope_kind=$13 AND scope_id=$14 AND id=$15 AND revision=$16`, run.Status, run.Priority, run.AssignedAgentID, run.Revision,
		run.Deadline, run.AvailableAt, run.QueueEnteredAt, run.LeaseOwner, run.LeaseExpiresAt, run.LastClaimedAt, run.Attempt,
		string(payload), run.Scope.Kind, run.Scope.ID, run.ID, expectedRevision)); err != nil {
		return err
	}
	return nil
}

func (s *PostgresStore) insertActivityTx(ctx context.Context, tx *sql.Tx, value *ActivityEvent) (*ActivityEvent, error) {
	event := cloneActivityEvent(value)
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM `+s.table("agent_runs")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 FOR UPDATE`, event.Scope.Kind, event.Scope.ID, event.RunID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRunNotFound
		}
		return nil, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM `+s.table("run_activity")+` WHERE scope_kind=$1 AND scope_id=$2 AND run_id=$3`, event.Scope.Kind, event.Scope.ID, event.RunID).Scan(&event.Sequence); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("run_activity")+` (scope_kind,scope_id,run_id,sequence,id,event_type,created_at,payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb)`, event.Scope.Kind, event.Scope.ID, event.RunID, event.Sequence, event.ID, event.EventType, event.CreatedAt, string(payload)); err != nil {
		return nil, err
	}
	return event, nil
}

func rowsOne(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrRevisionConflict
	}
	return nil
}
