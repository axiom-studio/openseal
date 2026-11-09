//go:build integration

package runtime

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A v77 database keeps its Personal space: migration 78 renames the column,
// moves the id to default-<md5> with its members and conversations, and
// keeps a name someone gave the space.
func TestPostgresDefaultSpaceMigration(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN")
	}
	ctx := context.Background()
	schema := "default_space_" + uuid.NewString()[:8]
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
		_ = store.Close()
	})
	scope := Scope{Kind: "tenant", ID: "9"}
	conversation, _, err := NewConversationService(store).CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "helper"}, Title: "Old chat", IdempotencyKey: "old",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureMemberHasSpace(ctx, scope, SpaceMemberAgent, "helper", conversation.CreatedAt); err != nil {
		t.Fatal(err)
	}
	newID := DefaultSpaceID(scope)
	oldID := "personal-" + strings.TrimPrefix(newID, "default-")
	for _, statement := range []string{
		`ALTER TABLE ` + store.table("spaces") + ` RENAME COLUMN is_default TO is_personal`,
		`ALTER INDEX ` + store.table("spaces_default_idx") + ` RENAME TO spaces_personal_idx`,
		`ALTER TABLE ` + store.table("space_members") + ` DROP CONSTRAINT space_members_scope_kind_scope_id_space_id_fkey`,
		`UPDATE ` + store.table("spaces") + ` SET id = '` + oldID + `', name = 'Shared'`,
		`UPDATE ` + store.table("space_members") + ` SET space_id = '` + oldID + `'`,
		`ALTER TABLE ` + store.table("space_members") + ` ADD FOREIGN KEY (scope_kind, scope_id, space_id) REFERENCES ` + store.table("spaces") + ` (scope_kind, scope_id, id) ON DELETE CASCADE`,
		`UPDATE ` + store.table("conversations") + ` SET space_id = '` + oldID + `', payload = jsonb_set(payload, '{spaceId}', to_jsonb('` + oldID + `'::text))`,
		`DELETE FROM ` + store.table("schema_migrations") + ` WHERE version = 78`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	_ = store.Close()
	reopened, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	listed, err := reopened.ListSpaces(ctx, scope)
	if err != nil || len(listed) != 1 || listed[0].ID != newID || !listed[0].IsDefault || listed[0].Name != "Shared" {
		t.Fatalf("spaces = %+v, err = %v", listed, err)
	}
	members, err := reopened.ListSpaceMembers(ctx, scope, newID)
	if err != nil || len(members) != 1 || members[0].ID != "helper" {
		t.Fatalf("members = %+v, err = %v", members, err)
	}
	moved, err := reopened.GetConversation(ctx, scope, conversation.ID)
	if err != nil || moved.SpaceID != newID {
		t.Fatalf("conversation = %+v, err = %v", moved, err)
	}
	if version, err := reopened.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("schema version = %d, %v", version, err)
	}
}
