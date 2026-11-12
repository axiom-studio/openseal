package runtime

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// These contracts use the same public commands and durable scheduling boundary
// on the SQLite and PostgreSQL stores.
type runStoreContractStore interface {
	RunCommandStore
	AgentRunScheduleStore
}

type runStoreContractFixture struct {
	store  runStoreContractStore
	reopen func() runStoreContractStore
	// db is the test's own connection to the store's database, for rows no
	// store method reads or writes; table names a table for SQL on it. Both
	// stay valid across reopen.
	db    *sql.DB
	table func(name string) string
}

func runStoreContractFixtures(t *testing.T, test func(*testing.T, runStoreContractFixture)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "contract.db")
		current, err := NewSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = current.Close() })
		test(t, runStoreContractFixture{store: current, reopen: func() runStoreContractStore {
			if err := current.Close(); err != nil {
				t.Fatal(err)
			}
			current, err = NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			return current
		}, db: openTestSQLiteDB(t, path), table: func(name string) string { return name }})
	})
	t.Run("postgres", func(t *testing.T) {
		pgSchema := storetest.NewPostgresSchema(t, "run_store_contract_")
		schema, dsn := pgSchema.Name, pgSchema.DSN
		current, err := NewPostgresStore(t.Context(), dsn, WithPostgresSchema(schema))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = current.Close() })
		test(t, runStoreContractFixture{store: current, reopen: func() runStoreContractStore {
			if err := current.Close(); err != nil {
				t.Fatal(err)
			}
			current, err = NewPostgresStore(t.Context(), dsn, WithPostgresSchema(schema))
			if err != nil {
				t.Fatal(err)
			}
			return current
		}, db: pgSchema.DB, table: pgSchema.Table})
	})
}

var runStoreContractEpoch = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
