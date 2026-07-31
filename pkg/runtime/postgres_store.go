package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

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
	PostgresMigrationCurrent  PostgresMigrationPhase = "current"
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
	current, currentSchemaVersion, err := s.postgresSchemaCurrent(ctx, tx)
	if err != nil {
		return err
	}
	if current {
		if err := tx.Commit(); err != nil {
			return err
		}
		s.recordMigrationOutcome(PostgresMigrationCurrent, PostgresMigrationStats{
			WaitDuration: waitDuration, TotalDuration: time.Since(startedAt), SchemaVersion: currentSchemaVersion,
		})
		return nil
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+` (version, name) VALUES (1, 'initial kernel schema') ON CONFLICT (version) DO NOTHING`); err != nil {
		return err
	}
	if err := s.migrateProjectContract(ctx, tx); err != nil {
		return err
	}
	if err := s.migratePortfolio(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateProjects(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateProjectRunRefs(ctx, tx); err != nil {
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
	if err := s.migrateActivityProjectionRepair(ctx, tx); err != nil {
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
	if err := s.migrateExternalConversations(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateExternalConversationIngressRoutes(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateExternalConversationGateways(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateAuthoringChangeSets(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateSensitiveAuthoringPrompts(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateNaturalTeamCoordination(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateTeamCoordinationDefaults(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateActivationContinuations(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateSkillBindingLifecycleRepair(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateSourceQualifiedSkillVariants(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateExternalOperationReceipts(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateRunbookActivations(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateCallbackRegistry(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateTerminalActivityLookup(ctx, tx); err != nil {
		return err
	}
	if err := s.migrateTerminalRunScan(ctx, tx); err != nil {
		return err
	}
	var schemaVersion int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM `+s.table("schema_migrations")).Scan(&schemaVersion); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.recordMigrationOutcome(PostgresMigrationComplete, PostgresMigrationStats{
		WaitDuration: waitDuration, TotalDuration: time.Since(startedAt), SchemaVersion: schemaVersion,
	})
	return nil
}

type postgresMigrationQuerier interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

func (s *PostgresStore) postgresMigrationApplied(ctx context.Context, query postgresMigrationQuerier, version int64) (bool, error) {
	var applied bool
	err := query.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM `+s.table("schema_migrations")+` WHERE version = $1
	)`, version).Scan(&applied)
	return applied, err
}

// postgresSchemaCurrent verifies the complete contiguous migration ledger
// while the caller holds migration leadership. A highest-version-only check
// could hide a partially restored or manually damaged ledger.
func (s *PostgresStore) postgresSchemaCurrent(ctx context.Context, query postgresMigrationQuerier) (bool, int64, error) {
	var relation sql.NullString
	if err := query.QueryRowContext(ctx, `SELECT to_regclass($1)`, s.schema+".schema_migrations").Scan(&relation); err != nil {
		return false, 0, err
	}
	if !relation.Valid {
		return false, 0, nil
	}
	var knownVersions, schemaVersion int64
	if err := query.QueryRowContext(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE version BETWEEN 1 AND $1),
			COALESCE(MAX(version), 0)
		FROM `+s.table("schema_migrations"), currentPostgresSchemaVersion).Scan(&knownVersions, &schemaVersion); err != nil {
		return false, 0, err
	}
	return knownVersions == currentPostgresSchemaVersion, schemaVersion, nil
}

func (s *PostgresStore) recordMigrationOutcome(phase PostgresMigrationPhase, stats PostgresMigrationStats) {
	s.migrationMu.Lock()
	s.migrationStats = stats
	s.migrationMu.Unlock()
	s.observeMigration(PostgresMigrationEvent{
		Phase: phase, WaitDuration: stats.WaitDuration, TotalDuration: stats.TotalDuration, SchemaVersion: stats.SchemaVersion,
	})
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

func (s *PostgresStore) Close() error { return s.db.Close() }

func (s *PostgresStore) quotedSchema() string { return `"` + s.schema + `"` }

func (s *PostgresStore) table(name string) string { return s.quotedSchema() + `.` + `"` + name + `"` }
