package runtime

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

// testPostgresSchema is a Postgres schema a test owns, with the test's own
// connection to the database for the SQL no store method offers: seeding a
// legacy layout, reading a raw row, checking the migration ledger. The test
// never reaches into a store's connection.
type testPostgresSchema struct {
	dsn  string
	name string
	db   *sql.DB
}

// quoted returns the schema name quoted for SQL.
func (s testPostgresSchema) quoted() string { return `"` + s.name + `"` }

// table returns the quoted, schema-qualified name of a table.
func (s testPostgresSchema) table(name string) string { return s.quoted() + `."` + name + `"` }

// open opens another store on the schema, closed when the test ends.
func (s testPostgresSchema) open(t testing.TB, options ...PostgresStoreOption) *PostgresStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, err := NewPostgresStore(ctx, s.dsn, append([]PostgresStoreOption{WithPostgresSchema(s.name)}, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newTestPostgresSchema names a fresh schema prefix+<random> and opens the
// test's connection. It skips the test when OPENSEAL_TEST_POSTGRES_DSN is not
// set, and drops the schema when the test ends. No store is opened: a test
// can lay out tables first and open stores on the schema afterwards.
func newTestPostgresSchema(t testing.TB, prefix string) testPostgresSchema {
	t.Helper()
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := testPostgresSchema{dsn: dsn, name: prefix + uuid.NewString()[:8], db: db}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema.quoted()+` CASCADE`)
		_ = db.Close()
	})
	return schema
}

// openTestPostgresStore opens a Postgres store on a fresh schema (see
// newTestPostgresSchema) and returns it with the schema.
func openTestPostgresStore(t testing.TB, prefix string, options ...PostgresStoreOption) (*PostgresStore, testPostgresSchema) {
	t.Helper()
	schema := newTestPostgresSchema(t, prefix)
	return schema.open(t, options...), schema
}
