package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// Store test: each store keeps a Run's ActingUser fixed, and the postgres
// subtest also checks the Postgres store's own in-transaction update. It
// moves with the stores in U2.8/U2.9, one copy per backend: the postgres
// subtest (with actingUserPostgresStore) to store/postgres, the sqlite one to
// store/sqlite.

// TestAgentRunActingUserIsImmutable runs the acting-user contract on each
// store; on Postgres it also checks the store's in-transaction Run update.
func TestAgentRunActingUserIsImmutable(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "acting-user.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		storetest.RunActingUserContract(t, store)
	})
	t.Run("postgres", func(t *testing.T) {
		postgres := actingUserPostgresStore(t)
		stored := storetest.RunActingUserContract(t, postgres)
		ctx := context.Background()
		stored.ActingUser = "8"
		expected := stored.Revision
		stored.Revision++
		tx, err := postgres.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := postgres.updateAgentRunTx(ctx, tx, stored, expected); !errors.Is(err, models.ErrRevisionConflict) {
			t.Fatalf("updateAgentRunTx changing ActingUser error = %v", err)
		}
	})
}

func actingUserPostgresStore(t *testing.T) *PostgresStore {
	t.Helper()
	pgSchema := storetest.NewPostgresSchema(t, "openseal_acting_user_")
	dsn := pgSchema.DSN
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(pgSchema.Name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}
