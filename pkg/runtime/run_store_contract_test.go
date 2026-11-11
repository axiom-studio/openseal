package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
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
		}})
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("set OPENSEAL_TEST_POSTGRES_DSN for the shared PostgreSQL contract")
		}
		schema := "run_store_contract_" + uuid.NewString()[:8]
		current, err := NewPostgresStore(t.Context(), dsn, WithPostgresSchema(schema))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = current.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+current.quotedSchema()+` CASCADE`)
			_ = current.Close()
		})
		test(t, runStoreContractFixture{store: current, reopen: func() runStoreContractStore {
			if err := current.Close(); err != nil {
				t.Fatal(err)
			}
			current, err = NewPostgresStore(t.Context(), dsn, WithPostgresSchema(schema))
			if err != nil {
				t.Fatal(err)
			}
			return current
		}})
	})
}

var runStoreContractEpoch = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
