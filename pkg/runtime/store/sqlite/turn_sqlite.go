package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
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

func (s *SQLiteStore) CreateAgentTurn(ctx context.Context, turn *models.AgentTurn) (*models.AgentTurn, error) {
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
		return nil, models.ErrRunNotFound
	}
	if err != nil {
		return nil, err
	}
	err = conn.QueryRowContext(ctx, `SELECT 1 FROM agent_turns
		WHERE scope_kind = ? AND scope_id = ? AND run_id = ? AND status = ? LIMIT 1`,
		turn.Scope.Kind, turn.Scope.ID, turn.RunID, models.AgentTurnStatusRunning).Scan(&exists)
	if err == nil {
		return nil, models.ErrActiveTurnExists
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	persisted := models.CloneAgentTurn(turn)
	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM agent_turns
		WHERE scope_kind = ? AND scope_id = ? AND run_id = ?`,
		turn.Scope.Kind, turn.Scope.ID, turn.RunID).Scan(&persisted.Sequence); err != nil {
		return nil, err
	}
	if err := insertSQLiteAgentTurn(ctx, conn, persisted); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return models.CloneAgentTurn(persisted), nil
}

func (s *SQLiteStore) GetAgentTurn(ctx context.Context, scope models.Scope, turnID string) (*models.AgentTurn, error) {
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
	return sqlstore.DecodeAgentTurnRecord(payload)
}

func (s *SQLiteStore) ListAgentTurns(ctx context.Context, filter models.AgentTurnFilter) ([]*models.AgentTurn, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(filter.RunID) == "" {
		return nil, models.ErrRunNotFound
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT payload FROM agent_turns
		WHERE scope_kind = ? AND scope_id = ? AND run_id = ? AND sequence > ?`
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID, filter.RunID, filter.AfterSequence}
	if len(filter.Statuses) > 0 {
		query += ` AND status IN (` + strings.TrimRight(strings.Repeat("?,", len(filter.Statuses)), ",") + `)`
		for _, status := range filter.Statuses {
			args = append(args, status)
		}
	}
	if filter.Descending {
		query += ` ORDER BY sequence DESC LIMIT ?`
	} else {
		query += ` ORDER BY sequence ASC LIMIT ?`
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*models.AgentTurn, 0, limit)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		turn, err := sqlstore.DecodeAgentTurnRecord(payload)
		if err != nil {
			return nil, err
		}
		result = append(result, turn)
	}
	return result, rows.Err()
}

func (s *SQLiteStore) UpdateAgentTurn(ctx context.Context, turn *models.AgentTurn, expectedRevision int64, workerID string) error {
	if err := turn.Validate(); err != nil {
		return err
	}
	if turn.Revision != expectedRevision+1 {
		return models.ErrRevisionConflict
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
		return models.ErrRevisionConflict
	}
	return nil
}

func (s *SQLiteStore) RecordAgentTurnStepModel(ctx context.Context, turn *models.AgentTurn, expectedRevision int64, events []*models.ActivityEvent) error {
	if err := models.ValidateStepModelRecordInput(turn, expectedRevision, events); err != nil {
		return err
	}
	payload, err := json.Marshal(turn)
	if err != nil {
		return err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
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
		return models.ErrRunNotFound
	}
	if err != nil {
		return err
	}
	var currentPayload string
	err = conn.QueryRowContext(ctx, `SELECT payload FROM agent_turns WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
		turn.Scope.Kind, turn.Scope.ID, turn.ID).Scan(&currentPayload)
	if err == sql.ErrNoRows {
		return models.ErrTurnNotFound
	}
	if err != nil {
		return err
	}
	current, err := sqlstore.DecodeAgentTurnRecord(currentPayload)
	if err != nil {
		return err
	}
	if err := models.ValidateStepModelWrite(current, turn, expectedRevision); err != nil {
		return err
	}
	result, err := conn.ExecContext(ctx, `UPDATE agent_turns SET status = ?, revision = ?, lease_owner = ?, lease_expires_at = ?, payload = ?
		WHERE scope_kind = ? AND scope_id = ? AND id = ? AND revision = ? AND lease_owner = ?`,
		turn.Status, turn.Revision, turn.LeaseOwner, turn.LeaseExpiresAt, string(payload), turn.Scope.Kind, turn.Scope.ID, turn.ID,
		expectedRevision, turn.LeaseOwner)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return models.ErrRevisionConflict
	}
	for _, event := range events {
		if _, err := insertSQLiteActivityConn(ctx, conn, event); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func (s *SQLiteStore) ClaimAgentTurn(ctx context.Context, scope models.Scope, turnID, workerID string, now time.Time, leaseDuration time.Duration) (*models.AgentTurn, error) {
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
		return nil, models.ErrTurnNotFound
	}
	if err != nil {
		return nil, err
	}
	if models.AgentTurnStatus(status) != models.AgentTurnStatusRunning ||
		(leaseOwner != workerID && leaseExpires.Valid && leaseExpires.Time.After(now)) {
		return nil, models.ErrTurnLeaseHeld
	}
	turn, err := sqlstore.DecodeAgentTurnRecord(payload)
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
		return nil, models.ErrRevisionConflict
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return turn, nil
}

func insertSQLiteAgentTurn(ctx context.Context, exec sqlstore.Execer, turn *models.AgentTurn) error {
	payload, err := json.Marshal(turn)
	if err != nil {
		return err
	}
	_, err = exec.ExecContext(ctx, `INSERT INTO agent_turns
		(id, scope_kind, scope_id, run_id, sequence, status, revision, lease_owner, lease_expires_at, created_at, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, turn.ID, turn.Scope.Kind, turn.Scope.ID,
		turn.RunID, turn.Sequence, turn.Status, turn.Revision, turn.LeaseOwner,
		turn.LeaseExpiresAt, turn.CreatedAt, string(payload))
	return err
}
