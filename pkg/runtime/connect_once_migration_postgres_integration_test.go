//go:build integration

package runtime

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/google/uuid"
)

func TestPostgresConnectOnceMigration(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN")
	}
	ctx := context.Background()
	schema := "connect_once_" + uuid.NewString()[:8]
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
		_ = store.Close()
	})
	h := connectOnceMigrationHarness{
		db:    func() *sql.DB { return store.db },
		table: func(name string) string { return store.table(name) },
		store: func() connectOnceMigrationStore { return store },
		rerun: func(t *testing.T) {
			t.Helper()
			if _, err := store.db.ExecContext(ctx, `DELETE FROM `+store.table("schema_migrations")+` WHERE version = $1`, connectOnceMigrationVersion); err != nil {
				t.Fatal(err)
			}
		},
	}
	runConnectOnceMigrationCase(t, h, func(t *testing.T) {
		t.Helper()
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if store, err = NewPostgresStore(ctx, dsn, WithPostgresSchema(schema)); err != nil {
			t.Fatal(err)
		}
	})
}
