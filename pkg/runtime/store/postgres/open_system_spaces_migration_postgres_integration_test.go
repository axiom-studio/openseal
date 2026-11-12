//go:build integration

package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// A v77 database with a tenant's "Personal" space created by "system" and
// holding its chats, a scope's former default space a person owns, and
// people's own spaces: after every migration the first two (and a space with
// no owner) are "everyone", owners unchanged, and people's spaces "owner".
// Rolling back 87 closes them again, twice over, and migrating reopens them.
func TestPostgresOpenSystemSpacesMigration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store, dsn, schema := openSpacesPostgresStore(t, "openseal_open_system_spaces_")
	acme, beta := models.Scope{Kind: "tenant", ID: "acme"}, models.Scope{Kind: "tenant", ID: "beta"}
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	work, err := store.CreateSpace(ctx, storetest.OwnedSpace(acme, "Work", "7", at))
	if err != nil {
		t.Fatal(err)
	}
	team, err := store.CreateSpace(ctx, storetest.OwnedSpace(acme, "Team", "8", at.Add(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	var chats []string
	for _, key := range []string{"one", "two"} {
		conversation, _, err := store.CreateConversation(ctx, &models.Conversation{
			ID: "chat-" + key, Scope: acme, SpaceID: work.ID, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "helper"}, Title: key,
			Status: models.ConversationStatusActive, ApprovalMode: models.DefaultConversationApprovalMode, Revision: 1, CreatedAt: at, UpdatedAt: at,
		}, key)
		if err != nil {
			t.Fatal(err)
		}
		chats = append(chats, conversation.ID)
	}
	if err := store.RollbackPostgresMigrations(ctx, 77); err != nil {
		t.Fatal(err)
	}
	acmeDefault, betaDefault := storetest.LegacyDefaultSpaceID(acme), storetest.LegacyDefaultSpaceID(beta)
	personal := func(id string) string { return "personal-" + strings.TrimPrefix(id, "default-") }
	spaces, conversations := store.table("spaces"), store.table("conversations")
	// Rewind to v77: is_personal, and each scope's one Personal space.
	for _, statement := range []string{
		`ALTER TABLE ` + spaces + ` RENAME COLUMN is_default TO is_personal`,
		`UPDATE ` + spaces + ` SET is_personal = FALSE`,
		`INSERT INTO ` + spaces + ` (scope_kind, scope_id, id, name, look, position, is_personal, created_by, created_at, updated_at) VALUES
			('tenant', 'acme', '` + personal(acmeDefault) + `', 'Personal', 'tidepool', 0, TRUE, 'system', '2026-08-01', '2026-08-01'),
			('tenant', 'beta', '` + personal(betaDefault) + `', 'Personal', 'tidepool', 0, TRUE, '7', '2026-08-01', '2026-08-01'),
			('tenant', 'acme', 'default-not-a-scope-digest', 'Lookalike', 'tidepool', 3, FALSE, '8', '2026-08-02', '2026-08-02'),
			('tenant', 'acme', 'blank', 'Blank', 'tidepool', 4, FALSE, '', '2026-08-02', '2026-08-02')`,
		`UPDATE ` + conversations + ` SET space_id = '` + personal(acmeDefault) + `',
			payload = jsonb_set(payload, '{spaceId}', to_jsonb('` + personal(acmeDefault) + `'::text))`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	reopen := func() *Store {
		t.Helper()
		reopened, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		if version, err := reopened.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
			t.Fatalf("schema version = %d, %v", version, err)
		}
		return reopened
	}
	type row struct{ owner, access string }
	assertSpaces := func(store *Store, when string, want map[string]row) {
		t.Helper()
		for _, scope := range []models.Scope{acme, beta} {
			listed, err := store.ListSpaces(ctx, scope)
			if err != nil {
				t.Fatal(err)
			}
			for _, space := range listed {
				w, ok := want[space.ID]
				if !ok || space.OwnerID != w.owner || string(space.Access) != w.access {
					t.Fatalf("%s: space %s = owner %q, %s; want %+v", when, space.ID, space.OwnerID, space.Access, w)
				}
				delete(want, space.ID)
			}
		}
		if len(want) != 0 {
			t.Fatalf("%s: missing spaces %v", when, want)
		}
	}
	opened := func() map[string]row {
		return map[string]row{
			acmeDefault: {"system", "everyone"}, betaDefault: {"7", "everyone"}, "blank": {"", "everyone"},
			work.ID: {"7", "owner"}, team.ID: {"8", "owner"}, "default-not-a-scope-digest": {"8", "owner"},
		}
	}

	migrated := reopen()
	assertSpaces(migrated, "migrated from 77", opened())
	listed, err := migrated.ListConversations(ctx, models.ConversationFilter{Scope: acme, SpaceID: acmeDefault})
	if err != nil || len(listed) != len(chats) {
		t.Fatalf("chats in the Default space = %d, %v", len(listed), err)
	}
	if visible, err := migrated.VisibleSpaces(ctx, acme, "9"); err != nil || len(visible) != 2 {
		t.Fatalf("visible to another person = %v, %v", visible, err)
	}

	// A person opens their own space; running the migration again changes
	// nothing.
	if _, err := migrated.SetSpaceAccess(ctx, acme, team.ID, models.SpaceAccessEveryone, at); err != nil {
		t.Fatal(err)
	}
	if _, err := migrated.db.ExecContext(ctx, `DELETE FROM `+migrated.table("schema_migrations")+` WHERE version = $1`, openSystemSpacesMigrationVersion); err != nil {
		t.Fatal(err)
	}
	want := opened()
	want[team.ID] = row{"8", "everyone"}
	assertSpaces(reopen(), "migrated again", want)

	// Rolling back closes the spaces it matches, and only those, however
	// often it runs.
	for range 2 {
		if err := migrated.RollbackPostgresMigrations(ctx, openSystemSpacesMigrationVersion-1); err != nil {
			t.Fatal(err)
		}
	}
	closed := map[string]row{
		acmeDefault: {"system", "owner"}, betaDefault: {"7", "owner"}, "blank": {"", "owner"},
		work.ID: {"7", "owner"}, team.ID: {"8", "everyone"}, "default-not-a-scope-digest": {"8", "owner"},
	}
	assertSpaces(migrated, "rolled back", closed)
	for range 2 {
		if err := migrated.RollbackPostgresMigrations(ctx, 77); err != nil {
			t.Fatal(err)
		}
	}
	assertSpaces(reopen(), "migrated after rollbacks to 77", opened())
}
