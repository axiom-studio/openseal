package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/axiom-studio/openseal/pkg/executor"
	_ "github.com/mattn/go-sqlite3"
)

// SQLiteStore persists execution records in SQLite.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore opens (and creates) a SQLite-backed execution store.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS runs (
			run_id INTEGER PRIMARY KEY AUTOINCREMENT,
			workflow_name TEXT NOT NULL,
			workflow_json TEXT NOT NULL DEFAULT '{}',
			trigger_data TEXT NOT NULL DEFAULT '{}',
			status TEXT NOT NULL DEFAULT 'pending',
			node_results TEXT DEFAULT '{}',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			started_at DATETIME,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			available_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			lease_owner TEXT NOT NULL DEFAULT '',
			lease_expires_at DATETIME,
			completed_at DATETIME,
			error TEXT,
			retry_count INTEGER DEFAULT 0
		);
	`); err != nil {
		return err
	}
	if err := addMissingRunColumns(db); err != nil {
		return err
	}
	if _, err := db.Exec(`
		UPDATE runs SET
			workflow_json = COALESCE(NULLIF(workflow_json, ''), '{}'),
			trigger_data = COALESCE(NULLIF(trigger_data, ''), '{}'),
			created_at = COALESCE(created_at, started_at, CURRENT_TIMESTAMP),
			updated_at = COALESCE(updated_at, started_at, CURRENT_TIMESTAMP),
			available_at = COALESCE(available_at, started_at, CURRENT_TIMESTAMP),
			lease_owner = COALESCE(lease_owner, '');
		CREATE INDEX IF NOT EXISTS idx_runs_runnable ON runs(status, available_at, lease_expires_at, run_id);
	`); err != nil {
		return err
	}
	if err := migratePortfolio(db); err != nil {
		return err
	}
	if err := migrateAgentRegistry(db); err != nil {
		return err
	}
	if err := migrateTeamRegistry(db); err != nil {
		return err
	}
	return migrateSkillCatalog(db)
}

func addMissingRunColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(runs)`)
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

	additions := []struct {
		name string
		sql  string
	}{
		{"workflow_json", `ALTER TABLE runs ADD COLUMN workflow_json TEXT NOT NULL DEFAULT '{}'`},
		{"trigger_data", `ALTER TABLE runs ADD COLUMN trigger_data TEXT NOT NULL DEFAULT '{}'`},
		{"created_at", `ALTER TABLE runs ADD COLUMN created_at DATETIME`},
		{"updated_at", `ALTER TABLE runs ADD COLUMN updated_at DATETIME`},
		{"available_at", `ALTER TABLE runs ADD COLUMN available_at DATETIME`},
		{"lease_owner", `ALTER TABLE runs ADD COLUMN lease_owner TEXT NOT NULL DEFAULT ''`},
		{"lease_expires_at", `ALTER TABLE runs ADD COLUMN lease_expires_at DATETIME`},
	}
	for _, addition := range additions {
		if columns[addition.name] {
			continue
		}
		if _, err := db.Exec(addition.sql); err != nil {
			return fmt.Errorf("add runs.%s: %w", addition.name, err)
		}
	}
	return nil
}

// CreateRun implements ExecutionStore.
func (s *SQLiteStore) CreateRun(ctx context.Context, workflow WorkflowEntry, triggerData map[string]interface{}) (int, error) {
	workflowJSON, err := json.Marshal(workflow)
	if err != nil {
		return 0, fmt.Errorf("marshal workflow snapshot: %w", err)
	}
	triggerJSON, err := json.Marshal(triggerData)
	if err != nil {
		return 0, fmt.Errorf("marshal trigger data: %w", err)
	}
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO runs (workflow_name, workflow_json, trigger_data, status, created_at, updated_at, available_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		workflow.Name, string(workflowJSON), string(triggerJSON), RunStatusPending, now, now, now,
	)
	if err != nil {
		return 0, fmt.Errorf("insert run: %w", err)
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// GetRun implements ExecutionStore.
func (s *SQLiteStore) GetRun(_ context.Context, runID int) (*RunRecord, error) {
	r, err := scanRun(s.db.QueryRow(runSelect+` WHERE run_id = ?`, runID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select run: %w", err)
	}
	return r, nil
}

func (s *SQLiteStore) ClaimNextRunnable(ctx context.Context, workerID string, leaseDuration time.Duration) (*RunRecord, error) {
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

	now := time.Now()
	var runID int
	err = conn.QueryRowContext(ctx, `
		SELECT run_id FROM runs
		WHERE ((status IN (?, ?) AND available_at <= ?)
		   OR (status = ? AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?))
		ORDER BY available_at ASC, run_id ASC LIMIT 1`,
		RunStatusPending, RunStatusRetrying, now, RunStatusRunning, now,
	).Scan(&runID)
	if err == sql.ErrNoRows {
		_, err = conn.ExecContext(ctx, "COMMIT")
		committed = err == nil
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	expires := now.Add(leaseDuration)
	result, err := conn.ExecContext(ctx, `
		UPDATE runs SET status = ?, lease_owner = ?, lease_expires_at = ?,
		started_at = COALESCE(started_at, ?), updated_at = ?, completed_at = NULL
		WHERE run_id = ? AND ((status IN (?, ?) AND available_at <= ?)
		   OR (status = ? AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?))`,
		RunStatusRunning, workerID, expires, now, now, runID,
		RunStatusPending, RunStatusRetrying, now, RunStatusRunning, now,
	)
	if err != nil {
		return nil, err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return nil, fmt.Errorf("claim run %d: %w", runID, ErrLeaseLost)
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return s.GetRun(ctx, runID)
}

func (s *SQLiteStore) RenewLease(ctx context.Context, runID int, workerID string, leaseDuration time.Duration) error {
	now := time.Now()
	return expectOne(s.db.ExecContext(ctx, `UPDATE runs SET lease_expires_at = ?, updated_at = ?
		WHERE run_id = ? AND status = ? AND lease_owner = ?`,
		now.Add(leaseDuration), now, runID, RunStatusRunning, workerID))
}

func (s *SQLiteStore) UpdateNodeResult(ctx context.Context, runID int, workerID, nodeID string, result *executor.NodeResult) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT node_results FROM runs WHERE run_id = ? AND status = ? AND lease_owner = ?`, runID, RunStatusRunning, workerID).Scan(&raw)
	if err == sql.ErrNoRows {
		return ErrLeaseLost
	}
	if err != nil {
		return err
	}
	results := make(map[string]*executor.NodeResult)
	_ = json.Unmarshal([]byte(raw), &results)
	results[nodeID] = result
	encoded, err := json.Marshal(results)
	if err != nil {
		return err
	}
	if err := expectOne(tx.ExecContext(ctx, `UPDATE runs SET node_results = ?, updated_at = ? WHERE run_id = ? AND status = ? AND lease_owner = ?`, string(encoded), time.Now(), runID, RunStatusRunning, workerID)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) CompleteRun(ctx context.Context, runID int, workerID, status string, runErr error) error {
	now := time.Now()
	return expectOne(s.db.ExecContext(ctx, `UPDATE runs SET status = ?, completed_at = ?, updated_at = ?, error = ?, lease_owner = '', lease_expires_at = NULL
		WHERE run_id = ? AND status = ? AND lease_owner = ?`, status, now, now, errorString(runErr), runID, RunStatusRunning, workerID))
}

func (s *SQLiteStore) ScheduleRetry(ctx context.Context, runID int, workerID string, availableAt time.Time, runErr error) error {
	return expectOne(s.db.ExecContext(ctx, `UPDATE runs SET status = ?, retry_count = retry_count + 1, available_at = ?, updated_at = ?, error = ?, lease_owner = '', lease_expires_at = NULL
		WHERE run_id = ? AND status = ? AND lease_owner = ?`, RunStatusRetrying, availableAt, time.Now(), errorString(runErr), runID, RunStatusRunning, workerID))
}

// ListRuns implements ExecutionStore.
func (s *SQLiteStore) ListRuns(_ context.Context, limit int) ([]*RunRecord, error) {
	query := runSelect + ` ORDER BY run_id DESC`
	args := []interface{}{}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	var results []*RunRecord
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// Close closes the underlying database.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

const runSelect = `SELECT run_id, workflow_name, workflow_json, trigger_data, status, node_results,
	created_at, started_at, updated_at, available_at, lease_owner, lease_expires_at,
	completed_at, error, retry_count FROM runs`

type rowScanner interface {
	Scan(dest ...interface{}) error
}

func scanRun(row rowScanner) (*RunRecord, error) {
	var r RunRecord
	var workflowJSON, triggerJSON, nodeResultsJSON string
	var startedAt, leaseExpiresAt, completedAt sql.NullTime
	var runErr sql.NullString
	err := row.Scan(&r.RunID, &r.WorkflowName, &workflowJSON, &triggerJSON, &r.Status, &nodeResultsJSON,
		&r.CreatedAt, &startedAt, &r.UpdatedAt, &r.AvailableAt, &r.LeaseOwner, &leaseExpiresAt,
		&completedAt, &runErr, &r.RetryCount)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(workflowJSON), &r.Workflow); err != nil {
		return nil, fmt.Errorf("decode workflow snapshot for run %d: %w", r.RunID, err)
	}
	_ = json.Unmarshal([]byte(triggerJSON), &r.TriggerData)
	_ = json.Unmarshal([]byte(nodeResultsJSON), &r.NodeResults)
	if r.NodeResults == nil {
		r.NodeResults = make(map[string]*executor.NodeResult)
	}
	if startedAt.Valid {
		r.StartedAt = &startedAt.Time
	}
	if leaseExpiresAt.Valid {
		r.LeaseExpiresAt = &leaseExpiresAt.Time
	}
	if completedAt.Valid {
		r.CompletedAt = &completedAt.Time
	}
	if runErr.Valid {
		r.Error = runErr.String
	}
	return &r, nil
}

func expectOne(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrLeaseLost
	}
	return nil
}

func errorString(err error) interface{} {
	if err == nil {
		return nil
	}
	return err.Error()
}
