// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/runtime/store/postgres. Remove once in-flight branches have landed.
//
// Re-exports of the Postgres store, kept so that importers of pkg/runtime
// continue to compile.

package runtime

import (
	"context"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
)

var ErrPostgresMigrationLockTimeout = postgres.ErrPostgresMigrationLockTimeout

type PostgresMigrationPhase = postgres.PostgresMigrationPhase

const PostgresMigrationWaiting = postgres.PostgresMigrationWaiting

const PostgresMigrationAcquired = postgres.PostgresMigrationAcquired

const PostgresMigrationCurrent = postgres.PostgresMigrationCurrent

const PostgresMigrationComplete = postgres.PostgresMigrationComplete

const PostgresMigrationTimeout = postgres.PostgresMigrationTimeout

type PostgresMigrationEvent = postgres.PostgresMigrationEvent

type PostgresMigrationObserver = postgres.PostgresMigrationObserver

type PostgresMigrationStats = postgres.PostgresMigrationStats

type PostgresStoreOption = postgres.PostgresStoreOption

type PostgresPoolConfig = postgres.PostgresPoolConfig

// DefaultPostgresPoolConfig forwards to postgres.DefaultPostgresPoolConfig.
func DefaultPostgresPoolConfig() postgres.PostgresPoolConfig {
	return postgres.DefaultPostgresPoolConfig()
}

// WithPostgresPool forwards to postgres.WithPostgresPool.
func WithPostgresPool(pool postgres.PostgresPoolConfig) postgres.PostgresStoreOption {
	return postgres.WithPostgresPool(pool)
}

type PostgresPoolStats = postgres.PostgresPoolStats

// WithPostgresSchema forwards to postgres.WithPostgresSchema.
func WithPostgresSchema(schema string) postgres.PostgresStoreOption {
	return postgres.WithPostgresSchema(schema)
}

// WithPostgresMigrationLock forwards to postgres.WithPostgresMigrationLock.
func WithPostgresMigrationLock(timeout, pollInterval time.Duration) postgres.PostgresStoreOption {
	return postgres.WithPostgresMigrationLock(timeout, pollInterval)
}

// WithPostgresMigrationObserver forwards to postgres.WithPostgresMigrationObserver.
func WithPostgresMigrationObserver(observer postgres.PostgresMigrationObserver) postgres.PostgresStoreOption {
	return postgres.WithPostgresMigrationObserver(observer)
}

type PostgresStore = postgres.Store

// NewPostgresStore forwards to postgres.NewPostgresStore.
func NewPostgresStore(ctx context.Context, dsn string, options ...postgres.PostgresStoreOption) (*postgres.Store, error) {
	return postgres.NewPostgresStore(ctx, dsn, options...)
}

type WorkspaceSearchHit = postgres.WorkspaceSearchHit
