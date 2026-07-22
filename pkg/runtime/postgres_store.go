package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/executor"
	_ "github.com/lib/pq"
)

const defaultPostgresSchema = "openseal"

const (
	defaultPostgresMaxOpenConnections        = 16
	defaultPostgresMaxIdleConnections        = 4
	defaultPostgresConnectionLifetime        = 30 * time.Minute
	defaultPostgresConnectionIdleTime        = 5 * time.Minute
	defaultPostgresMigrationLockTimeout      = 20 * time.Second
	defaultPostgresMigrationLockPollInterval = 100 * time.Millisecond
)

var postgresIdentifier = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

var ErrPostgresMigrationLockTimeout = errors.New("PostgreSQL migration lock wait timed out")

type PostgresMigrationPhase string

const (
	PostgresMigrationWaiting  PostgresMigrationPhase = "waiting"
	PostgresMigrationAcquired PostgresMigrationPhase = "acquired"
	PostgresMigrationComplete PostgresMigrationPhase = "complete"
	PostgresMigrationTimeout  PostgresMigrationPhase = "timeout"
)

// PostgresMigrationEvent is a credential-free lifecycle projection suitable
// for host logs and metrics. It intentionally excludes the DSN and schema.
type PostgresMigrationEvent struct {
	Phase         PostgresMigrationPhase
	WaitDuration  time.Duration
	TotalDuration time.Duration
	SchemaVersion int64
}

type PostgresMigrationObserver func(PostgresMigrationEvent)

type PostgresMigrationStats struct {
	WaitDuration  time.Duration
	TotalDuration time.Duration
	SchemaVersion int64
}

type PostgresStoreOption func(*postgresStoreConfig) error

type postgresStoreConfig struct {
	schema                    string
	pool                      PostgresPoolConfig
	migrationLockTimeout      time.Duration
	migrationLockPollInterval time.Duration
	migrationObserver         PostgresMigrationObserver
}

// PostgresPoolConfig bounds the process-local database/sql pool. OpenSeal
// starts several durable worker families, so an explicit pool budget is a
// correctness requirement during rolling deploys rather than a tuning hint.
type PostgresPoolConfig struct {
	MaxOpenConnections int
	MaxIdleConnections int
	ConnectionLifetime time.Duration
	ConnectionIdleTime time.Duration
}

func DefaultPostgresPoolConfig() PostgresPoolConfig {
	return PostgresPoolConfig{
		MaxOpenConnections: defaultPostgresMaxOpenConnections,
		MaxIdleConnections: defaultPostgresMaxIdleConnections,
		ConnectionLifetime: defaultPostgresConnectionLifetime,
		ConnectionIdleTime: defaultPostgresConnectionIdleTime,
	}
}

func (c PostgresPoolConfig) validate() error {
	if c.MaxOpenConnections <= 0 || c.MaxOpenConnections > 1024 {
		return errors.New("PostgreSQL max open connections must be between 1 and 1024")
	}
	if c.MaxIdleConnections < 0 || c.MaxIdleConnections > c.MaxOpenConnections {
		return errors.New("PostgreSQL max idle connections must be between zero and max open connections")
	}
	if c.ConnectionLifetime < 0 || c.ConnectionIdleTime < 0 {
		return errors.New("PostgreSQL connection lifetime and idle time cannot be negative")
	}
	return nil
}

// WithPostgresPool applies a validated process-local connection budget.
func WithPostgresPool(pool PostgresPoolConfig) PostgresStoreOption {
	return func(config *postgresStoreConfig) error {
		if err := pool.validate(); err != nil {
			return err
		}
		config.pool = pool
		return nil
	}
}

// PostgresPoolStats is a credential-free operational projection of the
// database/sql pool. Hosts can export it through their metrics system without
// exposing DSNs, queries, tenant identifiers, or other private state.
type PostgresPoolStats struct {
	MaxOpenConnections int
	OpenConnections    int
	InUseConnections   int
	IdleConnections    int
	WaitCount          int64
	WaitDuration       time.Duration
	MaxIdleClosed      int64
	MaxIdleTimeClosed  int64
	MaxLifetimeClosed  int64
}

// WithPostgresSchema isolates OpenSeal tables in a validated PostgreSQL schema.
func WithPostgresSchema(schema string) PostgresStoreOption {
	return func(config *postgresStoreConfig) error {
		schema = strings.TrimSpace(schema)
		if !postgresIdentifier.MatchString(schema) {
			return errors.New("PostgreSQL schema must be a lowercase SQL identifier")
		}
		config.schema = schema
		return nil
	}
}

// WithPostgresMigrationLock bounds database-native migration leadership
// election. Every process opening the same schema waits on the same PostgreSQL
// advisory lock, so only one process can execute DDL at a time.
func WithPostgresMigrationLock(timeout, pollInterval time.Duration) PostgresStoreOption {
	return func(config *postgresStoreConfig) error {
		if timeout <= 0 || timeout > 10*time.Minute {
			return errors.New("PostgreSQL migration lock timeout must be between zero and ten minutes")
		}
		if pollInterval <= 0 || pollInterval > timeout {
			return errors.New("PostgreSQL migration lock poll interval must be positive and no greater than the timeout")
		}
		config.migrationLockTimeout = timeout
		config.migrationLockPollInterval = pollInterval
		return nil
	}
}

// WithPostgresMigrationObserver projects migration leadership without making
// a logging or telemetry implementation part of the OpenSeal kernel.
func WithPostgresMigrationObserver(observer PostgresMigrationObserver) PostgresStoreOption {
	return func(config *postgresStoreConfig) error {
		config.migrationObserver = observer
		return nil
	}
}

// PostgresStore is the shared, horizontally safe OpenSeal persistence adapter.
// Additional kernel contracts are implemented on this type in focused slices.
type PostgresStore struct {
	db                        *sql.DB
	schema                    string
	migrationLockTimeout      time.Duration
	migrationLockPollInterval time.Duration
	migrationObserver         PostgresMigrationObserver
	migrationMu               sync.RWMutex
	migrationStats            PostgresMigrationStats
}

var _ KernelStore = (*PostgresStore)(nil)

// NewPostgresStore opens PostgreSQL and applies OpenSeal's versioned schema.
func NewPostgresStore(ctx context.Context, dsn string, options ...PostgresStoreOption) (*PostgresStore, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("PostgreSQL DSN is required")
	}
	config := postgresStoreConfig{
		schema: defaultPostgresSchema, pool: DefaultPostgresPoolConfig(),
		migrationLockTimeout:      defaultPostgresMigrationLockTimeout,
		migrationLockPollInterval: defaultPostgresMigrationLockPollInterval,
	}
	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option(&config); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL: %w", err)
	}
	db.SetMaxOpenConns(config.pool.MaxOpenConnections)
	db.SetMaxIdleConns(config.pool.MaxIdleConnections)
	db.SetConnMaxLifetime(config.pool.ConnectionLifetime)
	db.SetConnMaxIdleTime(config.pool.ConnectionIdleTime)
	store := &PostgresStore{
		db: db, schema: config.schema,
		migrationLockTimeout:      config.migrationLockTimeout,
		migrationLockPollInterval: config.migrationLockPollInterval,
		migrationObserver:         config.migrationObserver,
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("connect PostgreSQL: %w", err)
	}
	if err := store.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate PostgreSQL: %w", err)
	}
	return store, nil
}

func (s *PostgresStore) MigrationStats() PostgresMigrationStats {
	if s == nil {
		return PostgresMigrationStats{}
	}
	s.migrationMu.RLock()
	defer s.migrationMu.RUnlock()
	return s.migrationStats
}

func (s *PostgresStore) PoolStats() PostgresPoolStats {
	if s == nil || s.db == nil {
		return PostgresPoolStats{}
	}
	stats := s.db.Stats()
	return PostgresPoolStats{
		MaxOpenConnections: stats.MaxOpenConnections,
		OpenConnections:    stats.OpenConnections,
		InUseConnections:   stats.InUse,
		IdleConnections:    stats.Idle,
		WaitCount:          stats.WaitCount,
		WaitDuration:       stats.WaitDuration,
		MaxIdleClosed:      stats.MaxIdleClosed,
		MaxIdleTimeClosed:  stats.MaxIdleTimeClosed,
		MaxLifetimeClosed:  stats.MaxLifetimeClosed,
	}
}

func (s *PostgresStore) migrate(ctx context.Context) error {
	startedAt := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	waitDuration, err := s.acquireMigrationLock(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE SCHEMA IF NOT EXISTS `+s.quotedSchema()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("schema_migrations")+` (
			version BIGINT PRIMARY KEY,
			name TEXT NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS `+s.table("runs")+` (
			run_id BIGSERIAL PRIMARY KEY,
			workflow_name TEXT NOT NULL,
			workflow_json JSONB NOT NULL DEFAULT '{}'::jsonb,
			trigger_data JSONB NOT NULL DEFAULT '{}'::jsonb,
			status TEXT NOT NULL DEFAULT 'pending',
			node_results JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
			started_at TIMESTAMPTZ,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
			available_at TIMESTAMPTZ NOT NULL DEFAULT CURRENT_TIMESTAMP,
			lease_owner TEXT NOT NULL DEFAULT '',
			lease_expires_at TIMESTAMPTZ,
			completed_at TIMESTAMPTZ,
			error TEXT,
			retry_count INTEGER NOT NULL DEFAULT 0,
			CHECK (retry_count >= 0)
		)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS runs_runnable_idx ON `+s.table("runs")+` (status, available_at, lease_expires_at, run_id)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name) VALUES (1, 'workflow execution') ON CONFLICT (version) DO NOTHING`); err != nil {
		return err
	}
	if err := s.migratePortfolio(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateInitiatives(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateSourceMonitors(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateSourcePolicies(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateEventSourceCheckpoints(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateEventSourceSubscriptions(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateOutreach(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateActivityAndTurns(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateAgentAndSkillControlPlane(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateSkillSourceArtifacts(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateAgentDefinitionCompilations(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateTeamControlPlane(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateActions(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateActionCredentialLeaseRedemptions(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateActivityFeed(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateCollaboration(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateArtifacts(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateRunDependencies(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateConversations(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateAuthoringChangeSets(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateSourceQualifiedSkillVariants(ctx, tx); err != nil {
		return err
	}
	var schemaVersion int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM `+s.table("schema_migrations")).Scan(&schemaVersion); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	stats := PostgresMigrationStats{WaitDuration: waitDuration, TotalDuration: time.Since(startedAt), SchemaVersion: schemaVersion}
	s.migrationMu.Lock()
	s.migrationStats = stats
	s.migrationMu.Unlock()
	s.observeMigration(PostgresMigrationEvent{Phase: PostgresMigrationComplete, WaitDuration: stats.WaitDuration, TotalDuration: stats.TotalDuration, SchemaVersion: stats.SchemaVersion})
	return nil
}

func (s *PostgresStore) acquireMigrationLock(ctx context.Context, tx *sql.Tx) (time.Duration, error) {
	startedAt := time.Now()
	lockContext, cancel := context.WithTimeout(ctx, s.migrationLockTimeout)
	defer cancel()
	waitingReported := false
	for {
		var acquired bool
		if err := tx.QueryRowContext(lockContext, `SELECT pg_try_advisory_xact_lock(hashtext($1))`, "openseal:migrate:"+s.schema).Scan(&acquired); err != nil {
			if errors.Is(lockContext.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				waited := time.Since(startedAt)
				s.observeMigration(PostgresMigrationEvent{Phase: PostgresMigrationTimeout, WaitDuration: waited})
				return waited, fmt.Errorf("%w after %s", ErrPostgresMigrationLockTimeout, waited.Round(time.Millisecond))
			}
			return time.Since(startedAt), err
		}
		if acquired {
			waited := time.Since(startedAt)
			s.observeMigration(PostgresMigrationEvent{Phase: PostgresMigrationAcquired, WaitDuration: waited})
			return waited, nil
		}
		if !waitingReported {
			waitingReported = true
			s.observeMigration(PostgresMigrationEvent{Phase: PostgresMigrationWaiting})
		}
		timer := time.NewTimer(s.migrationLockPollInterval)
		select {
		case <-lockContext.Done():
			if !timer.Stop() {
				<-timer.C
			}
			waited := time.Since(startedAt)
			if errors.Is(lockContext.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				s.observeMigration(PostgresMigrationEvent{Phase: PostgresMigrationTimeout, WaitDuration: waited})
				return waited, fmt.Errorf("%w after %s", ErrPostgresMigrationLockTimeout, waited.Round(time.Millisecond))
			}
			return waited, lockContext.Err()
		case <-timer.C:
		}
	}
}

func (s *PostgresStore) observeMigration(event PostgresMigrationEvent) {
	if s == nil || s.migrationObserver == nil {
		return
	}
	func() {
		defer func() { _ = recover() }()
		s.migrationObserver(event)
	}()
}

func (s *PostgresStore) CreateRun(ctx context.Context, workflow WorkflowEntry, triggerData map[string]interface{}) (int, error) {
	workflowJSON, err := json.Marshal(workflow)
	if err != nil {
		return 0, fmt.Errorf("marshal workflow snapshot: %w", err)
	}
	triggerJSON, err := json.Marshal(triggerData)
	if err != nil {
		return 0, fmt.Errorf("marshal trigger data: %w", err)
	}
	var runID int
	err = s.db.QueryRowContext(ctx, `INSERT INTO `+s.table("runs")+`
		(workflow_name, workflow_json, trigger_data, status)
		VALUES ($1, $2::jsonb, $3::jsonb, $4) RETURNING run_id`,
		workflow.Name, string(workflowJSON), string(triggerJSON), RunStatusPending).Scan(&runID)
	if err != nil {
		return 0, fmt.Errorf("insert run: %w", err)
	}
	return runID, nil
}

func (s *PostgresStore) GetRun(ctx context.Context, runID int) (*RunRecord, error) {
	run, err := scanRun(s.db.QueryRowContext(ctx, s.runSelect()+` WHERE run_id = $1`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("select run: %w", err)
	}
	return run, nil
}

func (s *PostgresStore) ClaimNextRunnable(ctx context.Context, workerID string, leaseDuration time.Duration) (*RunRecord, error) {
	if strings.TrimSpace(workerID) == "" || leaseDuration <= 0 {
		return nil, errors.New("worker id and positive lease duration are required")
	}
	leaseMicros := leaseDuration.Microseconds()
	query := `WITH candidate AS (
		SELECT run_id FROM ` + s.table("runs") + `
		WHERE ((status IN ($1, $2) AND available_at <= CURRENT_TIMESTAMP)
		   OR (status = $3 AND lease_expires_at IS NOT NULL AND lease_expires_at <= CURRENT_TIMESTAMP))
		ORDER BY available_at ASC, run_id ASC
		FOR UPDATE SKIP LOCKED LIMIT 1
	)
	UPDATE ` + s.table("runs") + ` AS run SET
		status = $3, lease_owner = $4,
		lease_expires_at = CURRENT_TIMESTAMP + ($5 * INTERVAL '1 microsecond'),
		started_at = COALESCE(run.started_at, CURRENT_TIMESTAMP),
		updated_at = CURRENT_TIMESTAMP, completed_at = NULL
	FROM candidate WHERE run.run_id = candidate.run_id
	RETURNING run.run_id, run.workflow_name, run.workflow_json, run.trigger_data, run.status, run.node_results,
		run.created_at, run.started_at, run.updated_at, run.available_at, run.lease_owner,
		run.lease_expires_at, run.completed_at, run.error, run.retry_count`
	run, err := scanRun(s.db.QueryRowContext(ctx, query, RunStatusPending, RunStatusRetrying, RunStatusRunning, workerID, leaseMicros))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim runnable run: %w", err)
	}
	return run, nil
}

func (s *PostgresStore) RenewLease(ctx context.Context, runID int, workerID string, leaseDuration time.Duration) error {
	if leaseDuration <= 0 {
		return errors.New("positive lease duration is required")
	}
	return expectOne(s.db.ExecContext(ctx, `UPDATE `+s.table("runs")+`
		SET lease_expires_at = CURRENT_TIMESTAMP + ($1 * INTERVAL '1 microsecond'), updated_at = CURRENT_TIMESTAMP
		WHERE run_id = $2 AND status = $3 AND lease_owner = $4`, leaseDuration.Microseconds(), runID, RunStatusRunning, workerID))
}

func (s *PostgresStore) UpdateNodeResult(ctx context.Context, runID int, workerID, nodeID string, result *executor.NodeResult) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT node_results FROM `+s.table("runs")+`
		WHERE run_id = $1 AND status = $2 AND lease_owner = $3 FOR UPDATE`, runID, RunStatusRunning, workerID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	}
	if err != nil {
		return err
	}
	results := make(map[string]*executor.NodeResult)
	if err := json.Unmarshal(raw, &results); err != nil {
		return fmt.Errorf("decode node results: %w", err)
	}
	results[nodeID] = result
	encoded, err := json.Marshal(results)
	if err != nil {
		return err
	}
	if err := expectOne(tx.ExecContext(ctx, `UPDATE `+s.table("runs")+`
		SET node_results = $1::jsonb, updated_at = CURRENT_TIMESTAMP
		WHERE run_id = $2 AND status = $3 AND lease_owner = $4`, string(encoded), runID, RunStatusRunning, workerID)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) CompleteRun(ctx context.Context, runID int, workerID, status string, runErr error) error {
	return expectOne(s.db.ExecContext(ctx, `UPDATE `+s.table("runs")+`
		SET status = $1, completed_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP,
			error = $2, lease_owner = '', lease_expires_at = NULL
		WHERE run_id = $3 AND status = $4 AND lease_owner = $5`, status, errorString(runErr), runID, RunStatusRunning, workerID))
}

func (s *PostgresStore) ScheduleRetry(ctx context.Context, runID int, workerID string, availableAt time.Time, runErr error) error {
	return expectOne(s.db.ExecContext(ctx, `UPDATE `+s.table("runs")+`
		SET status = $1, retry_count = retry_count + 1, available_at = $2,
			updated_at = CURRENT_TIMESTAMP, error = $3, lease_owner = '', lease_expires_at = NULL
		WHERE run_id = $4 AND status = $5 AND lease_owner = $6`, RunStatusRetrying, availableAt, errorString(runErr), runID, RunStatusRunning, workerID))
}

func (s *PostgresStore) ListRuns(ctx context.Context, limit int) ([]*RunRecord, error) {
	query := s.runSelect() + ` ORDER BY run_id DESC`
	args := make([]interface{}, 0, 1)
	if limit > 0 {
		query += ` LIMIT $1`
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()
	results := make([]*RunRecord, 0)
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, run)
	}
	return results, rows.Err()
}

func (s *PostgresStore) Close() error { return s.db.Close() }

func (s *PostgresStore) quotedSchema() string { return `"` + s.schema + `"` }

func (s *PostgresStore) table(name string) string { return s.quotedSchema() + `.` + `"` + name + `"` }

func (s *PostgresStore) runSelect() string {
	return `SELECT run_id, workflow_name, workflow_json, trigger_data, status, node_results,
		created_at, started_at, updated_at, available_at, lease_owner, lease_expires_at,
		completed_at, error, retry_count FROM ` + s.table("runs")
}
