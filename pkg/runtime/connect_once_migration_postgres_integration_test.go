//go:build integration

package runtime

import (
	"context"
	"database/sql"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestPostgresConnectOnceMigration(t *testing.T) {
	pgSchema := storetest.NewPostgresSchema(t, "connect_once_")
	dsn := pgSchema.DSN
	schema := pgSchema.Name
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pgSchema.DB.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+pgSchema.Quoted()+` CASCADE`)
		_ = store.Close()
	})
	h := storetest.ConnectOnceMigrationHarness{
		DB:    func() *sql.DB { return pgSchema.DB },
		Table: func(name string) string { return pgSchema.Table(name) },
		Store: func() storetest.ConnectOnceMigrationStore { return store },
		Rerun: func(t *testing.T) {
			t.Helper()
			if _, err := pgSchema.DB.ExecContext(ctx, `DELETE FROM `+pgSchema.Table("schema_migrations")+` WHERE version = $1`, connectOnceMigrationVersion); err != nil {
				t.Fatal(err)
			}
		},
	}
	storetest.RunConnectOnceMigrationCase(t, h, func(t *testing.T) {
		t.Helper()
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if store, err = NewPostgresStore(ctx, dsn, WithPostgresSchema(schema)); err != nil {
			t.Fatal(err)
		}
	})
}
