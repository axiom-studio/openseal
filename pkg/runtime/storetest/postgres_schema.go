package storetest

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/google/uuid"
	_ "github.com/lib/pq" // the "postgres" driver the schema connection opens
)

// PostgresSchema is a Postgres schema a test owns, with the test's own
// connection to the database for the SQL no store method offers: seeding a
// legacy layout, reading a raw row, checking the migration ledger. The test
// never reaches into a store's connection.
type PostgresSchema struct {
	DSN  string
	Name string
	DB   *sql.DB
}

// Quoted returns the schema name quoted for SQL.
func (s PostgresSchema) Quoted() string { return `"` + s.Name + `"` }

// Table returns the quoted, schema-qualified name of a table.
func (s PostgresSchema) Table(name string) string { return s.Quoted() + `."` + name + `"` }

// NewPostgresSchema names a fresh schema prefix+<random> and opens the
// test's connection. It skips the test when OPENSEAL_TEST_POSTGRES_DSN is not
// set, and drops the schema when the test ends. No store is opened: a test
// can lay out tables first and open stores on the schema afterwards.
func NewPostgresSchema(t testing.TB, prefix string) PostgresSchema {
	t.Helper()
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := PostgresSchema{DSN: dsn, Name: prefix + uuid.NewString()[:8], DB: db}
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+schema.Quoted()+` CASCADE`)
		_ = db.Close()
	})
	return schema
}
