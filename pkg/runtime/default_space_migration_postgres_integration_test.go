//go:build integration

package runtime

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// A v77 database keeps its Personal space: migration 78 renames the column,
// moves the id to default-<md5> with its members and conversations, and
// keeps a name someone gave the space. Migration 86 leaves it the default
// space of the owner it has.
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
	seeded := seedTestSpace(t, store, scope)
	if _, _, err := store.PutSpaceMember(ctx, scope, &SpaceMember{SpaceID: seeded.ID, Type: SpaceMemberAgent, ID: "helper", AddedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	conversation, _, err := NewConversationService(store).CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "helper"}, Title: "Old chat", IdempotencyKey: "old",
	})
	if err != nil {
		t.Fatal(err)
	}
	newID := legacyDefaultSpaceID(scope)
	oldID := "personal-" + strings.TrimPrefix(newID, "default-")
	// Rewind to the Personal schema: one space per scope, owned by no person.
	// The space keeps a name someone gave it.
	for _, statement := range []string{
		`DROP TABLE ` + store.table("space_people"),
		`DROP INDEX ` + store.table("spaces_owner_default_idx"),
		`ALTER TABLE ` + store.table("spaces") + ` DROP COLUMN access`,
		`ALTER TABLE ` + store.table("spaces") + ` RENAME COLUMN owner_id TO created_by`,
		`ALTER TABLE ` + store.table("spaces") + ` RENAME COLUMN is_default TO is_personal`,
		`CREATE UNIQUE INDEX spaces_personal_idx ON ` + store.table("spaces") + ` (scope_kind, scope_id) WHERE is_personal`,
		`ALTER TABLE ` + store.table("space_members") + ` DROP CONSTRAINT space_members_scope_kind_scope_id_space_id_fkey`,
		`UPDATE ` + store.table("spaces") + ` SET id = '` + oldID + `', name = 'Shared', created_by = '` + legacyDefaultSpaceOwner + `'`,
		`UPDATE ` + store.table("space_members") + ` SET space_id = '` + oldID + `'`,
		`ALTER TABLE ` + store.table("space_members") + ` ADD FOREIGN KEY (scope_kind, scope_id, space_id) REFERENCES ` + store.table("spaces") + ` (scope_kind, scope_id, id) ON DELETE CASCADE`,
		`UPDATE ` + store.table("conversations") + ` SET space_id = '` + oldID + `', payload = jsonb_set(payload, '{spaceId}', to_jsonb('` + oldID + `'::text))`,
		`DELETE FROM ` + store.table("schema_migrations") + ` WHERE version IN (78, 84, 86)`,
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
	if err != nil || len(listed) != 1 || listed[0].ID != newID || !listed[0].IsDefault || listed[0].Name != "Shared" ||
		listed[0].Access != SpaceAccessEveryone || listed[0].OwnerID != legacyDefaultSpaceOwner {
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
