package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestSQLiteSpaceAccess(t *testing.T) {
	t.Parallel()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "access.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	exerciseSpaceAccess(t, store)
}

// exerciseSpaceAccess is the access contract shared by the SQLite
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

	// A person's default space is theirs alone, like any space they create.
	mine, err := spaces.CreateSpace(ctx, CreateSpaceRequest{Scope: scope, Name: "Mine", Look: DefaultSpaceLook, OwnerID: ana})
	if err != nil || mine.Access != SpaceAccessOwner || mine.OwnerID != ana || !mine.IsDefault {
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
	visible(scope, ana, mine.ID, invited.ID, open.ID)
	visible(scope, ben, open.ID)
	visible(scope, "", open.ID)
	visible(other, ana, theirs.ID)
	visible(other, ben)

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
	visible(scope, ben, invited.ID, open.ID)
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
	check(open.ID, ana, false, true, true)
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
	visible(scope, ben, open.ID)
	visible(scope, cy, invited.ID, open.ID)

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
	visible(scope, cy, open.ID)
	if people, err := spaces.ListSpacePeople(ctx, scope, mine.ID); err != nil || len(people) != 1 {
		t.Fatalf("people of another space = %+v, err = %v", people, err)
	}

	exerciseSpaceAccessState(t, spaces, scope, other, mine.ID, open.ID)
}

// exerciseSpaceAccessState covers the write Axiom Cloud keeps the kernel
// equal to its own record with: the whole access of a space at once.
func exerciseSpaceAccessState(t *testing.T, spaces *SpaceService, scope, other Scope, anasDefault, spaceID string) {
	t.Helper()
	ctx := context.Background()
	const ana, ben, cy, dee = "7", "8", "9", "10"

	state := func(owner, wantOwner string, access SpaceAccess, people ...string) *Space {
		t.Helper()
		stored, err := spaces.SetSpaceAccessState(ctx, SetSpaceAccessStateRequest{Scope: scope, SpaceID: spaceID, OwnerID: owner, Access: access, People: people, AddedBy: "system"})
		if err != nil || stored.OwnerID != wantOwner || stored.Access != access {
			t.Fatalf("state = %+v, err = %v", stored, err)
		}
		return stored
	}
	isDefault := func(id string) bool {
		t.Helper()
		stored, err := spaces.GetSpace(ctx, scope, id)
		if err != nil {
			t.Fatal(err)
		}
		return stored.IsDefault
	}
	peopleOf := func() []string {
		t.Helper()
		people, err := spaces.ListSpacePeople(ctx, scope, spaceID)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(people))
		for _, person := range people {
			ids = append(ids, person.UserID)
		}
		slices.Sort(ids)
		return ids
	}
	if _, _, err := spaces.AddSpacePerson(ctx, AddSpacePersonRequest{Scope: scope, SpaceID: spaceID, UserID: cy, AddedBy: ana}); err != nil {
		t.Fatal(err)
	}
	before, err := spaces.ListSpacePeople(ctx, scope, spaceID)
	if err != nil || len(before) != 1 {
		t.Fatalf("people before = %+v, err = %v", before, err)
	}
	// Handing the space to someone with no space makes it their default, as
	// SetSpaceOwner does; the former owner keeps their own default.
	if handed := state(" "+ben+" ", ben, SpaceAccessInvited, cy, dee, dee); !handed.IsDefault || !isDefault(anasDefault) {
		t.Fatalf("handed = %+v; ana's default kept = %v", handed, isDefault(anasDefault))
	}
	if got := peopleOf(); !slices.Equal(got, []string{dee, cy}) {
		t.Fatalf("people = %v", got)
	}
	after, err := spaces.ListSpacePeople(ctx, scope, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, person := range after {
		if person.UserID == cy && (person.AddedBy != ana || !person.AddedAt.Equal(before[0].AddedAt)) {
			t.Fatalf("a person who stays keeps when they were added: %+v", person)
		}
	}
	if visible, err := spaces.VisibleSpaces(ctx, scope, dee); err != nil || !slices.Contains(visible, spaceID) {
		t.Fatalf("an added person sees an invited space: %v, err = %v", visible, err)
	}
	if visible, err := spaces.VisibleSpaces(ctx, scope, ana); err != nil || slices.Contains(visible, spaceID) {
		t.Fatalf("the former owner no longer sees it: %v, err = %v", visible, err)
	}
	// Repeating the same state changes nothing.
	state(ben, ben, SpaceAccessInvited, dee, cy)
	if got := peopleOf(); !slices.Equal(got, []string{dee, cy}) {
		t.Fatalf("people after repeat = %v", got)
	}
	// An owner the host cannot name yet leaves the owner as it is.
	state("", ben, SpaceAccessOwner)
	if got := peopleOf(); len(got) != 0 {
		t.Fatalf("people after clearing = %v", got)
	}
	// Back with ana, the space is no longer anyone's default: ana's is older.
	if back := state(ana, ana, SpaceAccessEveryone); back.IsDefault || !isDefault(anasDefault) {
		t.Fatalf("back = %+v", back)
	}
	if _, err := spaces.SetSpaceAccessState(ctx, SetSpaceAccessStateRequest{Scope: scope, SpaceID: spaceID, OwnerID: "a/b", Access: SpaceAccessOwner}); !errors.Is(err, ErrInvalidSpace) {
		t.Fatalf("state with a bad owner: %v", err)
	}
	if _, err := spaces.SetSpaceAccessState(ctx, SetSpaceAccessStateRequest{Scope: scope, SpaceID: spaceID, OwnerID: ana, Access: "public"}); !errors.Is(err, ErrInvalidSpace) {
		t.Fatalf("state with bad access: %v", err)
	}
	if _, err := spaces.SetSpaceAccessState(ctx, SetSpaceAccessStateRequest{Scope: scope, SpaceID: spaceID, OwnerID: ana, Access: SpaceAccessInvited, People: []string{" "}}); !errors.Is(err, ErrInvalidSpace) {
		t.Fatalf("state with an empty person: %v", err)
	}
	if _, err := spaces.SetSpaceAccessState(ctx, SetSpaceAccessStateRequest{Scope: other, SpaceID: spaceID, OwnerID: ana, Access: SpaceAccessOwner}); !errors.Is(err, ErrSpaceNotFound) {
		t.Fatalf("state across scopes: %v", err)
	}
	if _, err := spaces.SetSpaceAccessState(ctx, SetSpaceAccessStateRequest{Scope: scope, SpaceID: "missing", OwnerID: ana, Access: SpaceAccessOwner}); !errors.Is(err, ErrSpaceNotFound) {
		t.Fatalf("state of a missing space: %v", err)
	}
}

// A database from before space access keeps created_by and one Default space
// per scope owned by no person: opening it renames the column, makes every
// space "owner" and the Default space "everyone".
func TestSQLiteSpaceAccessBackfill(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "legacy.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := openTestSQLiteDB(t, path)
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
	if _, err := sqlDB.Exec(`UPDATE spaces SET is_default = 0, position = 1 WHERE id = ?`, work.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO spaces (`+postgresSpaceColumns+`) VALUES (?, ?, ?, 'Default', 'tidepool', 0, 1, 'everyone', ?, ?, ?)`,
		storetest.LegacyDefaultSpaceID(scope), scope.Kind, scope.ID, storetest.LegacyDefaultSpaceOwner, work.CreatedAt.Add(-time.Hour), work.CreatedAt.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP INDEX idx_spaces_owner_default`,
		`CREATE UNIQUE INDEX idx_spaces_default ON spaces(scope_kind, scope_id) WHERE is_default = 1`,
		`ALTER TABLE spaces DROP COLUMN access`,
		`ALTER TABLE spaces RENAME COLUMN owner_id TO created_by`,
		`DROP TABLE space_people`,
	} {
		if _, err := sqlDB.Exec(statement); err != nil {
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
		storetest.AssertSpaceAccessBackfilled(t, reopened, scope, work.ID)
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
