package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// Store test: the Postgres store keeps a Run's ActingUser fixed, also in its
// own in-transaction update. The SQLite half of this test is in
// store/sqlite.

// TestAgentRunActingUserIsImmutable runs the acting-user contract on the
// Postgres store and checks its in-transaction Run update too.
func TestAgentRunActingUserIsImmutable(t *testing.T) {
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

func actingUserPostgresStore(t *testing.T) *Store {
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
