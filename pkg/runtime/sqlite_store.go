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
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS runs (
			run_id INTEGER PRIMARY KEY AUTOINCREMENT,
			workflow_name TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			node_results TEXT DEFAULT '{}',
			started_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			completed_at DATETIME,
			error TEXT,
			retry_count INTEGER DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status);
	`)
	return err
}

// CreateRun implements ExecutionStore.
func (s *SQLiteStore) CreateRun(_ context.Context, workflowName string) (int, error) {
	res, err := s.db.Exec(
		`INSERT INTO runs (workflow_name, status) VALUES (?, 'pending')`,
		workflowName,
	)
	if err != nil {
		return 0, fmt.Errorf("insert run: %w", err)
	}
	id, _ := res.LastInsertId()
	return int(id), nil
}

// GetRun implements ExecutionStore.
func (s *SQLiteStore) GetRun(_ context.Context, runID int) (*RunRecord, error) {
	var r RunRecord
	var nrJSON string
	var completedAt sql.NullTime
	var runErr sql.NullString

	err := s.db.QueryRow(
		`SELECT run_id, workflow_name, status, node_results, started_at, completed_at, error, retry_count
		 FROM runs WHERE run_id = ?`, runID,
	).Scan(
		&r.RunID, &r.WorkflowName, &r.Status, &nrJSON,
		&r.StartedAt, &completedAt, &runErr, &r.RetryCount,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select run: %w", err)
	}
	if completedAt.Valid {
		r.CompletedAt = &completedAt.Time
	}
	if runErr.Valid {
		r.Error = runErr.String
	}
	if err := json.Unmarshal([]byte(nrJSON), &r.NodeResults); err != nil {
		r.NodeResults = make(map[string]*executor.NodeResult)
	}
	return &r, nil
}

// UpdateNodeResult implements ExecutionStore.
func (s *SQLiteStore) UpdateNodeResult(_ context.Context, runID int, nodeID string, result *executor.NodeResult) error {
	// Load existing node results, merge, write back
	r, err := s.GetRun(nil, runID)
	if err != nil || r == nil {
		return err
	}
	if r.NodeResults == nil {
		r.NodeResults = make(map[string]*executor.NodeResult)
	}
	r.NodeResults[nodeID] = result
	nrJSON, err := json.Marshal(r.NodeResults)
	if err != nil {
		return fmt.Errorf("marshal node results: %w", err)
	}
	_, err = s.db.Exec(
		`UPDATE runs SET node_results = ? WHERE run_id = ?`,
		string(nrJSON), runID,
	)
	return err
}

// UpdateRunStatus implements ExecutionStore.
func (s *SQLiteStore) UpdateRunStatus(_ context.Context, runID int, status string, runErr error) error {
	var errStr *string
	if runErr != nil {
		e := runErr.Error()
		errStr = &e
	}
	_, err := s.db.Exec(
		`UPDATE runs SET status = ?, completed_at = ?, error = ? WHERE run_id = ?`,
		status, time.Now(), errStr, runID,
	)
	return err
}

// IncrementRetryCount implements ExecutionStore.
func (s *SQLiteStore) IncrementRetryCount(_ context.Context, runID int) (int, error) {
	_, err := s.db.Exec(
		`UPDATE runs SET retry_count = retry_count + 1 WHERE run_id = ?`,
		runID,
	)
	if err != nil {
		return 0, err
	}
	var count int
	err = s.db.QueryRow(
		`SELECT retry_count FROM runs WHERE run_id = ?`, runID,
	).Scan(&count)
	return count, err
}

// ListRuns implements ExecutionStore.
func (s *SQLiteStore) ListRuns(_ context.Context, limit int) ([]*RunRecord, error) {
	rows, err := s.db.Query(
		`SELECT run_id, workflow_name, status, node_results, started_at, completed_at, error, retry_count
		 FROM runs ORDER BY run_id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	var results []*RunRecord
	for rows.Next() {
		var r RunRecord
		var nrJSON string
		var completedAt sql.NullTime
		var runErr sql.NullString

		err := rows.Scan(
			&r.RunID, &r.WorkflowName, &r.Status, &nrJSON,
			&r.StartedAt, &completedAt, &runErr, &r.RetryCount,
		)
		if err != nil {
			continue
		}
		if completedAt.Valid {
			r.CompletedAt = &completedAt.Time
		}
		if runErr.Valid {
			r.Error = runErr.String
		}
		json.Unmarshal([]byte(nrJSON), &r.NodeResults)
		results = append(results, &r)
	}
	return results, rows.Err()
}

// Close closes the underlying database.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}
