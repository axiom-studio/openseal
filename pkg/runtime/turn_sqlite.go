package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
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
	return err
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
		(id, scope_kind, scope_id, run_id, sequence, status, revision, created_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, persisted.ID, persisted.Scope.Kind, persisted.Scope.ID,
		persisted.RunID, persisted.Sequence, persisted.Status, persisted.Revision, persisted.CreatedAt, string(payload))
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

func (s *SQLiteStore) UpdateAgentTurn(ctx context.Context, turn *AgentTurn, expectedRevision int64) error {
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
	result, err := s.db.ExecContext(ctx, `UPDATE agent_turns SET status = ?, revision = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND run_id = ? AND sequence = ? AND revision = ?`,
		turn.Status, turn.Revision, string(payload), turn.Scope.Kind, turn.Scope.ID, turn.ID,
		turn.RunID, turn.Sequence, expectedRevision)
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

func decodeAgentTurnRecord(payload string) (*AgentTurn, error) {
	var turn AgentTurn
	if err := json.Unmarshal([]byte(payload), &turn); err != nil {
		return nil, err
	}
	return &turn, nil
}
