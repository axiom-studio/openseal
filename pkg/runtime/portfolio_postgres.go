package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

func (s *PostgresStore) migratePortfolio(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("objectives")+` (
			id TEXT NOT NULL,
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			owner_type TEXT NOT NULL,
			owner_id TEXT NOT NULL,
			status TEXT NOT NULL,
			priority INTEGER NOT NULL DEFAULT 0,
			revision BIGINT NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL,
			payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("agent_runs")+` (
			id TEXT NOT NULL,
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			objective_id TEXT NOT NULL DEFAULT '',
			parent_run_id TEXT NOT NULL DEFAULT '',
			root_run_id TEXT NOT NULL,
			assigned_agent_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			priority INTEGER NOT NULL DEFAULT 0,
			revision BIGINT NOT NULL DEFAULT 1,
			deadline TIMESTAMPTZ,
			available_at TIMESTAMPTZ NOT NULL,
			queue_entered_at TIMESTAMPTZ NOT NULL,
			lease_owner TEXT NOT NULL DEFAULT '',
			lease_expires_at TIMESTAMPTZ,
			last_claimed_at TIMESTAMPTZ,
			attempt INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMPTZ NOT NULL,
			payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id),
			CHECK (revision > 0),
			CHECK (attempt >= 0)
		)`); err != nil {
		return err
	}
	statements := []string{
		`CREATE INDEX IF NOT EXISTS objectives_portfolio_idx ON ` + s.table("objectives") + ` (scope_kind, scope_id, owner_type, owner_id, status, priority, updated_at)`,
		`CREATE INDEX IF NOT EXISTS agent_runs_objective_idx ON ` + s.table("agent_runs") + ` (scope_kind, scope_id, objective_id, status, priority, created_at)`,
		`CREATE INDEX IF NOT EXISTS agent_runs_lineage_idx ON ` + s.table("agent_runs") + ` (scope_kind, scope_id, root_run_id, parent_run_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS agent_runs_runnable_idx ON ` + s.table("agent_runs") + ` (scope_kind, scope_id, status, available_at, lease_expires_at, priority, queue_entered_at)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name) VALUES (2, 'objective portfolios and agent runs') ON CONFLICT (version) DO NOTHING`)
	return err
}

func (s *PostgresStore) CreateObjective(ctx context.Context, objective *Objective) error {
	if err := objective.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(objective)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO `+s.table("objectives")+`
		(id, scope_kind, scope_id, owner_type, owner_id, status, priority, revision, updated_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::jsonb)`,
		objective.ID, objective.Scope.Kind, objective.Scope.ID, objective.Owner.Type, objective.Owner.ID,
		objective.Status, objective.Priority, objective.Revision, objective.UpdatedAt, string(payload))
	return err
}

func (s *PostgresStore) GetObjective(ctx context.Context, scope Scope, objectiveID string) (*Objective, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("objectives")+` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`, scope.Kind, scope.ID, objectiveID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeObjective(payload)
}

func (s *PostgresStore) ListObjectives(ctx context.Context, filter ObjectiveFilter) ([]*Objective, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("objectives")+` WHERE scope_kind = $1 AND scope_id = $2`, filter.Scope.Kind, filter.Scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*Objective, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		objective, err := decodeObjective(payload)
		if err != nil {
			return nil, err
		}
		if matchesObjectiveFilter(objective, filter) {
			result = append(result, objective)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Priority != result[j].Priority {
			return result[i].Priority > result[j].Priority
		}
		if !result[i].UpdatedAt.Equal(result[j].UpdatedAt) {
			return result[i].UpdatedAt.After(result[j].UpdatedAt)
		}
		return result[i].ID < result[j].ID
	})
	return pageObjectives(result, filter.Offset, filter.Limit), nil
}

func (s *PostgresStore) UpdateObjective(ctx context.Context, objective *Objective, expectedRevision int64) error {
	if err := objective.Validate(); err != nil {
		return err
	}
	if objective.Revision != expectedRevision+1 {
		return ErrRevisionConflict
	}
	payload, err := json.Marshal(objective)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.table("objectives")+`
		SET owner_type = $1, owner_id = $2, status = $3, priority = $4, revision = $5, updated_at = $6, payload = $7::jsonb
		WHERE scope_kind = $8 AND scope_id = $9 AND id = $10 AND revision = $11`,
		objective.Owner.Type, objective.Owner.ID, objective.Status, objective.Priority, objective.Revision,
		objective.UpdatedAt, string(payload), objective.Scope.Kind, objective.Scope.ID, objective.ID, expectedRevision)
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

func (s *PostgresStore) CreateAgentRun(ctx context.Context, run *AgentRun) error {
	if err := run.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(run)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO `+s.table("agent_runs")+`
		(id, scope_kind, scope_id, objective_id, parent_run_id, root_run_id, assigned_agent_id, status, priority, revision,
		 deadline, available_at, queue_entered_at, lease_owner, lease_expires_at, last_claimed_at, attempt, created_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19::jsonb)`,
		run.ID, run.Scope.Kind, run.Scope.ID, run.ObjectiveID, run.ParentRunID, run.RootRunID,
		run.AssignedAgentID, run.Status, run.Priority, run.Revision, run.Deadline, run.AvailableAt,
		run.QueueEnteredAt, run.LeaseOwner, run.LeaseExpiresAt, run.LastClaimedAt, run.Attempt, run.CreatedAt, string(payload))
	return err
}

func (s *PostgresStore) GetAgentRun(ctx context.Context, scope Scope, runID string) (*AgentRun, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("agent_runs")+` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`, scope.Kind, scope.ID, runID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeAgentRun(payload)
}

func (s *PostgresStore) ListAgentRuns(ctx context.Context, filter AgentRunFilter) ([]*AgentRun, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("agent_runs")+` WHERE scope_kind = $1 AND scope_id = $2`, filter.Scope.Kind, filter.Scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*AgentRun, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		run, err := decodeAgentRun(payload)
		if err != nil {
			return nil, err
		}
		if matchesRunFilter(run, filter) {
			result = append(result, run)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Priority != result[j].Priority {
			return result[i].Priority > result[j].Priority
		}
		if !result[i].CreatedAt.Equal(result[j].CreatedAt) {
			return result[i].CreatedAt.Before(result[j].CreatedAt)
		}
		return result[i].ID < result[j].ID
	})
	return pageAgentRuns(result, filter.Offset, filter.Limit), nil
}

func (s *PostgresStore) ClaimNextAgentRun(ctx context.Context, claim AgentRunClaim) (*AgentRun, error) {
	if err := claim.Validate(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if claim.MaxActiveForAgent > 0 || claim.MaxActiveForConcurrencyKey > 0 {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "openseal:agent-claim:"+claim.Scope.Kind+":"+claim.Scope.ID); err != nil {
			return nil, err
		}
	}
	agingSeconds := claim.AgingInterval.Seconds()
	var payload string
	err = tx.QueryRowContext(ctx, `SELECT candidate.payload FROM `+s.table("agent_runs")+` AS candidate
		WHERE candidate.scope_kind = $1 AND candidate.scope_id = $2
		AND ($3 = '' OR candidate.assigned_agent_id = $3)
		AND ($9 = '' OR COALESCE(candidate.payload->>'kind', 'agent_work') = $9)
		AND ((candidate.status = $4 AND candidate.available_at <= $6 AND (candidate.lease_expires_at IS NULL OR candidate.lease_expires_at <= $6))
		  OR (candidate.status = $5 AND (candidate.lease_expires_at IS NULL OR candidate.lease_expires_at <= $6)))
		AND ($7 = 0 OR candidate.assigned_agent_id = '' OR (
			SELECT COUNT(*) FROM `+s.table("agent_runs")+` AS active
			WHERE active.scope_kind = candidate.scope_kind AND active.scope_id = candidate.scope_id
			AND active.assigned_agent_id = candidate.assigned_agent_id AND active.status = $5
			AND active.lease_expires_at IS NOT NULL AND active.lease_expires_at > $6
		) < $7)
		AND ($10 = 0 OR COALESCE(candidate.payload->>'concurrencyKey', '') = '' OR (
			SELECT COUNT(*) FROM `+s.table("agent_runs")+` AS active
			WHERE active.scope_kind = candidate.scope_kind AND active.scope_id = candidate.scope_id
			AND COALESCE(active.payload->>'concurrencyKey', '') = COALESCE(candidate.payload->>'concurrencyKey', '')
			AND active.status = $5 AND active.lease_expires_at IS NOT NULL AND active.lease_expires_at > $6
		) < $10)
		ORDER BY candidate.priority + FLOOR(GREATEST(EXTRACT(EPOCH FROM ($6 - candidate.queue_entered_at)), 0) / $8) DESC,
			candidate.deadline ASC NULLS LAST, candidate.queue_entered_at ASC, candidate.id ASC
		FOR UPDATE OF candidate SKIP LOCKED LIMIT 1`,
		claim.Scope.Kind, claim.Scope.ID, claim.AssignedAgentID, AgentRunStatusQueued, AgentRunStatusRunning,
		claim.Now, claim.MaxActiveForAgent, agingSeconds, claim.Kind, claim.MaxActiveForConcurrencyKey).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	selected, err := decodeAgentRun(payload)
	if err != nil {
		return nil, err
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
	updatedPayload, err := json.Marshal(selected)
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("agent_runs")+`
		SET status = $1, revision = $2, lease_owner = $3, lease_expires_at = $4,
			last_claimed_at = $5, attempt = $6, payload = $7::jsonb
		WHERE scope_kind = $8 AND scope_id = $9 AND id = $10 AND revision = $11`,
		selected.Status, selected.Revision, selected.LeaseOwner, selected.LeaseExpiresAt,
		selected.LastClaimedAt, selected.Attempt, string(updatedPayload), selected.Scope.Kind, selected.Scope.ID,
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
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return selected, nil
}

func (s *PostgresStore) RenewAgentRunLease(ctx context.Context, scope Scope, runID, workerID string, now time.Time, leaseDuration time.Duration) (*AgentRun, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var payload string
	err = tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("agent_runs")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND id = $3 FOR UPDATE`, scope.Kind, scope.ID, runID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
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
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("agent_runs")+`
		SET revision = $1, lease_expires_at = $2, payload = $3::jsonb
		WHERE scope_kind = $4 AND scope_id = $5 AND id = $6 AND revision = $7 AND lease_owner = $8`,
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
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return run, nil
}
