//go:build integration

package runtime

import (
	"context"
	"database/sql"
	"testing"
)

func TestPostgresConnectOnceMigration(t *testing.T) {
	pgSchema := newTestPostgresSchema(t, "connect_once_")
	dsn := pgSchema.dsn
	schema := pgSchema.name
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pgSchema.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+pgSchema.quoted()+` CASCADE`)
		_ = store.Close()
	})
	h := connectOnceMigrationHarness{
		db:    func() *sql.DB { return pgSchema.db },
		table: func(name string) string { return pgSchema.table(name) },
		store: func() connectOnceMigrationStore { return store },
		rerun: func(t *testing.T) {
			t.Helper()
			if _, err := pgSchema.db.ExecContext(ctx, `DELETE FROM `+pgSchema.table("schema_migrations")+` WHERE version = $1`, connectOnceMigrationVersion); err != nil {
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
