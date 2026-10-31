package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type spaceConversationStore interface {
	SpaceStore
	ConversationStore
}

func TestMemorySpaceStore(t *testing.T) {
	t.Parallel()
	exerciseSpaceStore(t, NewMemoryStore())
}

func TestSQLiteSpaceStore(t *testing.T) {
	t.Parallel()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "spaces.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	exerciseSpaceStore(t, store)
}

func TestSpaceValidation(t *testing.T) {
	t.Parallel()
	service := NewSpaceService(NewMemoryStore())
	scope := Scope{Kind: "tenant", ID: "one"}
	ctx := context.Background()
	for _, request := range []CreateSpaceRequest{
		{Scope: scope, Name: "   ", Look: SpaceLookHarbor},
		{Scope: scope, Name: strings.Repeat("a", 61), Look: SpaceLookHarbor},
		{Scope: scope, Name: "Work", Look: "neon"},
		{Scope: Scope{}, Name: "Work", Look: SpaceLookHarbor},
	} {
		if _, err := service.CreateSpace(ctx, request); err == nil {
			t.Fatalf("CreateSpace(%+v) succeeded", request)
		}
	}
	if space, err := service.CreateSpace(ctx, CreateSpaceRequest{Scope: scope, Name: strings.Repeat("é", 60), Look: SpaceLookChalk}); err != nil || space.Position != 1 {
		t.Fatalf("60-rune name: %+v %v", space, err)
	}
	badLook := SpaceLook("neon")
	if _, err := service.UpdateSpace(ctx, UpdateSpaceRequest{Scope: scope, SpaceID: PersonalSpaceID(scope), Look: &badLook}); !errors.Is(err, ErrInvalidSpace) {
		t.Fatalf("invalid look update: %v", err)
	}
	if _, _, err := service.PutSpaceMember(ctx, PutSpaceMemberRequest{Scope: scope, SpaceID: PersonalSpaceID(scope), Type: "user", MemberID: "u"}); !errors.Is(err, ErrInvalidSpace) {
		t.Fatalf("invalid member type: %v", err)
	}
}

// exerciseSpaceStore is the store contract shared by the memory, SQLite and
// PostgreSQL implementations.
func exerciseSpaceStore(t *testing.T, store spaceConversationStore) {
	t.Helper()
	ctx := context.Background()
	clock := time.Date(2026, 10, 31, 9, 0, 0, 0, time.UTC)
	spaces := NewSpaceService(store)
	spaces.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	conversations := NewConversationService(store)
	conversations.now = spaces.now
	scope := Scope{Kind: "tenant", ID: "spaces-" + strings.ReplaceAll(t.Name(), "/", "-")}

	listed, err := spaces.ListSpaces(ctx, scope)
	if err != nil || len(listed) != 1 {
		t.Fatalf("initial spaces = %d, err = %v", len(listed), err)
	}
	personal := listed[0]
	if personal.ID != PersonalSpaceID(scope) || !personal.IsPersonal || personal.Name != PersonalSpaceName ||
		personal.Look != SpaceLookTidepool || personal.Position != 0 || personal.Scope != scope {
		t.Fatalf("personal = %+v", personal)
	}
	if again, created, err := spaces.EnsurePersonalSpace(ctx, scope); err != nil || created || again.ID != personal.ID {
		t.Fatalf("ensure again: %+v %v %v", again, created, err)
	}
	work, err := spaces.CreateSpace(ctx, CreateSpaceRequest{Scope: scope, Name: "  Work  ", Look: SpaceLookHarbor, CreatedBy: "7"})
	if err != nil || work.Name != "Work" || work.Position != 1 || work.IsPersonal || work.CreatedBy != "7" {
		t.Fatalf("work = %+v, err = %v", work, err)
	}
	home, err := spaces.CreateSpace(ctx, CreateSpaceRequest{Scope: scope, Name: "Home", Look: SpaceLookCritter, CreatedBy: "7"})
	if err != nil || home.Position != 2 {
		t.Fatalf("home = %+v, err = %v", home, err)
	}
	first, renamed, look := 0, "Household", SpaceLookAfterglow
	moved, err := spaces.UpdateSpace(ctx, UpdateSpaceRequest{Scope: scope, SpaceID: home.ID, Name: &renamed, Look: &look, Position: &first})
	if err != nil || moved.Position != 0 || moved.Name != renamed || moved.Look != look || !moved.UpdatedAt.After(home.UpdatedAt) {
		t.Fatalf("moved = %+v, err = %v", moved, err)
	}
	assertSpaceOrder(t, spaces, scope, home.ID, personal.ID, work.ID)
	far := 99
	if _, err := spaces.UpdateSpace(ctx, UpdateSpaceRequest{Scope: scope, SpaceID: home.ID, Position: &far}); err != nil {
		t.Fatal(err)
	}
	assertSpaceOrder(t, spaces, scope, personal.ID, work.ID, home.ID)
	if _, err := spaces.UpdateSpace(ctx, UpdateSpaceRequest{Scope: scope, SpaceID: home.ID, Position: &first}); err != nil {
		t.Fatal(err)
	}
	if _, err := spaces.UpdateSpace(ctx, UpdateSpaceRequest{Scope: scope, SpaceID: "missing", Name: &renamed}); !errors.Is(err, ErrSpaceNotFound) {
		t.Fatalf("update missing: %v", err)
	}

	put := func(spaceID string, memberType SpaceMemberType, id string, wantCreated bool, wantPosition int) {
		t.Helper()
		member, created, err := spaces.PutSpaceMember(ctx, PutSpaceMemberRequest{Scope: scope, SpaceID: spaceID, Type: memberType, MemberID: id})
		if err != nil || created != wantCreated || member.Position != wantPosition || member.SpaceID != spaceID || member.Type != memberType || member.ID != id {
			t.Fatalf("put %s/%s/%s = %+v created=%v err=%v", spaceID, memberType, id, member, created, err)
		}
	}
	put(work.ID, SpaceMemberAgent, "finn", true, 0)
	put(work.ID, SpaceMemberAgent, "finn", false, 0)
	put(work.ID, SpaceMemberTeam, "eng", true, 1)
	put(work.ID, SpaceMemberAgent, "solo", true, 2)
	put(home.ID, SpaceMemberAgent, "finn", true, 0)
	if _, _, err := spaces.PutSpaceMember(ctx, PutSpaceMemberRequest{Scope: scope, SpaceID: "missing", Type: SpaceMemberAgent, MemberID: "finn"}); !errors.Is(err, ErrSpaceNotFound) {
		t.Fatalf("put into missing space: %v", err)
	}
	members, err := spaces.ListSpaceMembers(ctx, scope, work.ID)
	if err != nil || len(members) != 3 || members[0].ID != "finn" || members[1].ID != "eng" || members[2].ID != "solo" {
		t.Fatalf("work members = %+v, err = %v", members, err)
	}

	create := func(key string, owner ObjectiveOwner, spaceID string) (*Conversation, error) {
		conversation, _, err := conversations.CreateConversation(ctx, CreateConversationRequest{
			Scope: scope, Owner: owner, Title: "Chat " + key, SpaceID: spaceID, IdempotencyKey: key,
		})
		return conversation, err
	}
	finn := ObjectiveOwner{Type: OwnerTypeAgent, ID: "finn"}
	defaulted, err := create("finn-default", finn, "")
	if err != nil || defaulted.SpaceID != home.ID {
		t.Fatalf("owner's first space: %+v %v", defaulted, err)
	}
	orphan, err := create("orphan", ObjectiveOwner{Type: OwnerTypeAgent, ID: "nobody"}, "")
	if err != nil || orphan.SpaceID != personal.ID {
		t.Fatalf("personal fallback: %+v %v", orphan, err)
	}
	inWork, err := create("finn-work", finn, work.ID)
	if err != nil || inWork.SpaceID != work.ID {
		t.Fatalf("explicit space: %+v %v", inWork, err)
	}
	teamChat, err := create("eng", ObjectiveOwner{Type: OwnerTypeTeam, ID: "eng"}, "")
	if err != nil || teamChat.SpaceID != work.ID {
		t.Fatalf("team space: %+v %v", teamChat, err)
	}
	if _, err := create("missing", finn, "missing"); !errors.Is(err, ErrSpaceNotFound) {
		t.Fatalf("unknown space: %v", err)
	}
	if replay, err := create("finn-work", finn, ""); err != nil || replay.ID != inWork.ID {
		t.Fatalf("replay without space: %+v %v", replay, err)
	}
	if _, err := create("finn-work", finn, home.ID); !errors.Is(err, ErrMessageConflict) {
		t.Fatalf("replay with another space: %v", err)
	}
	if stored, err := conversations.GetConversation(ctx, scope, inWork.ID); err != nil || stored.SpaceID != work.ID {
		t.Fatalf("stored = %+v %v", stored, err)
	}
	assertSpaceConversations(t, conversations, scope, work.ID, inWork.ID, teamChat.ID)

	title := "Renamed"
	updated, err := conversations.UpdateConversation(ctx, UpdateConversationRequest{Scope: scope, ConversationID: inWork.ID, ExpectedRevision: inWork.Revision, Title: &title})
	if err != nil || updated.SpaceID != work.ID {
		t.Fatalf("update keeps space: %+v %v", updated, err)
	}

	if err := spaces.DeleteSpace(ctx, scope, personal.ID); !errors.Is(err, ErrPersonalSpaceProtected) {
		t.Fatalf("delete personal: %v", err)
	}
	if err := spaces.DeleteSpace(ctx, scope, work.ID); err != nil {
		t.Fatal(err)
	}
	if err := spaces.DeleteSpace(ctx, scope, work.ID); !errors.Is(err, ErrSpaceNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if _, err := spaces.ListSpaceMembers(ctx, scope, work.ID); !errors.Is(err, ErrSpaceNotFound) {
		t.Fatalf("members of deleted space: %v", err)
	}
	assertSpaceConversations(t, conversations, scope, work.ID)
	assertSpaceConversations(t, conversations, scope, personal.ID, orphan.ID, inWork.ID, teamChat.ID)
	if moved, err := conversations.GetConversation(ctx, scope, inWork.ID); err != nil || moved.SpaceID != personal.ID || moved.Title != title {
		t.Fatalf("moved conversation = %+v %v", moved, err)
	}
	// Agents stay where else they are; anyone left in no space joins Personal.
	personalMembers, err := spaces.ListSpaceMembers(ctx, scope, personal.ID)
	if err != nil || len(personalMembers) != 2 || personalMembers[0].ID != "eng" || personalMembers[1].ID != "solo" ||
		personalMembers[0].Position != 0 || personalMembers[1].Position != 1 {
		t.Fatalf("personal members = %+v, err = %v", personalMembers, err)
	}
	if homeMembers, err := spaces.ListSpaceMembers(ctx, scope, home.ID); err != nil || len(homeMembers) != 1 || homeMembers[0].ID != "finn" {
		t.Fatalf("home members = %+v, err = %v", homeMembers, err)
	}
	if err := spaces.DeleteSpaceMember(ctx, scope, home.ID, SpaceMemberAgent, "finn"); err != nil {
		t.Fatal(err)
	}
	if err := spaces.DeleteSpaceMember(ctx, scope, home.ID, SpaceMemberAgent, "finn"); err != nil {
		t.Fatalf("delete absent member: %v", err)
	}
	if homeMembers, err := spaces.ListSpaceMembers(ctx, scope, home.ID); err != nil || len(homeMembers) != 0 {
		t.Fatalf("home members after delete = %+v, err = %v", homeMembers, err)
	}

	placed, err := spaces.EnsureMemberHasSpace(ctx, scope, SpaceMemberAgent, "fresh")
	if err != nil || placed == nil || placed.SpaceID != personal.ID || placed.Position != 2 {
		t.Fatalf("fresh deployment placed = %+v, err = %v", placed, err)
	}
	for _, id := range []string{"fresh", "eng"} {
		memberType := SpaceMemberAgent
		if id == "eng" {
			memberType = SpaceMemberTeam
		}
		if again, err := spaces.EnsureMemberHasSpace(ctx, scope, memberType, id); err != nil || again != nil {
			t.Fatalf("%s placed again = %+v, err = %v", id, again, err)
		}
	}
	// "finn" is no longer in any space after its removal from home.
	if placed, err := spaces.EnsureMemberHasSpace(ctx, scope, SpaceMemberAgent, "finn"); err != nil || placed == nil || placed.SpaceID != personal.ID {
		t.Fatalf("finn placed = %+v, err = %v", placed, err)
	}
	freshScope := Scope{Kind: scope.Kind, ID: scope.ID + "-fresh"}
	if placed, err := spaces.EnsureMemberHasSpace(ctx, freshScope, SpaceMemberTeam, "crew"); err != nil || placed == nil ||
		placed.SpaceID != PersonalSpaceID(freshScope) || placed.Position != 0 {
		t.Fatalf("placement creates Personal = %+v, err = %v", placed, err)
	}

	other := Scope{Kind: scope.Kind, ID: scope.ID + "-other"}
	if otherSpaces, err := spaces.ListSpaces(ctx, other); err != nil || len(otherSpaces) != 1 || otherSpaces[0].ID != PersonalSpaceID(other) {
		t.Fatalf("other scope = %+v, err = %v", otherSpaces, err)
	}
	if _, err := spaces.GetSpace(ctx, other, home.ID); !errors.Is(err, ErrSpaceNotFound) {
		t.Fatalf("cross-scope get: %v", err)
	}
}

func assertSpaceOrder(t *testing.T, spaces *SpaceService, scope Scope, ids ...string) {
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

func assertSpaceConversations(t *testing.T, conversations *ConversationService, scope Scope, spaceID string, ids ...string) {
	t.Helper()
	listed, err := conversations.ListConversations(context.Background(), ConversationFilter{Scope: scope, SpaceID: spaceID})
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
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "legacy"}
	agentOnly := Scope{Kind: "tenant", ID: "agents-only"}
	conversation, _, err := NewConversationService(store).CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "helper"}, Title: "Old chat", IdempotencyKey: "old",
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
		if _, err := store.db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("%s: %v", statement.query, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertSpacesBackfilled(t, reopened, scope, agentOnly, conversation.ID)
}

func assertSpacesBackfilled(t *testing.T, store spaceConversationStore, scope, agentOnly Scope, conversationID string) {
	t.Helper()
	ctx := context.Background()
	listed, err := store.ListSpaces(ctx, scope)
	if err != nil || len(listed) != 1 || listed[0].ID != PersonalSpaceID(scope) || !listed[0].IsPersonal ||
		listed[0].Name != PersonalSpaceName || listed[0].Look != SpaceLookTidepool || listed[0].Position != 0 {
		t.Fatalf("backfilled spaces = %+v, err = %v", listed, err)
	}
	members, err := store.ListSpaceMembers(ctx, scope, PersonalSpaceID(scope))
	if err != nil || len(members) != 3 {
		t.Fatalf("backfilled members = %+v, err = %v", members, err)
	}
	want := []SpaceMember{{Type: SpaceMemberAgent, ID: "helper"}, {Type: SpaceMemberAgent, ID: "writer"}, {Type: SpaceMemberTeam, ID: "crew"}}
	for index, member := range members {
		if member.Type != want[index].Type || member.ID != want[index].ID || member.Position != index {
			t.Fatalf("member %d = %+v", index, member)
		}
	}
	conversation, err := store.GetConversation(ctx, scope, conversationID)
	if err != nil || conversation == nil || conversation.SpaceID != PersonalSpaceID(scope) {
		t.Fatalf("backfilled conversation = %+v, err = %v", conversation, err)
	}
	listedConversations, err := store.ListConversations(ctx, ConversationFilter{Scope: scope, SpaceID: PersonalSpaceID(scope)})
	if err != nil || len(listedConversations) != 1 {
		t.Fatalf("space filter after backfill = %d, err = %v", len(listedConversations), err)
	}
	if solo, err := store.ListSpaceMembers(ctx, agentOnly, PersonalSpaceID(agentOnly)); err != nil || len(solo) != 1 || solo[0].ID != "solo" {
		t.Fatalf("agent-only scope = %+v, err = %v", solo, err)
	}
}
