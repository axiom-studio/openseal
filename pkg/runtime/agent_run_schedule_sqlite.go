package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

func (s *SQLiteStore) ClaimNextAgentRun(ctx context.Context, claim AgentRunClaim) (*AgentRun, error) {
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
	activeByAgent := make(map[string]int)
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
		if claim.MaxActiveForAgent > 0 && run.Status == AgentRunStatusRunning && run.AssignedAgentID != "" &&
			run.LeaseExpiresAt != nil && run.LeaseExpiresAt.After(claim.Now) {
			activeByAgent[run.AssignedAgentID]++
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var selected *AgentRun
	for _, run := range candidates {
		if !agentRunEligible(run, claim) {
			continue
		}
		if claim.MaxActiveForAgent > 0 && run.AssignedAgentID != "" && activeByAgent[run.AssignedAgentID] >= claim.MaxActiveForAgent {
			continue
		}
		if selected == nil || agentRunSchedulesBefore(run, selected, claim.Now, claim.AgingInterval) {
			selected = run
		}
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
	selected.Status = AgentRunStatusRunning
	selected.LeaseOwner = claim.WorkerID
	selected.LeaseExpiresAt = &expires
	selected.LastClaimedAt = &claim.Now
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
	return selected, nil
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
