package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lib/pq"
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
		`CREATE INDEX IF NOT EXISTS agent_runs_global_recovery_idx ON ` + s.table("agent_runs") + ` (status, available_at, lease_expires_at, scope_kind, scope_id) WHERE COALESCE(payload->>'kind', 'agent_work') = 'workforce_authoring'`,
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

func (s *PostgresStore) ListObjectiveScopes(ctx context.Context) ([]Scope, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT scope_kind, scope_id FROM `+s.table("objectives")+` ORDER BY scope_kind, scope_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]Scope, 0)
	for rows.Next() {
		var scope Scope
		if err := rows.Scan(&scope.Kind, &scope.ID); err != nil {
			return nil, err
		}
		result = append(result, scope)
	}
	return result, rows.Err()
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.allocatePostgresObjectiveRunTx(ctx, tx, run); err != nil {
		return err
	}
	if err := s.insertPostgresAgentRunTx(ctx, tx, run); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) allocatePostgresObjectiveRunTx(ctx context.Context, tx *sql.Tx, run *AgentRun) error {
	if run == nil || run.ObjectiveID == "" || run.ParentRunID != "" {
		return nil
	}
	var payload string
	if err := tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("objectives")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND id = $3 FOR UPDATE`, run.Scope.Kind, run.Scope.ID, run.ObjectiveID).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrObjectiveNotFound
		}
		return err
	}
	objective, err := decodeObjective(payload)
	if err != nil {
		return err
	}
	updated, err := allocateObjectiveRunBudget(objective, run, run.CreatedAt)
	if err != nil || updated == objective {
		return err
	}
	updatedPayload, err := json.Marshal(updated)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("objectives")+` SET revision = $1, updated_at = $2, payload = $3::jsonb
		WHERE scope_kind = $4 AND scope_id = $5 AND id = $6 AND revision = $7`, updated.Revision, updated.UpdatedAt,
		string(updatedPayload), updated.Scope.Kind, updated.Scope.ID, updated.ID, objective.Revision)
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
	query := `SELECT payload FROM ` + s.table("agent_runs") + ` WHERE scope_kind = $1 AND scope_id = $2`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	add := func(clause string, value interface{}) {
		args = append(args, value)
		query += fmt.Sprintf(clause, len(args))
	}
	if query := strings.TrimSpace(filter.Query); query != "" {
		add(` AND strpos(lower(COALESCE(payload->>'goal', '') || ' ' || status), lower($%d)) > 0`, query)
	}
	if filter.Kind != "" {
		add(` AND COALESCE(payload->>'kind', 'agent_work') = $%d`, filter.Kind)
	}
	if filter.Owner != nil {
		add(` AND payload->'owner'->>'type' = $%d`, filter.Owner.Type)
		add(` AND payload->'owner'->>'id' = $%d`, filter.Owner.ID)
	}
	if filter.ObjectiveID != "" {
		add(` AND objective_id = $%d`, filter.ObjectiveID)
	}
	if filter.ConversationID != "" {
		add(` AND payload->'context'->>'conversationId' = $%d`, filter.ConversationID)
	}
	if filter.ParentRunID != "" {
		add(` AND parent_run_id = $%d`, filter.ParentRunID)
	}
	if filter.RootRunID != "" {
		add(` AND root_run_id = $%d`, filter.RootRunID)
	}
	if filter.AssignedAgentID != "" {
		add(` AND assigned_agent_id = $%d`, filter.AssignedAgentID)
	}
	if len(filter.Statuses) > 0 {
		statuses := make([]string, 0, len(filter.Statuses))
		for _, status := range filter.Statuses {
			statuses = append(statuses, string(status))
		}
		add(` AND status = ANY($%d)`, pq.Array(statuses))
	}
	if filter.Order == AgentRunOrderCreatedDesc {
		query += ` ORDER BY created_at DESC, id DESC`
	} else {
		query += ` ORDER BY priority DESC, created_at ASC, id ASC`
	}
	if filter.Limit > 0 {
		add(` LIMIT $%d`, filter.Limit)
	}
	if filter.Offset > 0 {
		add(` OFFSET $%d`, filter.Offset)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
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
		result = append(result, run)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *PostgresStore) SummarizeAgentRuns(ctx context.Context, scope Scope, owners []ObjectiveOwner) ([]AgentRunOwnerSummary, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if len(owners) == 0 {
		return []AgentRunOwnerSummary{}, nil
	}
	keys := make([]string, 0, len(owners))
	for _, owner := range owners {
		keys = append(keys, string(owner.Type)+"\x1f"+owner.ID)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("agent_runs")+`
		WHERE scope_kind = $1 AND scope_id = $2
		AND ((payload->'owner'->>'type') || chr(31) || (payload->'owner'->>'id')) = ANY($3)`, scope.Kind, scope.ID, pq.Array(keys))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := make([]*AgentRun, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		run, err := decodeAgentRun(payload)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return summarizeAgentRunOwners(runs, owners), nil
}

func (s *PostgresStore) ClaimNextAgentRun(ctx context.Context, claim AgentRunClaim) (*AgentRun, error) {
	decision, err := s.ClaimNextAgentRunWithDecision(ctx, claim)
	if err != nil || decision == nil {
		return nil, err
	}
	return decision.Run, nil
}

func (s *PostgresStore) ClaimNextAgentRunWithDecision(ctx context.Context, claim AgentRunClaim) (*AgentRunAdmissionDecision, error) {
	if err := claim.Validate(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Objective-owned admission policy can constrain any claim even when the
	// embedding worker supplies no host ceiling. Serialize the short selection
	// transaction per scope so concurrent replicas cannot over-admit it.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "openseal:agent-claim:"+claim.Scope.Kind+":"+claim.Scope.ID); err != nil {
		return nil, err
	}
	agingSeconds := claim.AgingInterval.Seconds()
	var payload string
	err = tx.QueryRowContext(ctx, `SELECT candidate.payload FROM `+s.table("agent_runs")+` AS candidate
		LEFT JOIN `+s.table("objectives")+` AS objective
		ON objective.scope_kind = candidate.scope_kind AND objective.scope_id = candidate.scope_id AND objective.id = candidate.objective_id
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
		AND ($11 = 0 OR (
			SELECT COUNT(*) FROM `+s.table("agent_runs")+` AS active
			WHERE active.scope_kind = candidate.scope_kind AND active.scope_id = candidate.scope_id
			AND active.payload->'owner' = candidate.payload->'owner'
			AND active.status = $5 AND active.lease_expires_at IS NOT NULL AND active.lease_expires_at > $6
		) < $11)
		AND (candidate.objective_id = '' OR
			(CASE
				WHEN $12 > 0 AND COALESCE(NULLIF(objective.payload->'executionPolicy'->>'maximumConcurrentRuns', '')::integer, 0) > 0
					THEN LEAST($12, (objective.payload->'executionPolicy'->>'maximumConcurrentRuns')::integer)
				WHEN $12 > 0 THEN $12
				ELSE COALESCE(NULLIF(objective.payload->'executionPolicy'->>'maximumConcurrentRuns', '')::integer, 0)
			END) = 0 OR (
			SELECT COUNT(*) FROM `+s.table("agent_runs")+` AS active
			WHERE active.scope_kind = candidate.scope_kind AND active.scope_id = candidate.scope_id
			AND active.objective_id = candidate.objective_id
			AND active.status = $5 AND active.lease_expires_at IS NOT NULL AND active.lease_expires_at > $6
		) < (CASE
			WHEN $12 > 0 AND COALESCE(NULLIF(objective.payload->'executionPolicy'->>'maximumConcurrentRuns', '')::integer, 0) > 0
				THEN LEAST($12, (objective.payload->'executionPolicy'->>'maximumConcurrentRuns')::integer)
			WHEN $12 > 0 THEN $12
			ELSE COALESCE(NULLIF(objective.payload->'executionPolicy'->>'maximumConcurrentRuns', '')::integer, 0)
		END))
		AND NOT EXISTS (
			SELECT 1
			FROM jsonb_each_text(COALESCE(candidate.payload->'resourceRequirements', '{}'::jsonb)) AS requirement(resource, quantity)
			WHERE COALESCE(objective.payload->'executionPolicy'->'resourceCapacities', '{}'::jsonb) ? requirement.resource
			AND COALESCE((
				SELECT SUM(COALESCE(NULLIF(active.payload->'resourceRequirements'->>requirement.resource, '')::integer, 0))
				FROM `+s.table("agent_runs")+` AS active
				WHERE active.scope_kind = candidate.scope_kind AND active.scope_id = candidate.scope_id
				AND active.objective_id = candidate.objective_id
				AND active.status = $5 AND active.lease_expires_at IS NOT NULL AND active.lease_expires_at > $6
			), 0) + requirement.quantity::integer >
				COALESCE(NULLIF(objective.payload->'executionPolicy'->'resourceCapacities'->>requirement.resource, '')::integer, 0)
		)
		ORDER BY candidate.priority + FLOOR(GREATEST(EXTRACT(EPOCH FROM ($6 - candidate.queue_entered_at)), 0) / $8) DESC,
			candidate.deadline ASC NULLS LAST, candidate.queue_entered_at ASC, candidate.id ASC
		FOR UPDATE OF candidate SKIP LOCKED LIMIT 1`,
		claim.Scope.Kind, claim.Scope.ID, claim.AssignedAgentID, AgentRunStatusQueued, AgentRunStatusRunning,
		claim.Now, claim.MaxActiveForAgent, agingSeconds, claim.Kind, claim.MaxActiveForConcurrencyKey,
		claim.MaxActiveForOwner, claim.MaxActiveForObjective).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		decision, explainErr := s.explainPostgresAgentRunAdmission(ctx, tx, claim)
		if explainErr != nil {
			return nil, explainErr
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return decision, nil
	}
	if err != nil {
		return nil, err
	}
	selected, err := decodeAgentRun(payload)
	if err != nil {
		return nil, err
	}
	previousRevision := selected.Revision
	if err := applyAgentRunClaim(selected, claim); err != nil {
		return nil, err
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
	decision := &AgentRunAdmissionDecision{Outcome: AgentRunAdmissionClaimed, Run: selected, EvaluatedAt: claim.Now}
	if selected.Status == AgentRunStatusPaused && selected.BudgetState == BudgetStateExhausted {
		decision.Outcome = AgentRunAdmissionBudgetStopped
		decision.Blocks = []AgentRunAdmissionBlock{{
			RunID: selected.ID, ObjectiveID: selected.ObjectiveID, AssignedAgentID: selected.AssignedAgentID,
			Owner: selected.Owner, ConcurrencyKey: selected.ConcurrencyKey, Reason: AgentRunAdmissionReasonAttemptBudgetExhausted,
		}}
	}
	return decision, nil
}

func (s *PostgresStore) explainPostgresAgentRunAdmission(ctx context.Context, tx *sql.Tx, claim AgentRunClaim) (*AgentRunAdmissionDecision, error) {
	rows, err := tx.QueryContext(ctx, `SELECT payload FROM `+s.table("agent_runs")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND status IN ($3, $4)`,
		claim.Scope.Kind, claim.Scope.ID, AgentRunStatusQueued, AgentRunStatusRunning)
	if err != nil {
		return nil, err
	}
	runs := make([]*AgentRun, 0)
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
		runs = append(runs, run)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	objectiveRows, err := tx.QueryContext(ctx, `SELECT id, payload FROM `+s.table("objectives")+`
		WHERE scope_kind = $1 AND scope_id = $2`, claim.Scope.Kind, claim.Scope.ID)
	if err != nil {
		return nil, err
	}
	objectives := make(map[string]*Objective)
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
	selected, decision := evaluateAgentRunAdmission(runs, objectives, claim)
	if selected != nil {
		return nil, errors.New("agent run admission query drifted from the canonical evaluator")
	}
	return decision, nil
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
