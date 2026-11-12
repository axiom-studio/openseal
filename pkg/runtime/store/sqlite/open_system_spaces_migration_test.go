package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// A database from before space access, with a tenant's "Personal" space
// created by "system" and holding its chats, a scope's former default space a
// person owns, a space with no owner and people's own spaces: opening it makes
// the first three "everyone", owners unchanged, and people's spaces "owner".
// A later open changes nothing, so a space opened or closed since stays so.
func TestSQLiteOpenSystemSpacesMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "spaces.db")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	db := openTestSQLiteDB(t, path)
	acme, beta := models.Scope{Kind: "tenant", ID: "acme"}, models.Scope{Kind: "tenant", ID: "beta"}
	at := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	work, err := store.CreateSpace(ctx, storetest.OwnedSpace(acme, "Work", "7", at))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"one", "two"} {
		if _, _, err := store.CreateConversation(ctx, &models.Conversation{
			ID: "chat-" + key, Scope: acme, SpaceID: work.ID, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "helper"}, Title: key,
			Status: models.ConversationStatusActive, ApprovalMode: models.DefaultConversationApprovalMode, Revision: 1, CreatedAt: at, UpdatedAt: at,
		}, key); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	acmeDefault, betaDefault := storetest.LegacyDefaultSpaceID(acme), storetest.LegacyDefaultSpaceID(beta)
	personal := func(id string) string { return "personal-" + strings.TrimPrefix(id, "default-") }
	for _, statement := range []string{
		`DROP INDEX idx_spaces_owner_default`,
		`ALTER TABLE spaces DROP COLUMN access`,
		`ALTER TABLE spaces RENAME COLUMN owner_id TO created_by`,
		`ALTER TABLE spaces RENAME COLUMN is_default TO is_personal`,
		`DROP TABLE space_people`,
		`UPDATE spaces SET is_personal = 0`,
		`INSERT INTO spaces (scope_kind, scope_id, id, name, look, position, is_personal, created_by, created_at, updated_at) VALUES
			('tenant', 'acme', '` + personal(acmeDefault) + `', 'Personal', 'tidepool', 0, 1, 'system', '2026-08-01', '2026-08-01'),
			('tenant', 'beta', '` + personal(betaDefault) + `', 'Personal', 'tidepool', 0, 1, '7', '2026-08-01', '2026-08-01'),
			('tenant', 'acme', 'default-not-a-scope-digest', 'Lookalike', 'tidepool', 3, 0, '8', '2026-08-02', '2026-08-02'),
			('tenant', 'acme', 'blank', 'Blank', 'tidepool', 4, 0, '', '2026-08-02', '2026-08-02')`,
		`CREATE UNIQUE INDEX idx_spaces_personal ON spaces(scope_kind, scope_id) WHERE is_personal = 1`,
		`UPDATE conversations SET space_id = '` + personal(acmeDefault) + `', payload = json_set(payload, '$.spaceId', '` + personal(acmeDefault) + `')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	type row struct{ owner, access string }
	assertSpaces := func(when string, want map[string]row) {
		t.Helper()
		reopened, err := New(path)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		for _, scope := range []models.Scope{acme, beta} {
			listed, err := reopened.ListSpaces(ctx, scope)
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
		chats, err := reopened.ListConversations(ctx, models.ConversationFilter{Scope: acme, SpaceID: acmeDefault})
		if err != nil || len(chats) != 2 {
			t.Fatalf("%s: chats in the Default space = %d, %v", when, len(chats), err)
		}
	}
	opened := map[string]row{
		acmeDefault: {"system", "everyone"}, betaDefault: {"7", "everyone"}, "blank": {"", "everyone"},
		work.ID: {"7", "owner"}, "default-not-a-scope-digest": {"8", "owner"},
	}
	clone := func(rows map[string]row) map[string]row {
		out := map[string]row{}
		for id, r := range rows {
			out[id] = r
		}
		return out
	}
	assertSpaces("migrated", clone(opened))
	assertSpaces("opened again", clone(opened))

	if _, err := db.Exec(`UPDATE spaces SET access = 'owner' WHERE id = ?`, betaDefault); err != nil {
		t.Fatal(err)
	}
	closedLater := clone(opened)
	closedLater[betaDefault] = row{"7", "owner"}
	assertSpaces("after its owner closed it", closedLater)
}
