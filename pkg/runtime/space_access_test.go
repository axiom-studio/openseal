package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMemorySpaceAccess(t *testing.T) {
	t.Parallel()
	exerciseSpaceAccess(t, NewMemoryStore())
}

func TestSQLiteSpaceAccess(t *testing.T) {
	t.Parallel()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "access.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	exerciseSpaceAccess(t, store)
}

// exerciseSpaceAccess is the access contract shared by the memory, SQLite
// and PostgreSQL stores.
func exerciseSpaceAccess(t *testing.T, store SpaceStore) {
	t.Helper()
	ctx := context.Background()
	clock := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	spaces := NewSpaceService(store)
	spaces.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	scope := Scope{Kind: "tenant", ID: "access-" + strings.ReplaceAll(t.Name(), "/", "-")}
	other := Scope{Kind: "tenant", ID: scope.ID + "-other"}
	const ana, ben, cy = "7", "8", "9"

	defaultSpace, _, err := spaces.EnsureDefaultSpace(ctx, scope)
	if err != nil || defaultSpace.Access != SpaceAccessEveryone || defaultSpace.OwnerID != DefaultSpaceOwner {
		t.Fatalf("Default = %+v, err = %v", defaultSpace, err)
	}
	mine, err := spaces.CreateSpace(ctx, CreateSpaceRequest{Scope: scope, Name: "Mine", Look: DefaultSpaceLook, OwnerID: ana})
	if err != nil || mine.Access != SpaceAccessOwner || mine.OwnerID != ana {
		t.Fatalf("new space = %+v, err = %v", mine, err)
	}
	invited, err := spaces.CreateSpace(ctx, CreateSpaceRequest{Scope: scope, Name: "Invited", Look: DefaultSpaceLook, OwnerID: ana})
	if err != nil {
		t.Fatal(err)
	}
	if stored, err := spaces.SetSpaceAccess(ctx, scope, invited.ID, SpaceAccessInvited); err != nil || stored.Access != SpaceAccessInvited || !stored.UpdatedAt.After(invited.UpdatedAt) {
		t.Fatalf("set invited = %+v, err = %v", stored, err)
	}
	open, err := spaces.CreateSpace(ctx, CreateSpaceRequest{Scope: scope, Name: "Open", Look: DefaultSpaceLook, OwnerID: ana})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spaces.SetSpaceAccess(ctx, scope, open.ID, SpaceAccessEveryone); err != nil {
		t.Fatal(err)
	}
	if stored, err := spaces.GetSpace(ctx, scope, open.ID); err != nil || stored.Access != SpaceAccessEveryone {
		t.Fatalf("stored access = %+v, err = %v", stored, err)
	}
	if _, err := spaces.SetSpaceAccess(ctx, scope, open.ID, "public"); !errors.Is(err, ErrInvalidSpace) {
		t.Fatalf("invalid access: %v", err)
	}
	if _, err := spaces.SetSpaceAccess(ctx, scope, "missing", SpaceAccessEveryone); !errors.Is(err, ErrSpaceNotFound) {
		t.Fatalf("access of missing space: %v", err)
	}
	if _, err := spaces.SetSpaceAccess(ctx, other, open.ID, SpaceAccessOwner); !errors.Is(err, ErrSpaceNotFound) {
		t.Fatalf("access across scopes: %v", err)
	}
	theirs, err := spaces.CreateSpace(ctx, CreateSpaceRequest{Scope: other, Name: "Theirs", Look: DefaultSpaceLook, OwnerID: ana})
	if err != nil {
		t.Fatal(err)
	}

	visible := func(scope Scope, userID string, want ...string) {
		t.Helper()
		got, err := spaces.VisibleSpaces(ctx, scope, userID)
		if err != nil || !slices.Equal(got, want) {
			t.Fatalf("VisibleSpaces(%s, %q) = %v, err = %v; want %v", scope.ID, userID, got, err, want)
		}
	}
	visible(scope, ana, defaultSpace.ID, mine.ID, invited.ID, open.ID)
	visible(scope, ben, defaultSpace.ID, open.ID)
	visible(scope, "", defaultSpace.ID, open.ID)
	visible(other, ana, DefaultSpaceID(other), theirs.ID)
	visible(other, ben, DefaultSpaceID(other))

	person, created, err := spaces.AddSpacePerson(ctx, AddSpacePersonRequest{Scope: scope, SpaceID: invited.ID, UserID: " " + ben + " ", AddedBy: ana})
	if err != nil || !created || person.SpaceID != invited.ID || person.UserID != ben || person.AddedBy != ana || person.AddedAt.IsZero() {
		t.Fatalf("added = %+v created=%v err=%v", person, created, err)
	}
	if again, created, err := spaces.AddSpacePerson(ctx, AddSpacePersonRequest{Scope: scope, SpaceID: invited.ID, UserID: ben, AddedBy: cy}); err != nil || created ||
		again.AddedBy != ana || !again.AddedAt.Equal(person.AddedAt) {
		t.Fatalf("added again = %+v created=%v err=%v", again, created, err)
	}
	// A person added to an "owner" space does not see it until it is "invited".
	if _, _, err := spaces.AddSpacePerson(ctx, AddSpacePersonRequest{Scope: scope, SpaceID: mine.ID, UserID: ben, AddedBy: ana}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := spaces.AddSpacePerson(ctx, AddSpacePersonRequest{Scope: scope, SpaceID: invited.ID, UserID: cy, AddedBy: ana}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := spaces.AddSpacePerson(ctx, AddSpacePersonRequest{Scope: scope, SpaceID: "missing", UserID: ben}); !errors.Is(err, ErrSpaceNotFound) {
		t.Fatalf("add to missing space: %v", err)
	}
	if _, _, err := spaces.AddSpacePerson(ctx, AddSpacePersonRequest{Scope: other, SpaceID: invited.ID, UserID: ben}); !errors.Is(err, ErrSpaceNotFound) {
		t.Fatalf("add across scopes: %v", err)
	}
	if _, _, err := spaces.AddSpacePerson(ctx, AddSpacePersonRequest{Scope: scope, SpaceID: invited.ID, UserID: " "}); !errors.Is(err, ErrInvalidSpace) {
		t.Fatalf("add nobody: %v", err)
	}
	visible(scope, ben, defaultSpace.ID, invited.ID, open.ID)
	people, err := spaces.ListSpacePeople(ctx, scope, invited.ID)
	if err != nil || len(people) != 2 || people[0].UserID != ben || people[1].UserID != cy {
		t.Fatalf("people = %+v, err = %v", people, err)
	}
	if _, err := spaces.ListSpacePeople(ctx, other, invited.ID); !errors.Is(err, ErrSpaceNotFound) {
		t.Fatalf("people across scopes: %v", err)
	}

	for _, id := range []string{"finn", "writer"} {
		if _, _, err := spaces.PutSpaceMember(ctx, PutSpaceMemberRequest{Scope: scope, SpaceID: mine.ID, Type: SpaceMemberAgent, MemberID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := spaces.PutSpaceMember(ctx, PutSpaceMemberRequest{Scope: scope, SpaceID: open.ID, Type: SpaceMemberAgent, MemberID: "finn"}); err != nil {
		t.Fatal(err)
	}
	memberships, err := spaces.ListScopeSpaceMembers(ctx, scope)
	if err != nil || len(memberships) != 3 {
		t.Fatalf("scope memberships = %+v, err = %v", memberships, err)
	}
	finnSpaces := 0
	for _, member := range memberships {
		if member.ID == "finn" && (member.SpaceID == mine.ID || member.SpaceID == open.ID) {
			finnSpaces++
		}
	}
	if finnSpaces != 2 {
		t.Fatalf("finn's spaces = %d in %+v", finnSpaces, memberships)
	}
	if theirs, err := spaces.ListScopeSpaceMembers(ctx, other); err != nil || len(theirs) != 0 {
		t.Fatalf("another scope's memberships = %+v, err = %v", theirs, err)
	}

	can := func(name string, got bool, err error, want bool) {
		t.Helper()
		if err != nil || got != want {
			t.Fatalf("%s = %v, err = %v; want %v", name, got, err, want)
		}
	}
	check := func(spaceID, userID string, admin, wantEdit, wantManage bool) {
		t.Helper()
		edit, err := spaces.CanEditSpace(ctx, scope, spaceID, userID)
		can("CanEditSpace("+spaceID+","+userID+")", edit, err, wantEdit)
		manage, err := spaces.CanManageSpace(ctx, scope, spaceID, userID, admin)
		can("CanManageSpace("+spaceID+","+userID+")", manage, err, wantManage)
	}
	check(mine.ID, ana, false, true, true)
	check(mine.ID, ben, false, false, false)
	check(mine.ID, ben, true, false, false) // an admin who cannot see it
	check(invited.ID, ben, false, false, false)
	check(invited.ID, ben, true, false, true)
	check(open.ID, cy, true, false, true)
	check(open.ID, cy, false, false, false)
	check(defaultSpace.ID, ana, false, false, false)
	check(defaultSpace.ID, ana, true, false, true)
	check(defaultSpace.ID, DefaultSpaceOwner, false, true, true)
	check("missing", ana, true, false, false)
	check(mine.ID, "", true, false, false)
	if ok, err := spaces.CanManageSpace(ctx, other, mine.ID, ana, true); err != nil || ok {
		t.Fatalf("manage across scopes = %v, err = %v", ok, err)
	}

	if err := spaces.RemoveSpacePerson(ctx, scope, invited.ID, ben); err != nil {
		t.Fatal(err)
	}
	if err := spaces.RemoveSpacePerson(ctx, scope, invited.ID, ben); err != nil {
		t.Fatalf("remove absent person: %v", err)
	}
	if err := spaces.RemoveSpacePerson(ctx, scope, "missing", ben); !errors.Is(err, ErrSpaceNotFound) {
		t.Fatalf("remove from missing space: %v", err)
	}
	visible(scope, ben, defaultSpace.ID, open.ID)
	visible(scope, cy, defaultSpace.ID, invited.ID, open.ID)

	// Deleting a space deletes its people: a space made again under the same
	// id starts with nobody.
	if err := spaces.DeleteSpace(ctx, scope, invited.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := spaces.ListSpacePeople(ctx, scope, invited.ID); !errors.Is(err, ErrSpaceNotFound) {
		t.Fatalf("people of deleted space: %v", err)
	}
	remade := *invited
	remade.Access = SpaceAccessInvited
	if _, err := store.CreateSpace(ctx, &remade); err != nil {
		t.Fatal(err)
	}
	if people, err := spaces.ListSpacePeople(ctx, scope, invited.ID); err != nil || len(people) != 0 {
		t.Fatalf("people after delete = %+v, err = %v", people, err)
	}
	visible(scope, cy, defaultSpace.ID, open.ID)
	if people, err := spaces.ListSpacePeople(ctx, scope, mine.ID); err != nil || len(people) != 1 {
		t.Fatalf("people of another space = %+v, err = %v", people, err)
	}
}

// A database from before space access keeps created_by: opening it renames
// the column, makes every space "owner" and the Default space "everyone".
func TestSQLiteSpaceAccessBackfill(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "legacy.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "legacy"}
	spaces := NewSpaceService(store)
	work, err := spaces.CreateSpace(ctx, CreateSpaceRequest{Scope: scope, Name: "Work", Look: DefaultSpaceLook, OwnerID: "7"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := spaces.SetSpaceAccess(ctx, scope, work.ID, SpaceAccessEveryone); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`ALTER TABLE spaces DROP COLUMN access`,
		`ALTER TABLE spaces RENAME COLUMN owner_id TO created_by`,
		`DROP TABLE space_people`,
	} {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		reopened, err := NewSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		assertSpaceAccessBackfilled(t, reopened, scope, work.ID)
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func assertSpaceAccessBackfilled(t *testing.T, store SpaceStore, scope Scope, workID string) {
	t.Helper()
	ctx := context.Background()
	listed, err := store.ListSpaces(ctx, scope)
	if err != nil || len(listed) != 2 {
		t.Fatalf("spaces = %+v, err = %v", listed, err)
	}
	if listed[0].ID != DefaultSpaceID(scope) || listed[0].Access != SpaceAccessEveryone || listed[0].OwnerID != DefaultSpaceOwner {
		t.Fatalf("Default = %+v", listed[0])
	}
	if listed[1].ID != workID || listed[1].Access != SpaceAccessOwner || listed[1].OwnerID != "7" {
		t.Fatalf("work = %+v", listed[1])
	}
	if people, err := store.ListSpacePeople(ctx, scope, workID); err != nil || len(people) != 0 {
		t.Fatalf("people = %+v, err = %v", people, err)
	}
	if visible, err := store.VisibleSpaces(ctx, scope, "8"); err != nil || !slices.Equal(visible, []string{DefaultSpaceID(scope)}) {
		t.Fatalf("visible to another person = %v, err = %v", visible, err)
	}
}
