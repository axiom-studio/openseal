package runtime

import (
	"database/sql"
	"fmt"

	_ "github.com/mattn/go-sqlite3"
)

// SQLiteStore persists canonical kernel state in SQLite.
type SQLiteStore struct {
	db *sql.DB
}

// NewSQLiteStore opens (and creates) a SQLite-backed canonical kernel store.
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
	if err := migratePortfolio(db); err != nil {
		return err
	}
	if err := migrateRunbookActivationsSQLite(db); err != nil {
		return err
	}
	if err := migrateInitiatives(db); err != nil {
		return err
	}
	if err := migrateSourceMonitors(db); err != nil {
		return err
	}
	if err := migrateSourcePolicies(db); err != nil {
		return err
	}
	if err := migrateEventSourceCheckpoints(db); err != nil {
		return err
	}
	if err := migrateEventSourceSubscriptions(db); err != nil {
		return err
	}
	if err := migrateOutreach(db); err != nil {
		return err
	}
	if err := migrateAgentRegistry(db); err != nil {
		return err
	}
	if err := migrateTeamRegistry(db); err != nil {
		return err
	}
	if err := migrateSkillCatalog(db); err != nil {
		return err
	}
	if err := migrateSkillSourceArtifacts(db); err != nil {
		return err
	}
	if err := migrateActionCredentialLeaseRedemptionsSQLite(db); err != nil {
		return err
	}
	if err := migrateAuthoringChangeSets(db); err != nil {
		return err
	}
	if err := migrateSensitiveAuthoringPromptsSQLite(db); err != nil {
		return err
	}
	if err := migrateNaturalTeamCoordinationSQLite(db); err != nil {
		return err
	}
	if err := migrateTeamCoordinationDefaultsSQLite(db); err != nil {
		return err
	}
	if err := migrateActivationContinuationsSQLite(db); err != nil {
		return err
	}
	return migrateSkillBindingLifecycleRepairSQLite(db)
}

// Close closes the underlying database.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}
