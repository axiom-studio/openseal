//go:build integration

package runtime

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A v77 database keeps its Personal space: migration 78 renames the column,
// moves the id to default-<md5> with its members and conversations, and
// keeps a name someone gave the space. Migration 86 leaves it the default
// space of the owner it has.
func TestPostgresDefaultSpaceMigration(t *testing.T) {
	pgSchema := newTestPostgresSchema(t, "default_space_")
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
		`DROP TABLE ` + pgSchema.table("space_people"),
		`DROP INDEX ` + pgSchema.table("spaces_owner_default_idx"),
		`ALTER TABLE ` + pgSchema.table("spaces") + ` DROP COLUMN access`,
		`ALTER TABLE ` + pgSchema.table("spaces") + ` RENAME COLUMN owner_id TO created_by`,
		`ALTER TABLE ` + pgSchema.table("spaces") + ` RENAME COLUMN is_default TO is_personal`,
		`CREATE UNIQUE INDEX spaces_personal_idx ON ` + pgSchema.table("spaces") + ` (scope_kind, scope_id) WHERE is_personal`,
		`ALTER TABLE ` + pgSchema.table("space_members") + ` DROP CONSTRAINT space_members_scope_kind_scope_id_space_id_fkey`,
		`UPDATE ` + pgSchema.table("spaces") + ` SET id = '` + oldID + `', name = 'Shared', created_by = '` + legacyDefaultSpaceOwner + `'`,
		`UPDATE ` + pgSchema.table("space_members") + ` SET space_id = '` + oldID + `'`,
		`ALTER TABLE ` + pgSchema.table("space_members") + ` ADD FOREIGN KEY (scope_kind, scope_id, space_id) REFERENCES ` + pgSchema.table("spaces") + ` (scope_kind, scope_id, id) ON DELETE CASCADE`,
		`UPDATE ` + pgSchema.table("conversations") + ` SET space_id = '` + oldID + `', payload = jsonb_set(payload, '{spaceId}', to_jsonb('` + oldID + `'::text))`,
		`DELETE FROM ` + pgSchema.table("schema_migrations") + ` WHERE version IN (78, 84, 86)`,
	} {
		if _, err := pgSchema.db.ExecContext(ctx, statement); err != nil {
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
		listed[0].Access != SpaceAccessOwner || listed[0].OwnerID != legacyDefaultSpaceOwner {
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
	if version, err := reopened.PostgresSchemaVersion(ctx); err != nil || version != postgresSchemaHead(t) {
		t.Fatalf("schema version = %d, %v", version, err)
	}
}
