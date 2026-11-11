package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"sync/atomic"

	_ "github.com/mattn/go-sqlite3"
)

// SQLiteStore persists canonical kernel state in SQLite.
type SQLiteStore struct {
	db *sql.DB
	// memoryHold keeps an in-memory database alive: SQLite frees a memdb
	// database when its last connection closes, and the pool may close idle
	// connections at any time. Nil for file-backed stores.
	memoryHold *sql.Conn
}

// sqliteBusyTimeout makes a connection wait for a competing lock instead of
// failing at once; the pool holds several connections to one database.
const sqliteBusyTimeout = "_busy_timeout=5000"

// NewSQLiteStore opens (and creates) a SQLite-backed canonical kernel store.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite3", path+"?"+sqliteBusyTimeout+"&_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

var inMemorySQLiteStores atomic.Uint64

// NewInMemorySQLiteStore opens a SQLiteStore whose database lives only in
// process memory and is discarded by Close.
//
// It uses SQLite's memdb VFS under a name unique to this store. A memdb name
// starting with "/" is one database shared by every connection in the
// process, with SQLite's normal locking, so the pool behaves as it does for a
// file and the busy timeout applies. (A plain ":memory:" path gives each
// pooled connection its own empty database, and a shared-cache database
// fails concurrent access with "table is locked" instead of waiting.) Memdb
// has no WAL: a writer waits for open readers to finish, up to the busy
// timeout, where a file store in WAL mode would not.
func NewInMemorySQLiteStore() (*SQLiteStore, error) {
	name := fmt.Sprintf("file:/openseal-kernel-%d?vfs=memdb&%s", inMemorySQLiteStores.Add(1), sqliteBusyTimeout)
	db, err := sql.Open("sqlite3", name)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	hold, err := db.Conn(context.Background())
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := migrate(db); err != nil {
		hold.Close()
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &SQLiteStore{db: db, memoryHold: hold}, nil
}

func migrate(db *sql.DB) error {
	if err := migrateSkillSetupRequests(db); err != nil {
		return err
	}
	if err := migrateCredentialRequests(db); err != nil {
		return err
	}
	if err := migratePortfolio(db); err != nil {
		return err
	}
	if err := migrateRunTerminalReportingSQLite(db); err != nil {
		return err
	}
	if err := migrateRunbookActivationsSQLite(db); err != nil {
		return err
	}
	if err := migrateRemoveEventSourcesSQLite(db); err != nil {
		return err
	}
	if err := migrateRoutineEventsSQLite(db); err != nil {
		return err
	}
	if err := migrateRemoveEventWaitsSQLite(db); err != nil {
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
	if err := migrateSkillUpgradeDrainSQLite(db); err != nil {
		return err
	}
	if err := migrateSkillRuntimeMaintenanceSQLite(db); err != nil {
		return err
	}
	if err := migrateRunSkillDependenciesSQLite(db); err != nil {
		return err
	}
	if err := migrateSkillSourceArtifacts(db); err != nil {
		return err
	}
	if err := migrateActionCredentialLeaseRedemptionsSQLite(db); err != nil {
		return err
	}
	if err := migrateTeamRoutingSQLite(db); err != nil {
		return err
	}
	if err := migrateSkillBindingLifecycleRepairSQLite(db); err != nil {
		return err
	}
	if err := migrateExternalConversationGatewayAuthoritySQLite(db); err != nil {
		return err
	}
	if err := migrateCallbackRegistrySQLite(db); err != nil {
		return err
	}
	if err := migrateConversationTasksSQLite(db); err != nil {
		return err
	}
	if err := migrateEmbedInstallationsSQLite(db); err != nil {
		return err
	}
	if err := migrateSpacesSQLite(db); err != nil {
		return err
	}
	if err := migrateDeploymentIdentitiesSQLite(db); err != nil {
		return err
	}
	if err := migrateTeamSkillGrantRemovalSQLite(db); err != nil {
		return err
	}
	if err := migrateInteractionUmbrellasSQLite(db); err != nil {
		return err
	}
	if err := migrateChannelUmbrellasSQLite(db); err != nil {
		return err
	}
	if err := migrateConnectOnceSQLite(db); err != nil {
		return err
	}
	if err := migrateAgentSessionLogSQLite(db); err != nil {
		return err
	}
	if err := migrateAgentBuilderSQLite(db); err != nil {
		return err
	}
	if err := migrateAgentBuilderDraftsSQLite(db); err != nil {
		return err
	}
	return migrateRemoveKernelProjectsSQLite(db)
}

// Close closes the underlying database. An in-memory database is freed.
func (s *SQLiteStore) Close() error {
	if s.memoryHold != nil {
		s.memoryHold.Close()
	}
	return s.db.Close()
}
