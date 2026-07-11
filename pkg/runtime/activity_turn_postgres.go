package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

func (s *PostgresStore) CreateObjectiveWithEvent(ctx context.Context, objective *Objective, event *ActivityEvent) (*ActivityEvent, error) {
	if err := validateObjectiveActivity(objective, event); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(objective)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("objectives")+`
		(id, scope_kind, scope_id, owner_type, owner_id, status, priority, revision, updated_at, payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb)`, objective.ID, objective.Scope.Kind, objective.Scope.ID,
		objective.Owner.Type, objective.Owner.ID, objective.Status, objective.Priority, objective.Revision, objective.UpdatedAt, string(payload)); err != nil {
		return nil, err
	}
	persisted, err := s.insertPostgresActivityTx(ctx, tx, event)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return persisted, nil
}

func (s *PostgresStore) UpdateObjectiveWithEvent(ctx context.Context, objective *Objective, expectedRevision int64, event *ActivityEvent) (*ActivityEvent, error) {
	if err := validateObjectiveActivity(objective, event); err != nil {
		return nil, err
	}
	if objective.Revision != expectedRevision+1 {
		return nil, ErrRevisionConflict
	}
	payload, err := json.Marshal(objective)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("objectives")+` SET owner_type=$1, owner_id=$2, status=$3, priority=$4, revision=$5, updated_at=$6, payload=$7::jsonb
		WHERE scope_kind=$8 AND scope_id=$9 AND id=$10 AND revision=$11`, objective.Owner.Type, objective.Owner.ID,
		objective.Status, objective.Priority, objective.Revision, objective.UpdatedAt, string(payload), objective.Scope.Kind,
		objective.Scope.ID, objective.ID, expectedRevision)
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
	persisted, err := s.insertPostgresActivityTx(ctx, tx, event)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return persisted, nil
}

func (s *PostgresStore) CreateAgentRunWithEvent(ctx context.Context, run *AgentRun, event *ActivityEvent) (*ActivityEvent, error) {
	if err := run.Validate(); err != nil {
		return nil, err
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	if run.Scope != event.Scope || run.ID != event.RunID {
		return nil, ErrInvalidScope
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := s.allocatePostgresObjectiveRunTx(ctx, tx, run); err != nil {
		return nil, err
	}
	if err := s.insertPostgresAgentRunTx(ctx, tx, run); err != nil {
		return nil, err
	}
	persisted, err := s.insertPostgresActivityTx(ctx, tx, event)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return cloneActivityEvent(persisted), nil
}

func (s *PostgresStore) migrateActivityAndTurns(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("run_activity")+` (
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			agent_id TEXT NOT NULL DEFAULT '',
			objective_id TEXT NOT NULL DEFAULT '',
			team_id TEXT NOT NULL DEFAULT '',
			severity TEXT NOT NULL DEFAULT 'info',
			visibility TEXT NOT NULL DEFAULT 'scope',
			sequence BIGINT NOT NULL,
			id TEXT NOT NULL,
			event_type TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, run_id, sequence),
			UNIQUE (scope_kind, scope_id, id)
		);
		CREATE TABLE IF NOT EXISTS `+s.table("agent_turns")+` (
			id TEXT NOT NULL,
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			sequence BIGINT NOT NULL,
			status TEXT NOT NULL,
			revision BIGINT NOT NULL,
			lease_owner TEXT NOT NULL DEFAULT '',
			lease_expires_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL,
			payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id),
			UNIQUE (scope_kind, scope_id, run_id, sequence),
			CHECK (revision > 0)
		)`); err != nil {
		return err
	}
	statements := []string{
		`CREATE INDEX IF NOT EXISTS run_activity_stream_idx ON ` + s.table("run_activity") + ` (scope_kind, scope_id, run_id, sequence)`,
		`CREATE INDEX IF NOT EXISTS agent_turns_run_idx ON ` + s.table("agent_turns") + ` (scope_kind, scope_id, run_id, sequence)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS agent_turns_one_active_idx ON ` + s.table("agent_turns") + ` (scope_kind, scope_id, run_id) WHERE status = 'running'`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name) VALUES (3, 'activity streams and agent turns') ON CONFLICT (version) DO NOTHING`)
	return err
}

func (s *PostgresStore) migrateActivityFeed(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `ALTER TABLE `+s.table("run_activity")+`
		ADD COLUMN IF NOT EXISTS agent_id TEXT NOT NULL DEFAULT '',
		ADD COLUMN IF NOT EXISTS objective_id TEXT NOT NULL DEFAULT '',
		ADD COLUMN IF NOT EXISTS team_id TEXT NOT NULL DEFAULT '',
		ADD COLUMN IF NOT EXISTS severity TEXT NOT NULL DEFAULT 'info',
		ADD COLUMN IF NOT EXISTS visibility TEXT NOT NULL DEFAULT 'scope'`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+s.table("run_activity")+` SET
		agent_id = COALESCE(NULLIF(agent_id, ''), payload->>'agentId', ''),
		objective_id = COALESCE(NULLIF(objective_id, ''), payload->>'objectiveId', ''),
		team_id = COALESCE(NULLIF(team_id, ''), payload->>'teamId', ''),
		severity = COALESCE(NULLIF(severity, ''), payload->>'severity', 'info'),
		visibility = COALESCE(NULLIF(visibility, ''), payload->>'visibility', 'scope')`); err != nil {
		return err
	}
	statements := []string{
		`CREATE INDEX IF NOT EXISTS run_activity_agent_feed_idx ON ` + s.table("run_activity") + ` (scope_kind, scope_id, agent_id, created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS run_activity_objective_feed_idx ON ` + s.table("run_activity") + ` (scope_kind, scope_id, objective_id, created_at DESC, id DESC)`,
		`CREATE INDEX IF NOT EXISTS run_activity_team_feed_idx ON ` + s.table("run_activity") + ` (scope_kind, scope_id, team_id, created_at DESC, id DESC)`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name) VALUES (6, 'indexed activity projections') ON CONFLICT (version) DO NOTHING`)
	return err
}

func (s *PostgresStore) UpdateAgentRunWithEvent(ctx context.Context, run *AgentRun, expectedRevision int64, event *ActivityEvent, lease *AgentRunLeaseGuard) (*ActivityEvent, error) {
	if err := run.Validate(); err != nil {
		return nil, err
	}
	if err := event.Validate(); err != nil {
		return nil, err
	}
	if run.Scope != event.Scope || run.ID != event.RunID {
		return nil, ErrInvalidScope
	}
	if run.Revision != expectedRevision+1 {
		return nil, ErrRevisionConflict
	}
	runPayload, err := json.Marshal(run)
	if err != nil {
		return nil, err
	}
	return s.withActivity(ctx, event, func(tx *sql.Tx) error {
		query := `UPDATE ` + s.table("agent_runs") + ` SET status = $1, priority = $2, assigned_agent_id = $3, revision = $4,
			deadline = $5, available_at = $6, queue_entered_at = $7, lease_owner = $8, lease_expires_at = $9,
			last_claimed_at = $10, attempt = $11, payload = $12::jsonb
			WHERE scope_kind = $13 AND scope_id = $14 AND id = $15 AND revision = $16`
		args := []interface{}{run.Status, run.Priority, run.AssignedAgentID, run.Revision, run.Deadline, run.AvailableAt,
			run.QueueEnteredAt, run.LeaseOwner, run.LeaseExpiresAt, run.LastClaimedAt, run.Attempt, string(runPayload),
			run.Scope.Kind, run.Scope.ID, run.ID, expectedRevision}
		if lease != nil {
			query += ` AND lease_owner = $17 AND lease_expires_at > $18`
			args = append(args, lease.WorkerID, lease.Now)
		}
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected != 1 {
			if lease != nil {
				return ErrLeaseLost
			}
			return ErrRevisionConflict
		}
		return nil
	})
}

func (s *PostgresStore) AppendActivity(ctx context.Context, event *ActivityEvent) (*ActivityEvent, error) {
	if err := event.Validate(); err != nil {
		return nil, err
	}
	return s.withActivity(ctx, event, func(*sql.Tx) error { return nil })
}

func (s *PostgresStore) withActivity(ctx context.Context, event *ActivityEvent, beforeInsert func(*sql.Tx) error) (*ActivityEvent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var exists int
	table, id := s.table("agent_runs"), event.RunID
	if event.RunID == "" {
		table, id = s.table("objectives"), event.ObjectiveID
	}
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM `+table+`
		WHERE scope_kind = $1 AND scope_id = $2 AND id = $3 FOR UPDATE`, event.Scope.Kind, event.Scope.ID, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		if event.RunID == "" {
			return nil, ErrObjectiveNotFound
		}
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := beforeInsert(tx); err != nil {
		return nil, err
	}
	persisted, err := s.insertPostgresActivityTx(ctx, tx, event)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return cloneActivityEvent(persisted), nil
}

func (s *PostgresStore) insertPostgresActivityTx(ctx context.Context, tx *sql.Tx, event *ActivityEvent) (*ActivityEvent, error) {
	persisted := cloneActivityEvent(event)
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM `+s.table("run_activity")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND run_id = $3`, event.Scope.Kind, event.Scope.ID, activityStreamID(event)).Scan(&persisted.Sequence); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(persisted)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("run_activity")+`
		(scope_kind, scope_id, run_id, agent_id, objective_id, team_id, severity, visibility, sequence, id, event_type, created_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::jsonb)`, persisted.Scope.Kind, persisted.Scope.ID,
		activityStreamID(persisted), persisted.AgentID, persisted.ObjectiveID, persisted.TeamID, persisted.Severity, persisted.Visibility,
		persisted.Sequence, persisted.ID, persisted.EventType, persisted.CreatedAt, string(payload)); err != nil {
		return nil, err
	}
	return cloneActivityEvent(persisted), nil
}

func (s *PostgresStore) ListActivity(ctx context.Context, filter ActivityFilter) ([]*ActivityEvent, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if !filter.Descending {
		rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("run_activity")+`
			WHERE scope_kind = $1 AND scope_id = $2 AND run_id = $3 AND sequence > $4
			ORDER BY sequence ASC LIMIT $5`, filter.Scope.Kind, filter.Scope.ID, filter.RunID, filter.AfterSequence, limit)
		if err != nil {
			return nil, err
		}
		return scanPostgresActivity(rows, limit)
	}
	query := `SELECT payload FROM ` + s.table("run_activity") + ` WHERE scope_kind = $1 AND scope_id = $2`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID}
	placeholder := 3
	selectors := []struct{ column, value string }{{"run_id", filter.RunID}, {"agent_id", filter.AgentID}, {"objective_id", filter.ObjectiveID}, {"team_id", filter.TeamID}}
	for _, selector := range selectors {
		if selector.value == "" {
			continue
		}
		query += fmt.Sprintf(" AND %s = $%d", selector.column, placeholder)
		args = append(args, selector.value)
		placeholder++
	}
	query, args, placeholder = appendPostgresActivityStrings(query, args, placeholder, "run_id", filter.RunIDs)
	query, args, placeholder = appendPostgresActivityStrings(query, args, placeholder, "event_type", filter.EventTypes)
	severityValues := make([]string, 0, len(filter.Severities))
	for _, severity := range filter.Severities {
		severityValues = append(severityValues, string(severity))
	}
	query, args, placeholder = appendPostgresActivityStrings(query, args, placeholder, "severity", severityValues)
	visibilityValues := make([]string, 0, len(filter.Visibilities))
	for _, visibility := range filter.Visibilities {
		visibilityValues = append(visibilityValues, string(visibility))
	}
	query, args, placeholder = appendPostgresActivityStrings(query, args, placeholder, "visibility", visibilityValues)
	if filter.BeforeCreatedAt != nil {
		query += fmt.Sprintf(" AND (created_at < $%d OR (created_at = $%d AND id < $%d))", placeholder, placeholder, placeholder+1)
		args = append(args, filter.BeforeCreatedAt, filter.BeforeID)
		placeholder += 2
	}
	query += fmt.Sprintf(" ORDER BY created_at DESC, id DESC LIMIT $%d", placeholder)
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	return scanPostgresActivity(rows, limit)
}

func appendPostgresActivityStrings(query string, args []interface{}, placeholder int, column string, values []string) (string, []interface{}, int) {
	if len(values) == 0 {
		return query, args, placeholder
	}
	query += fmt.Sprintf(" AND %s = ANY($%d)", column, placeholder)
	args = append(args, pq.Array(values))
	return query, args, placeholder + 1
}

func scanPostgresActivity(rows *sql.Rows, capacity int) ([]*ActivityEvent, error) {
	defer rows.Close()
	result := make([]*ActivityEvent, 0, capacity)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var event ActivityEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return nil, err
		}
		result = append(result, &event)
	}
	return result, rows.Err()
}

func (s *PostgresStore) CreateAgentTurn(ctx context.Context, turn *AgentTurn) (*AgentTurn, error) {
	if err := turn.Validate(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM `+s.table("agent_runs")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND id = $3 FOR UPDATE`, turn.Scope.Kind, turn.Scope.ID, turn.RunID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM `+s.table("agent_turns")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND run_id = $3 AND status = $4 LIMIT 1`,
		turn.Scope.Kind, turn.Scope.ID, turn.RunID, AgentTurnStatusRunning).Scan(&exists)
	if err == nil {
		return nil, ErrActiveTurnExists
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	persisted := cloneAgentTurn(turn)
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM `+s.table("agent_turns")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND run_id = $3`, turn.Scope.Kind, turn.Scope.ID, turn.RunID).Scan(&persisted.Sequence); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(persisted)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("agent_turns")+`
		(id, scope_kind, scope_id, run_id, sequence, status, revision, lease_owner, lease_expires_at, created_at, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11::jsonb)`, persisted.ID, persisted.Scope.Kind,
		persisted.Scope.ID, persisted.RunID, persisted.Sequence, persisted.Status, persisted.Revision, persisted.LeaseOwner,
		persisted.LeaseExpiresAt, persisted.CreatedAt, string(payload)); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return cloneAgentTurn(persisted), nil
}

func (s *PostgresStore) GetAgentTurn(ctx context.Context, scope Scope, turnID string) (*AgentTurn, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM `+s.table("agent_turns")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`, scope.Kind, scope.ID, turnID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeAgentTurnRecord(payload)
}

func (s *PostgresStore) ListAgentTurns(ctx context.Context, filter AgentTurnFilter) ([]*AgentTurn, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM `+s.table("agent_turns")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND run_id = $3 AND sequence > $4
		ORDER BY sequence ASC LIMIT $5`, filter.Scope.Kind, filter.Scope.ID, filter.RunID, filter.AfterSequence, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*AgentTurn, 0, limit)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		turn, err := decodeAgentTurnRecord(payload)
		if err != nil {
			return nil, err
		}
		result = append(result, turn)
	}
	return result, rows.Err()
}

func (s *PostgresStore) UpdateAgentTurn(ctx context.Context, turn *AgentTurn, expectedRevision int64, workerID string) error {
	if err := turn.Validate(); err != nil {
		return err
	}
	if turn.Revision != expectedRevision+1 {
		return ErrRevisionConflict
	}
	payload, err := json.Marshal(turn)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE `+s.table("agent_turns")+`
		SET status = $1, revision = $2, lease_owner = $3, lease_expires_at = $4, payload = $5::jsonb
		WHERE scope_kind = $6 AND scope_id = $7 AND id = $8 AND run_id = $9 AND sequence = $10 AND revision = $11 AND lease_owner = $12`,
		turn.Status, turn.Revision, turn.LeaseOwner, turn.LeaseExpiresAt, string(payload), turn.Scope.Kind, turn.Scope.ID,
		turn.ID, turn.RunID, turn.Sequence, expectedRevision, workerID)
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

func (s *PostgresStore) ClaimAgentTurn(ctx context.Context, scope Scope, turnID, workerID string, now time.Time, leaseDuration time.Duration) (*AgentTurn, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var payload, status, leaseOwner string
	var leaseExpires sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT payload, status, lease_owner, lease_expires_at FROM `+s.table("agent_turns")+`
		WHERE scope_kind = $1 AND scope_id = $2 AND id = $3 FOR UPDATE`, scope.Kind, scope.ID, turnID).
		Scan(&payload, &status, &leaseOwner, &leaseExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTurnNotFound
	}
	if err != nil {
		return nil, err
	}
	if AgentTurnStatus(status) != AgentTurnStatusRunning || (leaseOwner != workerID && leaseExpires.Valid && leaseExpires.Time.After(now)) {
		return nil, ErrTurnLeaseHeld
	}
	turn, err := decodeAgentTurnRecord(payload)
	if err != nil {
		return nil, err
	}
	expires := now.Add(leaseDuration)
	turn.LeaseOwner = workerID
	turn.LeaseExpiresAt = &expires
	turn.UpdatedAt = now
	previousRevision := turn.Revision
	turn.Revision++
	updatedPayload, err := json.Marshal(turn)
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE `+s.table("agent_turns")+`
		SET revision = $1, lease_owner = $2, lease_expires_at = $3, payload = $4::jsonb
		WHERE scope_kind = $5 AND scope_id = $6 AND id = $7 AND revision = $8`, turn.Revision, workerID, expires,
		string(updatedPayload), scope.Kind, scope.ID, turnID, previousRevision)
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
	return turn, nil
}
