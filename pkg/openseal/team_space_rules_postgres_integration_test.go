//go:build integration

package openseal

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/google/uuid"
)

func TestPostgresTeamRostersFollowSpaces(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	schema := "openseal_team_spaces_" + uuid.NewString()[:8]
	store, err := postgres.New(t.Context(), dsn, postgres.WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = store.Close()
		if cleanup, err := sql.Open("postgres", dsn); err == nil {
			_, _ = cleanup.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`)
			_ = cleanup.Close()
		}
	})
	engine, err := New(WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	exerciseTeamSpaceRules(t, engine)
}
