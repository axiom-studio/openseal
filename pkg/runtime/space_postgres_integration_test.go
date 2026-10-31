//go:build integration

package runtime

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
)

func openSpacesPostgresStore(t *testing.T, prefix string) (*PostgresStore, string, string) {
	t.Helper()
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	schema := prefix + uuid.NewString()[:8]
	store, err := NewPostgresStore(context.Background(), dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
		_ = store.Close()
	})
	return store, dsn, schema
}

func TestPostgresSpaceStore(t *testing.T) {
	store, _, _ := openSpacesPostgresStore(t, "openseal_spaces_")
	if version, err := store.PostgresSchemaVersion(context.Background()); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}
	exerciseSpaceStore(t, store)
}

func TestPostgresSpacesBackfillAndRollback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store, dsn, schema := openSpacesPostgresStore(t, "openseal_spaces_backfill_")
	scope := Scope{Kind: "tenant", ID: "legacy"}
	agentOnly := Scope{Kind: "tenant", ID: "agents-only"}
	conversation, _, err := NewConversationService(store).CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "helper"}, Title: "Old chat", IdempotencyKey: "old",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RollbackPostgresMigrations(ctx, spacesMigrationVersion-1); err != nil {
		t.Fatal(err)
	}
	var hasColumn bool
	if err := store.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'conversations' AND column_name = 'space_id')`, schema).Scan(&hasColumn); err != nil || hasColumn {
		t.Fatalf("space_id after rollback = %v, err = %v", hasColumn, err)
	}
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		table string
		scope Scope
		id    string
		at    time.Time
	}{
		{"agent_deployments", scope, "helper", now},
		{"agent_deployments", scope, "writer", now.Add(time.Minute)},
		{"agent_deployments", agentOnly, "solo", now},
		{"team_deployments", scope, "crew", now},
	} {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO `+store.table(row.table)+`
			(scope_kind, scope_id, id, definition_id, active_version, revision, updated_at, payload)
			VALUES ($1, $2, $3, 'd', '1', 1, $4, '{}'::jsonb)`, row.scope.Kind, row.scope.ID, row.id, row.at); err != nil {
			t.Fatal(err)
		}
	}
	reopened, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if version, err := reopened.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}
	var nullable string
	if err := reopened.db.QueryRowContext(ctx, `SELECT is_nullable FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = 'conversations' AND column_name = 'space_id'`, schema).Scan(&nullable); err != nil || nullable != "NO" {
		t.Fatalf("space_id nullable = %q, err = %v", nullable, err)
	}
	assertSpacesBackfilled(t, reopened, scope, agentOnly, conversation.ID)
}
