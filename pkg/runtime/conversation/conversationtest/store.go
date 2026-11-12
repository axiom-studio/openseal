package conversationtest

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// SeedSpace gives the empty owner a default space in the scope, so a
// conversation created with no space and no person behind it has one to go
// to. An owner who already has a default space keeps it, so a fixture that
// is built more than once on a store can seed every time.
func SeedSpace(t testing.TB, store models.SpaceStore, scope models.Scope) *models.Space {
	t.Helper()
	if existing, err := store.DefaultSpace(context.Background(), scope, ""); err == nil {
		return existing
	} else if !errors.Is(err, models.ErrNoDefaultSpace) {
		t.Fatalf("seed space: %v", err)
	}
	space, err := portfolio.NewSpaceService(store).CreateSpace(context.Background(), portfolio.CreateSpaceRequest{
		Scope: scope, Name: "Home", Look: models.DefaultSpaceLook,
	})
	if err != nil {
		t.Fatalf("seed space: %v", err)
	}
	return space
}

type sqlStore = storetest.ConversationTaskStore

// forSQLStores runs test on a SQLite file store and on a Postgres schema
// (skipped without OPENSEAL_TEST_POSTGRES_DSN), each with a function that
// opens the same database again.
func forSQLStores(t *testing.T, test func(t *testing.T, store sqlStore, reopen func(*testing.T) sqlStore)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "conversation-tasks.db")
		open := func(t *testing.T) sqlStore {
			store, err := sqlite.New(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}
		test(t, open(t), open)
	})
	t.Run("postgres", func(t *testing.T) {
		pgSchema := storetest.NewPostgresSchema(t, "openseal_task_sql_")
		dsn := pgSchema.DSN
		schema := pgSchema.Name
		open := func(t *testing.T) sqlStore {
			store, err := postgres.New(t.Context(), dsn, postgres.WithSchema(schema))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}
		store := open(t)
		t.Cleanup(func() {
			cleanup, err := sql.Open("postgres", dsn)
			if err != nil {
				return
			}
			defer cleanup.Close()
			_, _ = cleanup.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+pgSchema.Quoted()+` CASCADE`)
		})
		test(t, store, open)
	})
}
