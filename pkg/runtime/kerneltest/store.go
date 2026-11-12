// Package kerneltest opens kernel stores for tests: in-memory and file
// SQLite stores, the test's own connection to a SQLite file, and Postgres
// stores on fresh schemas.
package kerneltest

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// NewStore returns an empty in-memory kernel store that is closed when the
// test ends.
func NewStore(t testing.TB) *sqlite.Store {
	t.Helper()
	store, err := sqlite.NewInMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// OpenSQLiteDB opens the test's own connection to the SQLite file at path,
// closed when the test ends.
func OpenSQLiteDB(t testing.TB, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// NewSQLiteFileStore opens a SQLite store on a fresh file and the test's own
// connection to that file, both closed when the test ends. Use it instead of
// NewStore when the test reads or writes rows no store method offers.
func NewSQLiteFileStore(t testing.TB) (*sqlite.Store, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, OpenSQLiteDB(t, path)
}

// OpenPostgresOn opens another store on schema s, closed when the test ends.
func OpenPostgresOn(t testing.TB, s storetest.PostgresSchema, options ...postgres.Option) *postgres.Store {
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

// OpenPostgres opens a Postgres store on a fresh schema (see
// storetest.NewPostgresSchema) and returns it with the schema.
func OpenPostgres(t testing.TB, prefix string, options ...postgres.Option) (*postgres.Store, storetest.PostgresSchema) {
	t.Helper()
	schema := storetest.NewPostgresSchema(t, prefix)
	return OpenPostgresOn(t, schema, options...), schema
}

// postgresHead caches the schema version a fresh schema migrates to.
var postgresHead struct {
	sync.Mutex
	version int64
}

// PostgresSchemaHead returns the version a store reaches when it migrates an
// empty schema: the head of this binary's migrations. Tests outside the
// store compare an upgraded schema with it; the store's own tests pin the
// number itself.
func PostgresSchemaHead(t testing.TB) int64 {
	t.Helper()
	postgresHead.Lock()
	defer postgresHead.Unlock()
	if postgresHead.version == 0 {
		store, _ := OpenPostgres(t, "openseal_head_")
		version, err := store.PostgresSchemaVersion(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		postgresHead.version = version
	}
	return postgresHead.version
}
