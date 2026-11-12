//go:build integration

package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// A v77 database keeps its Personal space: migration 78 renames the column,
// moves the id to default-<md5> with its members and conversations, and
// keeps a name someone gave the space. Migration 86 leaves it the default
// space of the owner it has, and migration 87 opens it to everyone.
func TestPostgresDefaultSpaceMigration(t *testing.T) {
	pgSchema := storetest.NewPostgresSchema(t, "default_space_")
	dsn := pgSchema.DSN
	schema := pgSchema.Name
	ctx := context.Background()
	store, err := postgres.New(ctx, dsn, postgres.WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pgSchema.DB.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+pgSchema.Quoted()+` CASCADE`)
		_ = store.Close()
	})
	scope := models.Scope{Kind: "tenant", ID: "9"}
	seeded := seedTestSpace(t, store, scope)
	if _, _, err := store.PutSpaceMember(ctx, scope, &models.SpaceMember{SpaceID: seeded.ID, Type: models.SpaceMemberAgent, ID: "helper", AddedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	conversation, _, err := conversation.NewConversationService(store).CreateConversation(ctx, conversation.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "helper"}, Title: "Old chat", IdempotencyKey: "old",
	})
	if err != nil {
		t.Fatal(err)
	}
	newID := storetest.LegacyDefaultSpaceID(scope)
	oldID := "personal-" + strings.TrimPrefix(newID, "default-")
	// Rewind to the Personal schema: one space per scope, owned by no person.
	// The space keeps a name someone gave it.
	for _, statement := range []string{
		`DROP TABLE ` + pgSchema.Table("space_people"),
		`DROP INDEX ` + pgSchema.Table("spaces_owner_default_idx"),
		`ALTER TABLE ` + pgSchema.Table("spaces") + ` DROP COLUMN access`,
		`ALTER TABLE ` + pgSchema.Table("spaces") + ` RENAME COLUMN owner_id TO created_by`,
		`ALTER TABLE ` + pgSchema.Table("spaces") + ` RENAME COLUMN is_default TO is_personal`,
		`CREATE UNIQUE INDEX spaces_personal_idx ON ` + pgSchema.Table("spaces") + ` (scope_kind, scope_id) WHERE is_personal`,
		`ALTER TABLE ` + pgSchema.Table("space_members") + ` DROP CONSTRAINT space_members_scope_kind_scope_id_space_id_fkey`,
		`UPDATE ` + pgSchema.Table("spaces") + ` SET id = '` + oldID + `', name = 'Shared', created_by = '` + storetest.LegacyDefaultSpaceOwner + `'`,
		`UPDATE ` + pgSchema.Table("space_members") + ` SET space_id = '` + oldID + `'`,
		`ALTER TABLE ` + pgSchema.Table("space_members") + ` ADD FOREIGN KEY (scope_kind, scope_id, space_id) REFERENCES ` + pgSchema.Table("spaces") + ` (scope_kind, scope_id, id) ON DELETE CASCADE`,
		`UPDATE ` + pgSchema.Table("conversations") + ` SET space_id = '` + oldID + `', payload = jsonb_set(payload, '{spaceId}', to_jsonb('` + oldID + `'::text))`,
		`DELETE FROM ` + pgSchema.Table("schema_migrations") + ` WHERE version IN (78, 84, 86, 87)`,
	} {
		if _, err := pgSchema.DB.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	_ = store.Close()
	reopened, err := postgres.New(ctx, dsn, postgres.WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	listed, err := reopened.ListSpaces(ctx, scope)
	if err != nil || len(listed) != 1 || listed[0].ID != newID || !listed[0].IsDefault || listed[0].Name != "Shared" ||
		listed[0].Access != models.SpaceAccessEveryone || listed[0].OwnerID != storetest.LegacyDefaultSpaceOwner {
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
