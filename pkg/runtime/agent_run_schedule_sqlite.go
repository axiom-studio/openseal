package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

func (s *SQLiteStore) ClaimNextAgentRun(ctx context.Context, claim AgentRunClaim) (*AgentRun, error) {
	decision, err := s.ClaimNextAgentRunWithDecision(ctx, claim)
	if err != nil || decision == nil {
		return nil, err
	}
	return decision.Run, nil
}

func (s *SQLiteStore) ClaimNextAgentRunWithDecision(ctx context.Context, claim AgentRunClaim) (*AgentRunAdmissionDecision, error) {
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
	rows, err := conn.QueryContext(ctx, `SELECT payload FROM agent_runs
		WHERE scope_kind = ? AND scope_id = ? AND status IN (?, ?)`,
		claim.Scope.Kind, claim.Scope.ID, AgentRunStatusQueued, AgentRunStatusRunning)
	if err != nil {
		return nil, err
	}
	candidates := make([]*AgentRun, 0)
	objectives := make(map[string]*Objective)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			rows.Close()
			return nil, err
		}
		run, err := decodeAgentRun(payload)
		if err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, run)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	objectiveRows, err := conn.QueryContext(ctx, `SELECT id, payload FROM objectives WHERE scope_kind = ? AND scope_id = ?`, claim.Scope.Kind, claim.Scope.ID)
	if err != nil {
		return nil, err
	}
	for objectiveRows.Next() {
		var id, payload string
		if err := objectiveRows.Scan(&id, &payload); err != nil {
			objectiveRows.Close()
			return nil, err
		}
		objective, err := decodeObjective(payload)
		if err != nil {
			objectiveRows.Close()
			return nil, err
		}
		objectives[id] = objective
	}
	if err := objectiveRows.Close(); err != nil {
		return nil, err
	}
	selected, decision := evaluateAgentRunAdmission(candidates, objectives, claim)
	if selected == nil {
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			return nil, err
		}
		committed = true
		return decision, nil
	}
	previousRevision := selected.Revision
	if err := applyAgentRunClaim(selected, claim); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(selected)
	if err != nil {
		return nil, err
	}
	result, err := conn.ExecContext(ctx, `UPDATE agent_runs SET status = ?, revision = ?, lease_owner = ?,
		lease_expires_at = ?, last_claimed_at = ?, attempt = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`,
		selected.Status, selected.Revision, selected.LeaseOwner, selected.LeaseExpiresAt,
		selected.LastClaimedAt, selected.Attempt, string(payload), selected.Scope.Kind, selected.Scope.ID,
		selected.ID, previousRevision)
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
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	decision.Run = selected
	decision.Outcome = AgentRunAdmissionClaimed
	if selected.Status == AgentRunStatusPaused && selected.BudgetState == BudgetStateExhausted {
		decision.Outcome = AgentRunAdmissionBudgetStopped
		decision.Blocks = []AgentRunAdmissionBlock{{
			RunID: selected.ID, ObjectiveID: selected.ObjectiveID, AssignedAgentID: selected.AssignedAgentID,
			Owner: selected.Owner, ConcurrencyKey: selected.ConcurrencyKey, Reason: AgentRunAdmissionReasonAttemptBudgetExhausted,
		}}
	}
	return decision, nil
}

func (s *SQLiteStore) RenewAgentRunLease(ctx context.Context, scope Scope, runID, workerID string, now time.Time, leaseDuration time.Duration) (*AgentRun, error) {
	if err := scope.Validate(); err != nil {
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
	var payload string
	err = conn.QueryRowContext(ctx, `SELECT payload FROM agent_runs WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
		scope.Kind, scope.ID, runID).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	run, err := decodeAgentRun(payload)
	if err != nil {
		return nil, err
	}
	if run.Status != AgentRunStatusRunning || run.LeaseOwner != workerID || run.LeaseExpiresAt == nil || !run.LeaseExpiresAt.After(now) {
		return nil, ErrLeaseLost
	}
	previousRevision := run.Revision
	expires := now.Add(leaseDuration)
	run.LeaseExpiresAt = &expires
	run.UpdatedAt = now
	run.Revision++
	updatedPayload, err := json.Marshal(run)
	if err != nil {
		return nil, err
	}
	result, err := conn.ExecContext(ctx, `UPDATE agent_runs SET revision = ?, lease_expires_at = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ? AND lease_owner = ?`,
		run.Revision, expires, string(updatedPayload), scope.Kind, scope.ID, runID, previousRevision, workerID)
	if err != nil {
		return nil, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if affected != 1 {
		return nil, ErrLeaseLost
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return run, nil
}
