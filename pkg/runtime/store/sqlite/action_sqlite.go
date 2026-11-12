package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
	"github.com/axiom-studio/openseal/pkg/skill"
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
			external_operation_digest TEXT NOT NULL DEFAULT '',
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
	if err != nil {
		return err
	}
	columns, err := sqliteTableColumns(db, "action_calls")
	if err != nil {
		return err
	}
	if !columns["external_operation_digest"] {
		if _, err := db.Exec(`ALTER TABLE action_calls ADD COLUMN external_operation_digest TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	_, err = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_action_calls_external_operation
		ON action_calls(scope_kind, scope_id, external_operation_digest)
		WHERE external_operation_digest <> '' AND status IN ('ready','waiting_for_approval','running','succeeded','compensating','compensated')`)
	return err
}

func (s *Store) CreateActionProposal(ctx context.Context, proposal models.ActionProposalRecord) (*models.ActionProposalResult, error) {
	if err := models.ValidateActionProposalRecord(proposal); err != nil {
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
		if err != nil && err != models.ErrActionNotFound {
			return nil, err
		}
		if existing != nil {
			if existing.InvocationDigest != call.InvocationDigest {
				return nil, models.ErrIdempotencyConflict
			}
			var approval *models.ApprovalCheckpoint
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
			return &models.ActionProposalResult{Call: existing, Approval: approval, Created: false}, nil
		}
	}
	if gate, err := sqliteSkillRuntimeMaintenanceActiveTx(ctx, conn, call.Scope, call.SkillID); err != nil {
		return nil, err
	} else if gate != nil {
		return nil, &models.SkillRuntimeMaintenanceError{Maintenance: *gate}
	}
	if proposal.RequireBindingFence {
		var bindingPayload string
		err := conn.QueryRowContext(ctx, `SELECT payload FROM skill_bindings
			WHERE scope_kind = ? AND scope_id = ? AND deployment_id = ? AND id = ?`,
			call.Scope.Kind, call.Scope.ID, call.DeploymentID, call.BindingID).Scan(&bindingPayload)
		if errors.Is(err, sql.ErrNoRows) {
			if err := models.ValidateActionSubmissionBinding(call, proposal.HostBinding); err != nil {
				return nil, err
			}
		} else if err != nil {
			return nil, err
		} else {
			var binding skill.Binding
			if err := json.Unmarshal([]byte(bindingPayload), &binding); err != nil {
				return nil, err
			}
			if proposal.HostBinding != nil {
				return nil, skill.ErrBindingAmbiguous
			}
			if err := models.ValidateActionSubmissionBinding(call, &binding); err != nil {
				return nil, err
			}
		}
	}
	if call.ExternalOperationDigest != "" && call.DuplicateOfActionCallID == "" && models.ExternalOperationProtects(call.Status) {
		existing, lookupErr := getActionCallFrom(ctx, conn, call.Scope,
			`external_operation_digest = ? AND status IN ('ready','waiting_for_approval','running','succeeded','compensating','compensated')`, call.ExternalOperationDigest)
		if lookupErr != nil && lookupErr != models.ErrActionNotFound {
			return nil, lookupErr
		}
		if existing != nil {
			return nil, &models.ExternalOperationConflictError{Prior: existing}
		}
	}

	var currentRevision int64
	var currentStatus, currentLeaseOwner string
	var currentLeaseExpiry sql.NullTime
	err = conn.QueryRowContext(ctx, `SELECT revision, status, lease_owner, lease_expires_at FROM agent_runs
		WHERE scope_kind = ? AND scope_id = ? AND id = ?`, call.Scope.Kind, call.Scope.ID, call.RunID).
		Scan(&currentRevision, &currentStatus, &currentLeaseOwner, &currentLeaseExpiry)
	if err == sql.ErrNoRows {
		return nil, models.ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	if proposal.Nested != nil {
		state := models.NestedActionTurnState{RunStatus: models.AgentRunStatus(currentStatus), RunLeaseOwner: currentLeaseOwner}
		if currentLeaseExpiry.Valid {
			state.RunLeaseExpiry = &currentLeaseExpiry.Time
		}
		turn := &models.AgentTurn{}
		var turnLeaseExpiry sql.NullTime
		err := conn.QueryRowContext(ctx, `SELECT run_id, status, lease_owner, lease_expires_at FROM agent_turns
			WHERE scope_kind = ? AND scope_id = ? AND id = ?`, call.Scope.Kind, call.Scope.ID, call.TurnID).
			Scan(&turn.RunID, &turn.Status, &turn.LeaseOwner, &turnLeaseExpiry)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		if err == nil {
			if turnLeaseExpiry.Valid {
				turn.LeaseExpiresAt = &turnLeaseExpiry.Time
			}
			state.Turn = turn
		}
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(CASE WHEN status IN (?, ?, ?) THEN 1 ELSE 0 END), 0) FROM action_calls
			WHERE scope_kind = ? AND scope_id = ? AND run_id = ? AND turn_id = ? AND json_extract(payload, '$.origin') = ?`,
			models.NestedActionUnfinishedStatuses[0], models.NestedActionUnfinishedStatuses[1], models.NestedActionUnfinishedStatuses[2],
			call.Scope.Kind, call.Scope.ID, call.RunID, call.TurnID, string(models.ActionOriginVM)).Scan(&state.TotalCalls, &state.UnfinishedCalls); err != nil {
			return nil, err
		}
		if err := proposal.Nested.Admit(call, state); err != nil {
			return nil, err
		}
	} else {
		if call.Origin == models.ActionOriginLoop && models.AgentRunStatus(currentStatus) != models.AgentRunStatusRunning {
			return nil, models.ErrLeaseLost
		}
		if proposal.Run != nil && (currentRevision != proposal.ExpectedRunRevision || proposal.Run.Revision != proposal.ExpectedRunRevision+1) {
			return nil, models.ErrRevisionConflict
		}
	}
	if proposal.Lease != nil && (currentLeaseOwner != proposal.Lease.WorkerID || !currentLeaseExpiry.Valid || !currentLeaseExpiry.Time.After(proposal.Lease.Now)) {
		return nil, models.ErrLeaseLost
	}

	if err := insertSQLiteActionCall(ctx, conn, call); err != nil {
		return nil, err
	}
	if proposal.Approval != nil {
		if err := insertSQLiteApprovalCheckpoint(ctx, conn, proposal.Approval); err != nil {
			return nil, err
		}
	}

	if proposal.Run != nil {
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
			return nil, models.ErrRevisionConflict
		}
	}

	event := models.CloneActivityEvent(proposal.Event)
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
	return &models.ActionProposalResult{Call: models.CloneActionCall(call), Approval: models.CloneApprovalCheckpoint(proposal.Approval), Run: models.CloneAgentRun(proposal.Run), Event: event, Created: true}, nil
}

func (s *Store) GetActionCall(ctx context.Context, scope models.Scope, actionID string) (*models.ActionCall, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return getActionCallFrom(ctx, s.db, scope, `id = ?`, actionID)
}

func (s *Store) ListActionCalls(ctx context.Context, filter models.ActionFilter) ([]*models.ActionCall, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM action_calls
		WHERE scope_kind = ? AND scope_id = ? ORDER BY created_at ASC, id ASC`, filter.Scope.Kind, filter.Scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*models.ActionCall, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		call, err := sqlstore.DecodeActionCall(payload)
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

func (s *Store) GetActionCallByExternalOperation(ctx context.Context, scope models.Scope, digest string) (*models.ActionCall, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if err := models.ValidateSHA256Digest(digest); err != nil {
		return nil, err
	}
	return getActionCallFrom(ctx, s.db, scope,
		`external_operation_digest = ? AND status IN ('ready','waiting_for_approval','running','succeeded','compensating','compensated')`, digest)
}

func (s *Store) GetActionCallByIdempotencyKey(ctx context.Context, scope models.Scope, runID, key string) (*models.ActionCall, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(runID) == "" || key == "" {
		return nil, models.ErrActionNotFound
	}
	return getActionCallFrom(ctx, s.db, scope, `run_id = ? AND idempotency_key = ?`, runID, key)
}

func (s *Store) GetApproval(ctx context.Context, scope models.Scope, approvalID string) (*models.ApprovalCheckpoint, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return getApprovalFrom(ctx, s.db, scope, `id = ?`, approvalID)
}

func (s *Store) ListApprovals(ctx context.Context, filter models.ApprovalFilter) ([]*models.ApprovalCheckpoint, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	query := `SELECT a.payload FROM approval_checkpoints a`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	if filter.Owner != nil {
		if err := filter.Owner.Validate(); err != nil {
			return nil, err
		}
	}
	if filter.Owner != nil || filter.ConversationID != "" {
		query += ` JOIN agent_runs r ON r.scope_kind = a.scope_kind AND r.scope_id = a.scope_id AND r.id = a.run_id`
	}
	var conversationExpr, triggerExpr string
	if filter.ConversationID != "" {
		query += ` LEFT JOIN agent_runs root ON root.scope_kind = r.scope_kind AND root.scope_id = r.scope_id AND root.id = r.root_run_id`
		conversationExpr = `COALESCE(NULLIF(json_extract(r.payload, '$.context.conversationId'), ''), json_extract(root.payload, '$.context.conversationId'), '')`
		triggerExpr = `COALESCE(NULLIF(json_extract(r.payload, '$.context.triggerMessageId'), ''), json_extract(root.payload, '$.context.triggerMessageId'), '')`
		query = strings.Replace(query, "SELECT a.payload", "SELECT a.payload, "+conversationExpr+", "+triggerExpr, 1)
	}
	query += ` WHERE a.scope_kind = ? AND a.scope_id = ?`
	if filter.Owner != nil {
		ownerType, ownerID := `json_extract(r.payload, '$.owner.type')`, `json_extract(r.payload, '$.owner.id')`
		if filter.ConversationID != "" {
			ownerType, ownerID = `COALESCE(json_extract(root.payload, '$.owner.type'), json_extract(r.payload, '$.owner.type'))`, `COALESCE(json_extract(root.payload, '$.owner.id'), json_extract(r.payload, '$.owner.id'))`
		}
		query += " AND " + ownerType + " = ? AND " + ownerID + " = ?"
		args = append(args, filter.Owner.Type, filter.Owner.ID)
	}
	if filter.ConversationID != "" {
		args = append(args, filter.ConversationID)
		query += " AND (" + conversationExpr + " = ?"
		if filter.IncludeUnscoped {
			query += " OR " + conversationExpr + " = ''"
		}
		query += ")"
	}
	if filter.RunIDs != nil {
		if len(filter.RunIDs) == 0 {
			return []*models.ApprovalCheckpoint{}, nil
		}
		query += ` AND a.run_id IN (?` + strings.Repeat(`,?`, len(filter.RunIDs)-1) + `)`
		for _, id := range filter.RunIDs {
			args = append(args, id)
		}
	}
	if filter.NewestFirst {
		query += ` ORDER BY a.created_at DESC, a.id DESC`
	} else {
		query += ` ORDER BY a.created_at ASC, a.id ASC`
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*models.ApprovalCheckpoint, 0)
	for rows.Next() {
		var payload string
		projection := &models.ApprovalConversationContext{}
		var scanErr error
		if filter.ConversationID != "" {
			scanErr = rows.Scan(&payload, &projection.ConversationID, &projection.TriggerMessageID)
		} else {
			scanErr = rows.Scan(&payload)
		}
		if scanErr != nil {
			return nil, scanErr
		}
		approval, err := sqlstore.DecodeApproval(payload)
		if err != nil {
			return nil, err
		}
		if filter.ConversationID != "" {
			approval.ConversationContext = projection
		}
		if (filter.RunID == "" || approval.RunID == filter.RunID) && (len(filter.Status) == 0 || containsApprovalStatus(filter.Status, approval.Status)) {
			result = append(result, approval)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(result, func(i, j int) bool {
		if filter.NewestFirst {
			return result[i].CreatedAt.After(result[j].CreatedAt)
		}
		return result[i].CreatedAt.Before(result[j].CreatedAt)
	})
	return pageApprovals(result, filter.Offset, filter.Limit), nil
}

func (s *Store) ResolveApproval(ctx context.Context, resolution models.ApprovalResolutionRecord) (*models.ApprovalResolutionResult, error) {
	if err := models.ValidateApprovalResolutionRecord(resolution); err != nil {
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
		resolution.Approval.Scope.Kind, resolution.Approval.Scope.ID, resolution.Approval.RunID).Scan(&runPayload)
	if err == sql.ErrNoRows {
		return nil, models.ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	currentRun, err := sqlstore.DecodeAgentRun(runPayload)
	if err != nil {
		return nil, err
	}
	if currentApproval.Status != models.ApprovalStatusPending {
		if currentApproval.DecisionID != "" && currentApproval.DecisionID == resolution.Approval.DecisionID {
			if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
				return nil, err
			}
			committed = true
			return &models.ApprovalResolutionResult{Approval: currentApproval, Call: currentCall, Run: currentRun, Resolved: false}, nil
		}
		return nil, models.ErrApprovalResolved
	}
	if currentApproval.Revision != resolution.ExpectedApprovalRevision || currentCall.Revision != resolution.ExpectedCallRevision ||
		resolution.Approval.Revision != resolution.ExpectedApprovalRevision+1 || resolution.Call.Revision != resolution.ExpectedCallRevision+1 ||
		resolution.Run != nil && (currentRun.Revision != resolution.ExpectedRunRevision || resolution.Run.Revision != resolution.ExpectedRunRevision+1) {
		return nil, models.ErrRevisionConflict
	}
	approvalPayload, err := json.Marshal(resolution.Approval)
	if err != nil {
		return nil, err
	}
	result, err := conn.ExecContext(ctx, `UPDATE approval_checkpoints SET status = ?, expires_at = ?, revision = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ? AND status = ?`,
		resolution.Approval.Status, resolution.Approval.ExpiresAt, resolution.Approval.Revision, string(approvalPayload),
		resolution.Approval.Scope.Kind, resolution.Approval.Scope.ID, resolution.Approval.ID, resolution.ExpectedApprovalRevision, models.ApprovalStatusPending)
	if err != nil {
		return nil, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return nil, models.ErrRevisionConflict
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
		return nil, models.ErrRevisionConflict
	}
	resolvedRun := currentRun
	if resolution.Run != nil {
		resolvedRun = resolution.Run
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
			return nil, models.ErrRevisionConflict
		}
	}
	event := models.CloneActivityEvent(resolution.Event)
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
	return &models.ApprovalResolutionResult{Approval: models.CloneApprovalCheckpoint(resolution.Approval), Call: models.CloneActionCall(resolution.Call), Run: models.CloneAgentRun(resolvedRun), Event: event, Resolved: true}, nil
}

func (s *Store) ClaimNextAction(ctx context.Context, claim models.ActionClaim) (*models.ActionCall, error) {
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
	var selectedPayload string
	err = conn.QueryRowContext(ctx, `SELECT c.payload FROM action_calls c
		WHERE c.scope_kind = ? AND c.scope_id = ? AND
		((c.status = ? AND c.available_at <= ?) OR (c.status = ? AND (c.lease_expires_at IS NULL OR c.lease_expires_at <= ?)))
		AND NOT EXISTS (SELECT 1 FROM agent_runs r WHERE r.scope_kind = c.scope_kind AND r.scope_id = c.scope_id AND r.id = c.run_id
			AND r.status = 'paused' AND json_extract(r.payload, '$.pausedFrom') = 'waiting_for_dependency'
			AND json_extract(r.payload, '$.pausedWakeCondition.type') = 'action'
			AND json_extract(r.payload, '$.pausedWakeCondition.reference') = c.id)
		AND NOT EXISTS (SELECT 1 FROM skill_runtime_maintenance m WHERE m.scope_kind = c.scope_kind AND m.scope_id = c.scope_id
			AND m.skill_id = c.skill_id AND m.active = 1)
		AND (CASE WHEN ? <> '' THEN c.id = ? AND json_extract(c.payload, '$.execution') IN (?, ?)
			ELSE NOT (COALESCE(json_extract(c.payload, '$.execution'), '') = ? AND c.status = ? AND c.available_at > ?)
				AND NOT (COALESCE(json_extract(c.payload, '$.execution'), '') = ? AND EXISTS (SELECT 1 FROM agent_runs r2
					WHERE r2.scope_kind = c.scope_kind AND r2.scope_id = c.scope_id AND r2.id = c.run_id
					AND r2.status NOT IN ('completed', 'failed', 'canceled'))) END)
		ORDER BY c.available_at, c.created_at, c.id LIMIT 1`, claim.Scope.Kind, claim.Scope.ID,
		models.ActionCallStatusReady, claim.Now, models.ActionCallStatusRunning, claim.Now,
		claim.OwnActionID, claim.OwnActionID, string(models.ActionExecutionHost), string(models.ActionExecutionLoop),
		string(models.ActionExecutionHost), models.ActionCallStatusReady, claim.HostReadyCutoff(),
		string(models.ActionExecutionLoop)).Scan(&selectedPayload)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, err
		}
		committed = true
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	selected, err := sqlstore.DecodeActionCall(selectedPayload)
	if err != nil {
		return nil, err
	}
	previousRevision := selected.Revision
	expires := claim.Now.Add(claim.LeaseDuration)
	selected.RecoveredRunning = selected.RecoveredRunning || selected.Status == models.ActionCallStatusRunning
	selected.Status = models.ActionCallStatusRunning
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
		return nil, models.ErrRevisionConflict
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return selected, nil
}

func (s *Store) RenewActionLease(ctx context.Context, scope models.Scope, actionID, workerID string, now time.Time, leaseDuration time.Duration) (*models.ActionCall, error) {
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
	if current.Status != models.ActionCallStatusRunning || current.LeaseOwner != workerID || current.LeaseExpiresAt == nil || !current.LeaseExpiresAt.After(now) {
		return nil, models.ErrLeaseLost
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
		return nil, models.ErrLeaseLost
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return current, nil
}

func (s *Store) PersistActionExecution(ctx context.Context, execution models.ActionExecutionRecord) (*models.ActionExecutionResult, error) {
	if err := models.ValidateActionExecutionRecord(execution); err != nil {
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
	if currentCall.RunID != execution.Call.RunID {
		return nil, models.ErrInvalidScope
	}
	if currentCall.Revision != execution.ExpectedCallRevision || execution.Call.Revision != execution.ExpectedCallRevision+1 {
		return nil, models.ErrRevisionConflict
	}
	if currentCall.Status != models.ActionCallStatusRunning || currentCall.LeaseOwner != execution.WorkerID || currentCall.LeaseExpiresAt == nil || !currentCall.LeaseExpiresAt.After(execution.Now) {
		return nil, models.ErrLeaseLost
	}
	var currentRunRevision int64
	err = conn.QueryRowContext(ctx, `SELECT revision FROM agent_runs WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
		currentCall.Scope.Kind, currentCall.Scope.ID, currentCall.RunID).Scan(&currentRunRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, models.ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	if currentRunRevision != execution.ExpectedRunRevision || execution.Run != nil && execution.Run.Revision != execution.ExpectedRunRevision+1 {
		return nil, models.ErrRevisionConflict
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
		return nil, models.ErrLeaseLost
	}
	if execution.Run != nil {
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
			return nil, models.ErrRevisionConflict
		}
	}
	if execution.CreatedFromBuilderConversationID != "" {
		if _, err := conn.ExecContext(ctx, `UPDATE conversations SET builder_draft_requester = NULL WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
			execution.Call.Scope.Kind, execution.Call.Scope.ID, execution.CreatedFromBuilderConversationID); err != nil {
			return nil, err
		}
	}
	event := models.CloneActivityEvent(execution.Event)
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
	return &models.ActionExecutionResult{Call: models.CloneActionCall(execution.Call), Run: models.CloneAgentRun(execution.Run), Event: event}, nil
}

func getActionCallFrom(ctx context.Context, queryer sqlstore.RowQuerier, scope models.Scope, predicate string, args ...interface{}) (*models.ActionCall, error) {
	queryArgs := []interface{}{scope.Kind, scope.ID}
	queryArgs = append(queryArgs, args...)
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM action_calls WHERE scope_kind = ? AND scope_id = ? AND `+predicate, queryArgs...).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, models.ErrActionNotFound
	}
	if err != nil {
		return nil, err
	}
	return sqlstore.DecodeActionCall(payload)
}

func getApprovalFrom(ctx context.Context, queryer sqlstore.RowQuerier, scope models.Scope, predicate string, args ...interface{}) (*models.ApprovalCheckpoint, error) {
	queryArgs := []interface{}{scope.Kind, scope.ID}
	queryArgs = append(queryArgs, args...)
	var payload string
	err := queryer.QueryRowContext(ctx, `SELECT payload FROM approval_checkpoints WHERE scope_kind = ? AND scope_id = ? AND `+predicate, queryArgs...).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, models.ErrApprovalNotFound
	}
	if err != nil {
		return nil, err
	}
	return sqlstore.DecodeApproval(payload)
}

func insertSQLiteActionCall(ctx context.Context, exec sqlstore.Execer, call *models.ActionCall) error {
	payload, err := json.Marshal(call)
	if err != nil {
		return err
	}
	_, err = exec.ExecContext(ctx, `INSERT INTO action_calls
		(id, scope_kind, scope_id, run_id, turn_id, status, idempotency_key, external_operation_digest, approval_id, available_at,
		 lease_owner, lease_expires_at, revision, created_at, payload, deployment_id, binding_id, binding_revision, skill_id, skill_version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		call.ID, call.Scope.Kind, call.Scope.ID, call.RunID, call.TurnID, call.Status, call.IdempotencyKey,
		models.ExternalOperationClaimDigest(call), call.ApprovalID, call.AvailableAt, call.LeaseOwner, call.LeaseExpiresAt, call.Revision, call.CreatedAt, string(payload),
		call.DeploymentID, call.BindingID, call.BindingRevision, call.SkillID, call.SkillVersion)
	return err
}

func insertSQLiteApprovalCheckpoint(ctx context.Context, exec sqlstore.Execer, approval *models.ApprovalCheckpoint) error {
	payload, err := json.Marshal(approval)
	if err != nil {
		return err
	}
	_, err = exec.ExecContext(ctx, `INSERT INTO approval_checkpoints
		(id, scope_kind, scope_id, run_id, action_call_id, status, expires_at, revision, created_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, approval.ID, approval.Scope.Kind, approval.Scope.ID,
		approval.RunID, approval.ActionCallID, approval.Status, approval.ExpiresAt, approval.Revision,
		approval.CreatedAt, string(payload))
	return err
}
