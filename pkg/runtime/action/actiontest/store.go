package actiontest

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// NewStore returns an empty in-memory kernel store that is closed when the
// test ends. It is a copy of runtime's newTestKernelStore (runtime's own tests
// cannot import this package without a cycle); change both.
func NewStore(t testing.TB) *sqlite.Store {
	t.Helper()
	store, err := sqlite.NewInMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// StoreBackend is one kernel store backend a test runs on.
type StoreBackend struct {
	Name string
	Open func(*testing.T) models.KernelStore
}

// Stores is SQLite, and Postgres when OPENSEAL_TEST_POSTGRES_DSN is set.
func Stores(t *testing.T) []StoreBackend {
	stores := []StoreBackend{
		{Name: "sqlite", Open: func(t *testing.T) models.KernelStore {
			store, err := sqlite.New(filepath.Join(t.TempDir(), "nested.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	}
	if dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN"); dsn != "" {
		stores = append(stores, StoreBackend{Name: "postgres", Open: func(t *testing.T) models.KernelStore {
			store, _ := openTestPostgresStore(t, "openseal_nested_")
			return store
		}})
	}
	return stores
}

// openPostgresStoreOn opens another store on schema s, closed when the
// test ends.
func openPostgresStoreOn(t testing.TB, s storetest.PostgresSchema, options ...postgres.Option) *postgres.Store {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, err := postgres.New(ctx, s.DSN, append([]postgres.Option{postgres.WithSchema(s.Name)}, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// openTestPostgresStore opens a Postgres store on a fresh schema (see
// storetest.NewPostgresSchema) and returns it with the schema.
func openTestPostgresStore(t testing.TB, prefix string, options ...postgres.Option) (*postgres.Store, storetest.PostgresSchema) {
	t.Helper()
	schema := storetest.NewPostgresSchema(t, prefix)
	return openPostgresStoreOn(t, schema, options...), schema
}

// openTestSQLiteDB opens the test's own connection to the SQLite file at
// path, closed when the test ends.
func openTestSQLiteDB(t testing.TB, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
