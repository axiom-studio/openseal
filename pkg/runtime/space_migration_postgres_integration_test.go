//go:build integration

package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
)

func openSpacesPostgresStore(t *testing.T, prefix string) (*PostgresStore, string, string) {
	t.Helper()
	store, pgSchema := openTestPostgresStore(t, prefix)
	schema := pgSchema.Name
	dsn := pgSchema.DSN
	return store, dsn, schema
}

// Migration 84 on a database with the legacy Default space and one space a
// person made: created_by becomes owner_id, the person's space is "owner" and
// the legacy space "everyone". Rolling it back restores created_by and drops
// the people. Migration 85 then makes the person's space their default.
func TestPostgresSpaceAccessBackfillAndRollback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store, dsn, schema := openSpacesPostgresStore(t, "openseal_space_access_backfill_")
	scope := models.Scope{Kind: "tenant", ID: "legacy"}
	spaces := NewSpaceService(store)
	work, err := spaces.CreateSpace(ctx, CreateSpaceRequest{Scope: scope, Name: "Work", Look: models.DefaultSpaceLook, OwnerID: "7"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spaces.SetSpaceAccess(ctx, scope, work.ID, models.SpaceAccessEveryone); err != nil {
		t.Fatal(err)
	}
	if _, _, err := spaces.AddSpacePerson(ctx, AddSpacePersonRequest{Scope: scope, SpaceID: work.ID, UserID: "8", AddedBy: "7"}); err != nil {
		t.Fatal(err)
	}
	// The shape before per-owner default spaces: the scope's one default
	// space is the legacy one, older than the space the person made.
	if _, err := store.db.ExecContext(ctx, `UPDATE `+store.table("spaces")+` SET is_default = FALSE, position = 1 WHERE id = $1`, work.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO `+store.table("spaces")+` (`+sqlstore.SpaceColumns+`)
		VALUES ($1, $2, $3, 'Default', 'tidepool', 0, TRUE, 'everyone', $4, $5, $5)`,
		legacyDefaultSpaceID(scope), scope.Kind, scope.ID, legacyDefaultSpaceOwner, work.CreatedAt.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.RollbackPostgresMigrations(ctx, spaceAccessMigrationVersion-1); err != nil {
		t.Fatal(err)
	}
	var columns int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = $1 AND (table_name = 'spaces' AND column_name IN ('access', 'owner_id') OR table_name = 'space_people')`, schema).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("space access columns after rollback = %d, err = %v", columns, err)
	}
	var creator string
	if err := store.db.QueryRowContext(ctx, `SELECT created_by FROM `+store.table("spaces")+` WHERE id = $1`, work.ID).Scan(&creator); err != nil || creator != "7" {
		t.Fatalf("created_by after rollback = %q, err = %v", creator, err)
	}
	reopened, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if version, err := reopened.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}
	assertSpaceAccessBackfilled(t, reopened, scope, work.ID)
}

func TestPostgresSpacesBackfillAndRollback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store, dsn, schema := openSpacesPostgresStore(t, "openseal_spaces_backfill_")
	scope := models.Scope{Kind: "tenant", ID: "legacy"}
	agentOnly := models.Scope{Kind: "tenant", ID: "agents-only"}
	// The conversation needs a space to be created; the rollback below drops it.
	seedTestSpace(t, store, scope)
	conversation, _, err := NewConversationService(store).CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "helper"}, Title: "Old chat", IdempotencyKey: "old",
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
		scope models.Scope
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
