package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

func migrateAgentTurns(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS agent_turns (
			id TEXT NOT NULL,
			scope_kind TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			run_id TEXT NOT NULL,
			sequence INTEGER NOT NULL,
			status TEXT NOT NULL,
			revision INTEGER NOT NULL,
			lease_owner TEXT NOT NULL DEFAULT '',
			lease_expires_at DATETIME,
			created_at DATETIME NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id),
			UNIQUE (scope_kind, scope_id, run_id, sequence)
		);
		CREATE INDEX IF NOT EXISTS idx_agent_turns_run
			ON agent_turns(scope_kind, scope_id, run_id, sequence);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_turns_one_active
			ON agent_turns(scope_kind, scope_id, run_id) WHERE status = 'running';
	`)
	if err != nil {
		return err
	}
	return addMissingAgentTurnColumns(db)
}

func addMissingAgentTurnColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(agent_turns)`)
	if err != nil {
		return err
	}
	columns := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue interface{}
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}
	additions := []struct{ name, statement string }{
		{"lease_owner", `ALTER TABLE agent_turns ADD COLUMN lease_owner TEXT NOT NULL DEFAULT ''`},
		{"lease_expires_at", `ALTER TABLE agent_turns ADD COLUMN lease_expires_at DATETIME`},
	}
	for _, addition := range additions {
		if columns[addition.name] {
			continue
		}
		if _, err := db.Exec(addition.statement); err != nil {
			return fmt.Errorf("add agent_turns.%s: %w", addition.name, err)
		}
	}
	return nil
}

func (s *SQLiteStore) CreateAgentTurn(ctx context.Context, turn *AgentTurn) (*AgentTurn, error) {
	if err := turn.Validate(); err != nil {
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
	var exists int
	err = conn.QueryRowContext(ctx, `SELECT 1 FROM agent_runs WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
		turn.Scope.Kind, turn.Scope.ID, turn.RunID).Scan(&exists)
	if err == sql.ErrNoRows {
		return nil, ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	err = conn.QueryRowContext(ctx, `SELECT 1 FROM agent_turns
		WHERE scope_kind = ? AND scope_id = ? AND run_id = ? AND status = ? LIMIT 1`,
		turn.Scope.Kind, turn.Scope.ID, turn.RunID, AgentTurnStatusRunning).Scan(&exists)
	if err == nil {
		return nil, ErrActiveTurnExists
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	persisted := cloneAgentTurn(turn)
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM agent_turns
		WHERE scope_kind = ? AND scope_id = ? AND run_id = ?`,
		turn.Scope.Kind, turn.Scope.ID, turn.RunID).Scan(&persisted.Sequence); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(persisted)
	if err != nil {
		return nil, err
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO agent_turns
		(id, scope_kind, scope_id, run_id, sequence, status, revision, lease_owner, lease_expires_at, created_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, persisted.ID, persisted.Scope.Kind, persisted.Scope.ID,
		persisted.RunID, persisted.Sequence, persisted.Status, persisted.Revision, persisted.LeaseOwner,
		persisted.LeaseExpiresAt, persisted.CreatedAt, string(payload))
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return cloneAgentTurn(persisted), nil
}

func (s *SQLiteStore) GetAgentTurn(ctx context.Context, scope Scope, turnID string) (*AgentTurn, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT payload FROM agent_turns WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
		scope.Kind, scope.ID, turnID).Scan(&payload)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeAgentTurnRecord(payload)
}

func (s *SQLiteStore) ListAgentTurns(ctx context.Context, filter AgentTurnFilter) ([]*AgentTurn, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM agent_turns
		WHERE scope_kind = ? AND scope_id = ? AND run_id = ? AND sequence > ?
		ORDER BY sequence ASC LIMIT ?`, filter.Scope.Kind, filter.Scope.ID, filter.RunID, filter.AfterSequence, limit)
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

func (s *SQLiteStore) UpdateAgentTurn(ctx context.Context, turn *AgentTurn, expectedRevision int64, workerID string) error {
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
	result, err := s.db.ExecContext(ctx, `UPDATE agent_turns SET status = ?, revision = ?, lease_owner = ?, lease_expires_at = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND run_id = ? AND sequence = ? AND revision = ? AND lease_owner = ?`,
		turn.Status, turn.Revision, turn.LeaseOwner, turn.LeaseExpiresAt, string(payload), turn.Scope.Kind, turn.Scope.ID, turn.ID,
		turn.RunID, turn.Sequence, expectedRevision, workerID)
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

func (s *SQLiteStore) ClaimAgentTurn(ctx context.Context, scope Scope, turnID, workerID string, now time.Time, leaseDuration time.Duration) (*AgentTurn, error) {
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
	var payload, status, leaseOwner string
	var leaseExpires sql.NullTime
	err = conn.QueryRowContext(ctx, `SELECT payload, status, lease_owner, lease_expires_at FROM agent_turns
		WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, turnID).
		Scan(&payload, &status, &leaseOwner, &leaseExpires)
	if err == sql.ErrNoRows {
		return nil, ErrTurnNotFound
	}
	if err != nil {
		return nil, err
	}
	if AgentTurnStatus(status) != AgentTurnStatusRunning ||
		(leaseOwner != workerID && leaseExpires.Valid && leaseExpires.Time.After(now)) {
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
	result, err := conn.ExecContext(ctx, `UPDATE agent_turns SET revision = ?, lease_owner = ?, lease_expires_at = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ?`, turn.Revision, workerID, expires,
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
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return turn, nil
}

func decodeAgentTurnRecord(payload string) (*AgentTurn, error) {
	var turn AgentTurn
	if err := json.Unmarshal([]byte(payload), &turn); err != nil {
		return nil, err
	}
	return &turn, nil
}
