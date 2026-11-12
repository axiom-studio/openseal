package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestSQLiteSpaceStore(t *testing.T) {
	t.Parallel()
	store, err := sqlite.New(filepath.Join(t.TempDir(), "spaces.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	exerciseSpaceStore(t, store)
}

func TestSpaceValidation(t *testing.T) {
	t.Parallel()
	service := portfolio.NewSpaceService(newTestKernelStore(t))
	scope := models.Scope{Kind: "tenant", ID: "one"}
	ctx := context.Background()
	for _, request := range []portfolio.CreateSpaceRequest{
		{Scope: scope, Name: "   ", Look: models.SpaceLook("harbor")},
		{Scope: scope, Name: strings.Repeat("a", 61), Look: models.SpaceLook("harbor")},
		{Scope: scope, Name: "Work", Look: "neon dusk"},
		{Scope: scope, Name: "Work", Look: "x"},
		{Scope: models.Scope{}, Name: "Work", Look: models.SpaceLook("harbor")},
	} {
		if _, err := service.CreateSpace(ctx, request); err == nil {
			t.Fatalf("CreateSpace(%+v) succeeded", request)
		}
	}
	if listed, err := service.ListSpaces(ctx, scope); err != nil || len(listed) != 0 {
		t.Fatalf("refused creates left spaces = %+v, err = %v", listed, err)
	}
	space, err := service.CreateSpace(ctx, portfolio.CreateSpaceRequest{Scope: scope, Name: strings.Repeat("é", 60), Look: models.SpaceLook("chalk")})
	if err != nil || space.Position != 0 || !space.IsDefault {
		t.Fatalf("60-rune name: %+v %v", space, err)
	}
	badLook := models.SpaceLook("Neon Dusk")
	if _, err := service.UpdateSpace(ctx, portfolio.UpdateSpaceRequest{Scope: scope, SpaceID: space.ID, Look: &badLook}); !errors.Is(err, models.ErrInvalidSpace) {
		t.Fatalf("invalid look update: %v", err)
	}
	if _, _, err := service.PutSpaceMember(ctx, portfolio.PutSpaceMemberRequest{Scope: scope, SpaceID: space.ID, Type: "user", MemberID: "u"}); !errors.Is(err, models.ErrInvalidSpace) {
		t.Fatalf("invalid member type: %v", err)
	}
}

// exerciseSpaceStore is the store contract shared by the SQLite and
// PostgreSQL implementations.
func exerciseSpaceStore(t *testing.T, store storetest.SpaceConversationStore) {
	t.Helper()
	ctx := context.Background()
	clock := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	tick := models.WithClock(func() time.Time { clock = clock.Add(time.Second); return clock })
	spaces := portfolio.NewSpaceService(store, tick)
	conversations := conversation.NewConversationService(store, tick)
	scope := models.Scope{Kind: "tenant", ID: "spaces-" + strings.ReplaceAll(t.Name(), "/", "-")}

	const owner = "7"
	if listed, err := spaces.ListSpaces(ctx, scope); err != nil || len(listed) != 0 {
		t.Fatalf("initial spaces = %+v, err = %v", listed, err)
	}
	defaultSpace, err := spaces.CreateSpace(ctx, portfolio.CreateSpaceRequest{Scope: scope, Name: "Main", Look: models.DefaultSpaceLook, OwnerID: owner})
	if err != nil || defaultSpace.ID == "" || !defaultSpace.IsDefault || defaultSpace.Name != "Main" || defaultSpace.Look != models.DefaultSpaceLook ||
		defaultSpace.Position != 0 || defaultSpace.Scope != scope || defaultSpace.OwnerID != owner || defaultSpace.Access != models.SpaceAccessOwner {
		t.Fatalf("defaultSpace = %+v, err = %v", defaultSpace, err)
	}
	if again, err := spaces.DefaultSpace(ctx, scope, " "+owner+" "); err != nil || again.ID != defaultSpace.ID || !again.IsDefault {
		t.Fatalf("default space again: %+v %v", again, err)
	}
	if listed, err := spaces.ListSpaces(ctx, scope); err != nil || len(listed) != 1 || listed[0].ID != defaultSpace.ID || !listed[0].IsDefault {
		t.Fatalf("spaces after the first = %+v, err = %v", listed, err)
	}
	work, err := spaces.CreateSpace(ctx, portfolio.CreateSpaceRequest{Scope: scope, Name: "  Work  ", Look: models.SpaceLook("harbor"), OwnerID: owner})
	if err != nil || work.Name != "Work" || work.Position != 1 || work.IsDefault || work.OwnerID != owner || work.Access != models.SpaceAccessOwner {
		t.Fatalf("work = %+v, err = %v", work, err)
	}
	home, err := spaces.CreateSpace(ctx, portfolio.CreateSpaceRequest{Scope: scope, Name: "Home", Look: models.SpaceLook("critter"), OwnerID: owner})
	if err != nil || home.Position != 2 || home.IsDefault {
		t.Fatalf("home = %+v, err = %v", home, err)
	}
	first, renamed, look := 0, "Household", models.SpaceLook("afterglow")
	moved, err := spaces.UpdateSpace(ctx, portfolio.UpdateSpaceRequest{Scope: scope, SpaceID: home.ID, Name: &renamed, Look: &look, Position: &first})
	if err != nil || moved.Position != 0 || moved.Name != renamed || moved.Look != look || !moved.UpdatedAt.After(home.UpdatedAt) {
		t.Fatalf("moved = %+v, err = %v", moved, err)
	}
	assertSpaceOrder(t, spaces, scope, home.ID, defaultSpace.ID, work.ID)
	far := 99
	if _, err := spaces.UpdateSpace(ctx, portfolio.UpdateSpaceRequest{Scope: scope, SpaceID: home.ID, Position: &far}); err != nil {
		t.Fatal(err)
	}
	assertSpaceOrder(t, spaces, scope, defaultSpace.ID, work.ID, home.ID)
	if _, err := spaces.UpdateSpace(ctx, portfolio.UpdateSpaceRequest{Scope: scope, SpaceID: home.ID, Position: &first}); err != nil {
		t.Fatal(err)
	}
	if _, err := spaces.UpdateSpace(ctx, portfolio.UpdateSpaceRequest{Scope: scope, SpaceID: "missing", Name: &renamed}); !errors.Is(err, models.ErrSpaceNotFound) {
		t.Fatalf("update missing: %v", err)
	}

	put := func(spaceID string, memberType models.SpaceMemberType, id string, wantCreated bool, wantPosition int) {
		t.Helper()
		member, created, err := spaces.PutSpaceMember(ctx, portfolio.PutSpaceMemberRequest{Scope: scope, SpaceID: spaceID, Type: memberType, MemberID: id})
		if err != nil || created != wantCreated || member.Position != wantPosition || member.SpaceID != spaceID || member.Type != memberType || member.ID != id {
			t.Fatalf("put %s/%s/%s = %+v created=%v err=%v", spaceID, memberType, id, member, created, err)
		}
	}
	put(work.ID, models.SpaceMemberAgent, "finn", true, 0)
	put(work.ID, models.SpaceMemberAgent, "finn", false, 0)
	put(work.ID, models.SpaceMemberTeam, "eng", true, 1)
	put(work.ID, models.SpaceMemberAgent, "solo", true, 2)
	put(home.ID, models.SpaceMemberAgent, "finn", true, 0)
	if _, _, err := spaces.PutSpaceMember(ctx, portfolio.PutSpaceMemberRequest{Scope: scope, SpaceID: "missing", Type: models.SpaceMemberAgent, MemberID: "finn"}); !errors.Is(err, models.ErrSpaceNotFound) {
		t.Fatalf("put into missing space: %v", err)
	}
	members, err := spaces.ListSpaceMembers(ctx, scope, work.ID)
	if err != nil || len(members) != 3 || members[0].ID != "finn" || members[1].ID != "eng" || members[2].ID != "solo" {
		t.Fatalf("work members = %+v, err = %v", members, err)
	}

	create := func(key string, conversationOwner models.ObjectiveOwner, spaceID string) (*models.Conversation, error) {
		conversation, _, err := conversations.CreateConversation(ctx, conversation.CreateConversationRequest{
			Scope: scope, Owner: conversationOwner, Title: "Chat " + key, SpaceID: spaceID, CreatedBy: owner, IdempotencyKey: key,
		})
		return conversation, err
	}
	finn := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "finn"}
	defaulted, err := create("finn-default", finn, "")
	if err != nil || defaulted.SpaceID != home.ID {
		t.Fatalf("owner's first space: %+v %v", defaulted, err)
	}
	orphan, err := create("orphan", models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "nobody"}, "")
	if err != nil || orphan.SpaceID != defaultSpace.ID || orphan.CreatedBy != owner {
		t.Fatalf("defaultSpace fallback: %+v %v", orphan, err)
	}
	inWork, err := create("finn-work", finn, work.ID)
	if err != nil || inWork.SpaceID != work.ID {
		t.Fatalf("explicit space: %+v %v", inWork, err)
	}
	teamChat, err := create("eng", models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "eng"}, "")
	if err != nil || teamChat.SpaceID != work.ID {
		t.Fatalf("team space: %+v %v", teamChat, err)
	}
	if _, err := create("missing", finn, "missing"); !errors.Is(err, models.ErrSpaceNotFound) {
		t.Fatalf("unknown space: %v", err)
	}
	if replay, err := create("finn-work", finn, ""); err != nil || replay.ID != inWork.ID {
		t.Fatalf("replay without space: %+v %v", replay, err)
	}
	if _, err := create("finn-work", finn, home.ID); !errors.Is(err, models.ErrMessageConflict) {
		t.Fatalf("replay with another space: %v", err)
	}
	if stored, err := conversations.GetConversation(ctx, scope, inWork.ID); err != nil || stored.SpaceID != work.ID {
		t.Fatalf("stored = %+v %v", stored, err)
	}
	assertSpaceConversations(t, conversations, scope, work.ID, inWork.ID, teamChat.ID)
	inSpace := func(spaceID string) int {
		t.Helper()
		listed, err := conversations.ListConversations(ctx, models.ConversationFilter{Scope: scope, SpaceID: spaceID})
		if err != nil {
			t.Fatal(err)
		}
		return len(listed)
	}
	all := inSpace("")
	for _, test := range []struct {
		spaceIDs []string
		want     int
	}{{[]string{work.ID}, 2}, {[]string{home.ID, "missing"}, inSpace(home.ID)}, {[]string{work.ID, home.ID}, 2 + inSpace(home.ID)}, {[]string{}, 0}, {nil, all}} {
		listed, err := conversations.ListConversations(ctx, models.ConversationFilter{Scope: scope, SpaceIDs: test.spaceIDs})
		if err != nil || len(listed) != test.want || (test.spaceIDs != nil && slices.ContainsFunc(listed, func(c *models.Conversation) bool { return !slices.Contains(test.spaceIDs, c.SpaceID) })) {
			t.Fatalf("SpaceIDs %v listed %d, err = %v; want %d", test.spaceIDs, len(listed), err, test.want)
		}
	}

	title := "Renamed"
	updated, err := conversations.UpdateConversation(ctx, conversation.UpdateConversationRequest{Scope: scope, ConversationID: inWork.ID, ExpectedRevision: inWork.Revision, Title: &title})
	if err != nil || updated.SpaceID != work.ID {
		t.Fatalf("update keeps space: %+v %v", updated, err)
	}

	if err := spaces.DeleteSpace(ctx, scope, defaultSpace.ID); !errors.Is(err, models.ErrDefaultSpaceProtected) {
		t.Fatalf("delete defaultSpace: %v", err)
	}
	if err := spaces.DeleteSpace(ctx, scope, work.ID); err != nil {
		t.Fatal(err)
	}
	if err := spaces.DeleteSpace(ctx, scope, work.ID); !errors.Is(err, models.ErrSpaceNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if _, err := spaces.ListSpaceMembers(ctx, scope, work.ID); !errors.Is(err, models.ErrSpaceNotFound) {
		t.Fatalf("members of deleted space: %v", err)
	}
	assertSpaceConversations(t, conversations, scope, work.ID)
	assertSpaceConversations(t, conversations, scope, defaultSpace.ID, orphan.ID, inWork.ID, teamChat.ID)
	if moved, err := conversations.GetConversation(ctx, scope, inWork.ID); err != nil || moved.SpaceID != defaultSpace.ID || moved.Title != title {
		t.Fatalf("moved conversation = %+v %v", moved, err)
	}
	// Agents stay where else they are; anyone left in no space joins the
	// owner's default space.
	defaultMembers, err := spaces.ListSpaceMembers(ctx, scope, defaultSpace.ID)
	if err != nil || len(defaultMembers) != 2 || defaultMembers[0].ID != "eng" || defaultMembers[1].ID != "solo" ||
		defaultMembers[0].Position != 0 || defaultMembers[1].Position != 1 {
		t.Fatalf("defaultSpace members = %+v, err = %v", defaultMembers, err)
	}
	if homeMembers, err := spaces.ListSpaceMembers(ctx, scope, home.ID); err != nil || len(homeMembers) != 1 || homeMembers[0].ID != "finn" {
		t.Fatalf("home members = %+v, err = %v", homeMembers, err)
	}
	if err := spaces.DeleteSpaceMember(ctx, portfolio.DeleteSpaceMemberRequest{Scope: scope, SpaceID: home.ID, Type: models.SpaceMemberAgent, MemberID: "finn"}); err != nil {
		t.Fatal(err)
	}
	if err := spaces.DeleteSpaceMember(ctx, portfolio.DeleteSpaceMemberRequest{Scope: scope, SpaceID: home.ID, Type: models.SpaceMemberAgent, MemberID: "finn"}); err != nil {
		t.Fatalf("delete absent member: %v", err)
	}
	if homeMembers, err := spaces.ListSpaceMembers(ctx, scope, home.ID); err != nil || len(homeMembers) != 0 {
		t.Fatalf("home members after delete = %+v, err = %v", homeMembers, err)
	}

	placed, err := spaces.EnsureMemberHasSpace(ctx, scope, owner, models.SpaceMemberAgent, "fresh")
	if err != nil || placed == nil || placed.SpaceID != defaultSpace.ID || placed.Position != 2 {
		t.Fatalf("fresh deployment placed = %+v, err = %v", placed, err)
	}
	for _, id := range []string{"fresh", "eng"} {
		memberType := models.SpaceMemberAgent
		if id == "eng" {
			memberType = models.SpaceMemberTeam
		}
		if again, err := spaces.EnsureMemberHasSpace(ctx, scope, owner, memberType, id); err != nil || again != nil {
			t.Fatalf("%s placed again = %+v, err = %v", id, again, err)
		}
	}
	// "finn" is no longer in any space after its removal from home.
	if placed, err := spaces.EnsureMemberHasSpace(ctx, scope, owner, models.SpaceMemberAgent, "finn"); err != nil || placed == nil || placed.SpaceID != defaultSpace.ID {
		t.Fatalf("finn placed = %+v, err = %v", placed, err)
	}
	freshScope := models.Scope{Kind: scope.Kind, ID: scope.ID + "-fresh"}
	// A default space is the owner's in one scope only: placement in another
	// scope finds none and creates none.
	if placed, err := spaces.EnsureMemberHasSpace(ctx, freshScope, owner, models.SpaceMemberTeam, "crew"); !errors.Is(err, models.ErrNoDefaultSpace) || placed != nil {
		t.Fatalf("placement without a default space = %+v, err = %v", placed, err)
	}
	if freshSpaces, err := spaces.ListSpaces(ctx, freshScope); err != nil || len(freshSpaces) != 0 {
		t.Fatalf("fresh scope = %+v, err = %v", freshSpaces, err)
	}

	other := models.Scope{Kind: scope.Kind, ID: scope.ID + "-other"}
	if otherSpaces, err := spaces.ListSpaces(ctx, other); err != nil || len(otherSpaces) != 0 {
		t.Fatalf("other scope = %+v, err = %v", otherSpaces, err)
	}
	if _, err := spaces.GetSpace(ctx, other, home.ID); !errors.Is(err, models.ErrSpaceNotFound) {
		t.Fatalf("cross-scope get: %v", err)
	}
}

func assertSpaceOrder(t *testing.T, spaces *portfolio.SpaceService, scope models.Scope, ids ...string) {
	t.Helper()
	listed, err := spaces.ListSpaces(context.Background(), scope)
	if err != nil || len(listed) != len(ids) {
		t.Fatalf("spaces = %d, err = %v", len(listed), err)
	}
	for index, space := range listed {
		if space.ID != ids[index] || space.Position != index {
			t.Fatalf("space %d = %s@%d, want %s@%d", index, space.ID, space.Position, ids[index], index)
		}
	}
}

func assertSpaceConversations(t *testing.T, conversations *conversation.ConversationService, scope models.Scope, spaceID string, ids ...string) {
	t.Helper()
	listed, err := conversations.ListConversations(context.Background(), models.ConversationFilter{Scope: scope, SpaceID: spaceID})
	if err != nil {
		t.Fatal(err)
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	if len(listed) != len(ids) {
		t.Fatalf("space %s has %d conversations, want %d", spaceID, len(listed), len(ids))
	}
	for _, conversation := range listed {
		if !want[conversation.ID] || conversation.SpaceID != spaceID {
			t.Fatalf("unexpected conversation %s in %s", conversation.ID, conversation.SpaceID)
		}
	}
}

func TestSQLiteSpacesBackfillExistingData(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "backfill.db")
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := openTestSQLiteDB(t, path)
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "legacy"}
	agentOnly := models.Scope{Kind: "tenant", ID: "agents-only"}
	// The conversation needs a space to be created; the rewind below drops it.
	seedTestSpace(t, store, scope)
	conversation, _, err := conversation.NewConversationService(store).CreateConversation(ctx, conversation.CreateConversationRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "helper"}, Title: "Old chat", IdempotencyKey: "old",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for _, statement := range []struct {
		query string
		args  []interface{}
	}{
		{`INSERT INTO agent_deployments (scope_kind, scope_id, id, definition_id, active_version, revision, updated_at, payload) VALUES (?, ?, ?, 'd', '1', 1, ?, '{}')`, []interface{}{scope.Kind, scope.ID, "helper", now}},
		{`INSERT INTO agent_deployments (scope_kind, scope_id, id, definition_id, active_version, revision, updated_at, payload) VALUES (?, ?, ?, 'd', '1', 1, ?, '{}')`, []interface{}{scope.Kind, scope.ID, "writer", now.Add(time.Minute)}},
		{`INSERT INTO agent_deployments (scope_kind, scope_id, id, definition_id, active_version, revision, updated_at, payload) VALUES (?, ?, ?, 'd', '1', 1, ?, '{}')`, []interface{}{agentOnly.Kind, agentOnly.ID, "solo", now}},
		{`INSERT INTO team_deployments (scope_kind, scope_id, id, definition_id, active_version, revision, updated_at, payload) VALUES (?, ?, ?, 'd', '1', 1, ?, '{}')`, []interface{}{scope.Kind, scope.ID, "crew", now}},
		// Rewind to the schema before spaces.
		{`DROP INDEX idx_conversations_space`, nil},
		{`ALTER TABLE conversations DROP COLUMN space_id`, nil},
		{`UPDATE conversations SET payload = json_remove(payload, '$.spaceId')`, nil},
		{`DELETE FROM space_members`, nil},
		{`DELETE FROM spaces`, nil},
	} {
		if _, err := sqlDB.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("%s: %v", statement.query, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	storetest.AssertSpacesBackfilled(t, reopened, scope, agentOnly, conversation.ID)
}

// assertSpacesBackfilled checks what migration 67 leaves in a database from
// before spaces: one space named Default per scope, owned by no person.
func TestSQLiteMigratesPersonalSpaceToDefault(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "default.db")
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := openTestSQLiteDB(t, path)
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "renamed"}
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
		`DROP INDEX idx_spaces_owner_default`,
		`ALTER TABLE spaces DROP COLUMN access`,
		`ALTER TABLE spaces RENAME COLUMN owner_id TO created_by`,
		`ALTER TABLE spaces RENAME COLUMN is_default TO is_personal`,
		`CREATE UNIQUE INDEX idx_spaces_personal ON spaces(scope_kind, scope_id) WHERE is_personal = 1`,
		`UPDATE spaces SET id = '` + oldID + `', name = 'Shared', created_by = '` + storetest.LegacyDefaultSpaceOwner + `'`,
		`UPDATE space_members SET space_id = '` + oldID + `'`,
		`UPDATE conversations SET space_id = '` + oldID + `', payload = json_set(payload, '$.spaceId', '` + oldID + `')`,
	} {
		if _, err := sqlDB.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.New(path)
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
}
