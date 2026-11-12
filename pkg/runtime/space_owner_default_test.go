package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestSQLiteOwnerDefaultSpaces(t *testing.T) {
	t.Parallel()
	store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "owner-default.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	exerciseOwnerDefaultSpaces(t, store)
}

// exerciseOwnerDefaultSpaces is the default-space contract shared by the
// SQLite and PostgreSQL stores: nothing creates a space but
// CreateSpace, an owner's first space in a scope is their default space, and
// what a deleted space held goes to its owner's default space only.
func exerciseOwnerDefaultSpaces(t *testing.T, store storetest.SpaceConversationStore) {
	t.Helper()
	ctx := context.Background()
	clock := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	tick := models.WithClock(func() time.Time { clock = clock.Add(time.Second); return clock })
	spaces := portfolio.NewSpaceService(store, tick)
	conversations := conversation.NewConversationService(store, tick)
	scope := models.Scope{Kind: "tenant", ID: "owner-default-" + strings.ReplaceAll(t.Name(), "/", "-")}
	const ana, ben, solo, stranger = "7", "8", "", "404"
	drifter := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "drifter"}

	create := func(key string, owner models.ObjectiveOwner, spaceID, createdBy string) (*models.Conversation, error) {
		conversation, _, err := conversations.CreateConversation(ctx, conversation.CreateConversationRequest{
			Scope: scope, Owner: owner, Title: "Chat " + key, SpaceID: spaceID, CreatedBy: createdBy, IdempotencyKey: key,
		})
		return conversation, err
	}
	noDefault := func(ownerID string) {
		t.Helper()
		if space, err := spaces.DefaultSpace(ctx, scope, ownerID); !errors.Is(err, models.ErrNoDefaultSpace) || space != nil {
			t.Fatalf("DefaultSpace(%q) = %+v, err = %v; want ErrNoDefaultSpace", ownerID, space, err)
		}
	}
	defaultOf := func(ownerID, want string) {
		t.Helper()
		space, err := spaces.DefaultSpace(ctx, scope, ownerID)
		if err != nil || space.ID != want || !space.IsDefault || space.OwnerID != ownerID {
			t.Fatalf("DefaultSpace(%q) = %+v, err = %v; want %s", ownerID, space, err, want)
		}
	}
	listed := func(want ...string) []*models.Space {
		t.Helper()
		got, err := spaces.ListSpaces(ctx, scope)
		if err != nil || len(got) != len(want) {
			t.Fatalf("spaces = %+v, err = %v; want %v", got, err, want)
		}
		for index, space := range got {
			if space.ID != want[index] {
				t.Fatalf("space %d = %s, want %s", index, space.ID, want[index])
			}
		}
		return got
	}
	members := func(spaceID string, want ...string) {
		t.Helper()
		got, err := spaces.ListSpaceMembers(ctx, scope, spaceID)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, 0, len(got))
		for _, member := range got {
			ids = append(ids, string(member.Type)+":"+member.ID)
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("members of %s = %v, want %v", spaceID, ids, want)
		}
	}
	memberships := func(want int) {
		t.Helper()
		if got, err := spaces.ListScopeSpaceMembers(ctx, scope); err != nil || len(got) != want {
			t.Fatalf("scope memberships = %+v, err = %v; want %d", got, err, want)
		}
	}
	space := func(name, ownerID string, wantDefault bool) *models.Space {
		t.Helper()
		created, err := spaces.CreateSpace(ctx, portfolio.CreateSpaceRequest{Scope: scope, Name: name, Look: models.DefaultSpaceLook, OwnerID: ownerID})
		if err != nil || created.IsDefault != wantDefault || created.OwnerID != ownerID || created.Access != models.SpaceAccessOwner {
			t.Fatalf("space %s of %q = %+v, err = %v; want default %v", name, ownerID, created, err, wantDefault)
		}
		return created
	}

	// An empty scope stays empty: listing, asking for a default space, placing
	// a deployment and creating a conversation create no space.
	listed()
	noDefault(ana)
	noDefault(solo)
	if placed, err := spaces.EnsureMemberHasSpace(ctx, scope, ana, models.SpaceMemberAgent, "early"); !errors.Is(err, models.ErrNoDefaultSpace) || placed != nil {
		t.Fatalf("placement in an empty scope = %+v, err = %v", placed, err)
	}
	for _, createdBy := range []string{ana, solo} {
		if conversation, err := create("empty-"+createdBy, drifter, "", createdBy); !errors.Is(err, models.ErrNoDefaultSpace) || conversation != nil {
			t.Fatalf("conversation in an empty scope by %q = %+v, err = %v", createdBy, conversation, err)
		}
	}
	listed()
	noDefault(ana)
	noDefault(solo)
	memberships(0)
	if all, err := conversations.ListConversations(ctx, models.ConversationFilter{Scope: scope}); err != nil || len(all) != 0 {
		t.Fatalf("conversations in an empty scope = %+v, err = %v", all, err)
	}

	// The first space of an owner is their default space; a second is not.
	anaHome := space("Ana home", ana, true)
	anaWork := space("Ana work", ana, false)
	defaultOf(ana, anaHome.ID)
	// Another owner's first space is their own default space.
	noDefault(ben)
	benHome := space("Ben home", ben, true)
	benWork := space("Ben work", ben, false)
	defaultOf(ben, benHome.ID)
	defaultOf(ana, anaHome.ID)
	// The empty owner is an owner like any other.
	noDefault(solo)
	soloHome := space("Solo home", solo, true)
	soloWork := space("Solo work", solo, false)
	defaultOf(solo, soloHome.ID)
	noDefault(stranger)
	for _, stored := range listed(anaHome.ID, anaWork.ID, benHome.ID, benWork.ID, soloHome.ID, soloWork.ID) {
		wantDefault := stored.ID == anaHome.ID || stored.ID == benHome.ID || stored.ID == soloHome.ID
		if stored.IsDefault != wantDefault {
			t.Fatalf("space %s default = %v, want %v", stored.Name, stored.IsDefault, wantDefault)
		}
	}

	// A default space cannot be deleted.
	for _, id := range []string{anaHome.ID, benHome.ID, soloHome.ID} {
		if err := spaces.DeleteSpace(ctx, scope, id); !errors.Is(err, models.ErrDefaultSpaceProtected) {
			t.Fatalf("delete default space %s: %v", id, err)
		}
	}
	listed(anaHome.ID, anaWork.ID, benHome.ID, benWork.ID, soloHome.ID, soloWork.ID)

	// A conversation with no space whose owner is in no space goes to the
	// default space of the person who created it.
	drifted := map[string]string{}
	for createdBy, want := range map[string]string{ana: anaHome.ID, ben: benHome.ID, solo: soloHome.ID} {
		conversation, err := create("drifter-"+createdBy, drifter, "", createdBy)
		if err != nil || conversation.SpaceID != want || conversation.CreatedBy != createdBy {
			t.Fatalf("conversation by %q = %+v, err = %v; want space %s", createdBy, conversation, err, want)
		}
		drifted[createdBy] = conversation.ID
	}
	if conversation, err := create("drifter-stranger", drifter, "", stranger); !errors.Is(err, models.ErrNoDefaultSpace) || conversation != nil {
		t.Fatalf("conversation by a person without a default space = %+v, err = %v", conversation, err)
	}
	assertSpaceConversations(t, conversations, scope, anaHome.ID, drifted[ana])

	put := func(spaceID string, memberType models.SpaceMemberType, id string) {
		t.Helper()
		if _, _, err := spaces.PutSpaceMember(ctx, portfolio.PutSpaceMemberRequest{Scope: scope, SpaceID: spaceID, Type: memberType, MemberID: id}); err != nil {
			t.Fatal(err)
		}
	}
	put(benWork.ID, models.SpaceMemberAgent, "sole")
	put(benWork.ID, models.SpaceMemberAgent, "shared")
	put(benWork.ID, models.SpaceMemberTeam, "crew")
	put(anaWork.ID, models.SpaceMemberAgent, "shared")
	// The owner deployment's own space wins over the creator's default space.
	placed, err := create("sole-by-ana", models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "sole"}, "", ana)
	if err != nil || placed.SpaceID != benWork.ID {
		t.Fatalf("conversation of a placed owner = %+v, err = %v", placed, err)
	}
	explicit, err := create("explicit", drifter, benWork.ID, ana)
	if err != nil || explicit.SpaceID != benWork.ID {
		t.Fatalf("explicit space = %+v, err = %v", explicit, err)
	}

	// Deleting a space moves what it held to its owner's default space and to
	// nobody else's. A member that is in another space is not added.
	if err := spaces.DeleteSpace(ctx, scope, benWork.ID); err != nil {
		t.Fatal(err)
	}
	listed(anaHome.ID, anaWork.ID, benHome.ID, soloHome.ID, soloWork.ID)
	assertSpaceConversations(t, conversations, scope, benHome.ID, drifted[ben], placed.ID, explicit.ID)
	assertSpaceConversations(t, conversations, scope, anaHome.ID, drifted[ana])
	assertSpaceConversations(t, conversations, scope, soloHome.ID, drifted[solo])
	assertSpaceConversations(t, conversations, scope, anaWork.ID)
	members(benHome.ID, "agent:sole", "team:crew")
	members(anaHome.ID)
	members(soloHome.ID)
	members(soloWork.ID)
	members(anaWork.ID, "agent:shared")
	memberships(3)
	defaultOf(ben, benHome.ID)

	// A deployment joins the default space of the person who created it, and
	// stays in no space when that person has none.
	if placed, err := spaces.EnsureMemberHasSpace(ctx, scope, stranger, models.SpaceMemberAgent, "newbie"); !errors.Is(err, models.ErrNoDefaultSpace) || placed != nil {
		t.Fatalf("placement for a person without a default space = %+v, err = %v", placed, err)
	}
	memberships(3)
	noDefault(stranger)
	if placed, err := spaces.EnsureMemberHasSpace(ctx, scope, solo, models.SpaceMemberAgent, "newbie"); err != nil || placed == nil || placed.SpaceID != soloHome.ID {
		t.Fatalf("placement for the empty owner = %+v, err = %v", placed, err)
	}
	if again, err := spaces.EnsureMemberHasSpace(ctx, scope, ana, models.SpaceMemberAgent, "newbie"); err != nil || again != nil {
		t.Fatalf("placed again = %+v, err = %v", again, err)
	}
	members(soloHome.ID, "agent:newbie")
	members(anaHome.ID)
	memberships(4)
}

type ownerDefaultSpaceRow struct {
	ID        string
	ScopeID   string
	Owner     string
	IsDefault bool
}

// A database from before per-owner default spaces: opening it marks the
// oldest space of every owner who has no default space, keeps the default
// space an owner has, and leaves the legacy Default space as it is. Opening
// it again changes nothing.
func TestSQLiteOwnerDefaultSpaceMigration(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "owner-default-migration.db")
	store, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := openTestSQLiteDB(t, path)
	scope := models.Scope{Kind: "tenant", ID: "legacy"}
	other := models.Scope{Kind: "tenant", ID: "legacy-other"}
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	hour := func(n int) time.Time { return base.Add(time.Duration(n) * time.Hour) }
	rows := []struct {
		scope     models.Scope
		id, owner string
		isDefault bool
		createdAt time.Time
	}{
		// The scope's one legacy Default space, owned by no person.
		{scope, storetest.LegacyDefaultSpaceID(scope), storetest.LegacyDefaultSpaceOwner, true, hour(0)},
		// The oldest space wins, whatever the id and position say.
		{scope, "ana-newer", "7", false, hour(3)},
		{scope, "ana-oldest", "7", false, hour(1)},
		{scope, "ana-middle", "7", false, hour(2)},
		// The same creation time falls back to the id.
		{scope, "ben-b", "8", false, hour(4)},
		{scope, "ben-a", "8", false, hour(4)},
		// The empty owner is an owner.
		{scope, "solo-older", "", false, hour(5)},
		{scope, "solo-newer", "", false, hour(6)},
		// An owner is handled per scope, and one who has a default space keeps
		// it although an older space exists.
		{other, "ana-other-older", "7", false, hour(1)},
		{other, "ana-other-default", "7", true, hour(7)},
	}
	for _, statement := range []string{
		`DROP INDEX idx_spaces_owner_default`,
		`CREATE UNIQUE INDEX idx_spaces_default ON spaces(scope_kind, scope_id) WHERE is_default = 1`,
	} {
		if _, err := sqlDB.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	for position, row := range rows {
		access := models.SpaceAccessOwner
		if row.owner == storetest.LegacyDefaultSpaceOwner {
			access = models.SpaceAccessEveryone
		}
		if _, err := sqlDB.Exec(`INSERT INTO spaces (`+sqlstore.SpaceColumns+`) VALUES (?, ?, ?, ?, 'tidepool', ?, ?, ?, ?, ?, ?)`,
			row.id, row.scope.Kind, row.scope.ID, "Space "+row.id, position, row.isDefault, access, row.owner, row.createdAt, row.createdAt); err != nil {
			t.Fatalf("insert %s: %v", row.id, err)
		}
	}
	legacySnapshot := sqliteSpacesSnapshot(t, sqlDB)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	wantDefaults := []string{
		"legacy-other/ana-other-default", "legacy/" + storetest.LegacyDefaultSpaceID(scope), "legacy/ana-oldest", "legacy/ben-a", "legacy/solo-older",
	}
	slices.Sort(wantDefaults)
	assertSQLiteOwnerDefaults(t, sqlDB, len(rows), wantDefaults)
	migrated := sqliteSpacesSnapshot(t, sqlDB)
	// Only is_default changed, and only on the three spaces that were marked.
	if changed := snapshotDifferences(legacySnapshot, migrated); !slices.Equal(changed, []string{"legacy/ana-oldest", "legacy/ben-a", "legacy/solo-older"}) {
		t.Fatalf("rows changed by the migration = %v", changed)
	}
	legacyKey := "legacy/" + storetest.LegacyDefaultSpaceID(scope)
	if migrated[legacyKey] != legacySnapshot[legacyKey] || !strings.Contains(migrated[legacyKey], "|"+storetest.LegacyDefaultSpaceOwner+"|") {
		t.Fatalf("legacy Default space = %q, was %q", migrated[legacyKey], legacySnapshot[legacyKey])
	}
	var indexes string
	if err := sqlDB.QueryRow(`SELECT COALESCE(group_concat(name, ','), '') FROM (SELECT name FROM sqlite_master
		WHERE type = 'index' AND name IN ('idx_spaces_default', 'idx_spaces_owner_default') ORDER BY name)`).Scan(&indexes); err != nil || indexes != "idx_spaces_owner_default" {
		t.Fatalf("default space indexes = %q, err = %v", indexes, err)
	}
	ctx := context.Background()
	for owner, want := range map[string]string{"7": "ana-oldest", "8": "ben-a", "": "solo-older", storetest.LegacyDefaultSpaceOwner: storetest.LegacyDefaultSpaceID(scope)} {
		if space, err := reopened.DefaultSpace(ctx, scope, owner); err != nil || space.ID != want {
			t.Fatalf("default space of %q = %+v, err = %v; want %s", owner, space, err, want)
		}
	}
	if space, err := reopened.DefaultSpace(ctx, other, "7"); err != nil || space.ID != "ana-other-default" {
		t.Fatalf("kept default space = %+v, err = %v", space, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	// The migration runs on every open.
	again, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close() })
	if changed := snapshotDifferences(migrated, sqliteSpacesSnapshot(t, sqlDB)); len(changed) != 0 {
		t.Fatalf("rows changed by opening again = %v", changed)
	}
	assertSQLiteOwnerDefaults(t, sqlDB, len(rows), wantDefaults)
	// The migrated database follows the store contract: a new owner's first
	// space is their default space, and an owner who has one gets no second.
	spaces := portfolio.NewSpaceService(again)
	if first, err := spaces.CreateSpace(ctx, portfolio.CreateSpaceRequest{Scope: scope, Name: "Cy home", Look: models.DefaultSpaceLook, OwnerID: "9"}); err != nil || !first.IsDefault {
		t.Fatalf("new owner's first space = %+v, err = %v", first, err)
	}
	if next, err := spaces.CreateSpace(ctx, portfolio.CreateSpaceRequest{Scope: scope, Name: "Ana more", Look: models.DefaultSpaceLook, OwnerID: "7"}); err != nil || next.IsDefault {
		t.Fatalf("space of an owner with a default space = %+v, err = %v", next, err)
	}
}

// sqliteSpacesSnapshot returns every spaces row as text, keyed by scope/id.
func sqliteSpacesSnapshot(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT scope_id, id, name, look, position, is_default, access, owner_id,
		CAST(created_at AS TEXT), CAST(updated_at AS TEXT) FROM spaces`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	snapshot := map[string]string{}
	for rows.Next() {
		var scopeID, id, name, look, access, owner, createdAt, updatedAt string
		var position int
		var isDefault bool
		if err := rows.Scan(&scopeID, &id, &name, &look, &position, &isDefault, &access, &owner, &createdAt, &updatedAt); err != nil {
			t.Fatal(err)
		}
		snapshot[scopeID+"/"+id] = fmt.Sprintf("%s|%s|%d|%v|%s|%s|%s|%s", name, look, position, isDefault, access, owner, createdAt, updatedAt)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// snapshotDifferences returns the sorted keys whose rows differ.
func snapshotDifferences(before, after map[string]string) []string {
	var changed []string
	for key, row := range after {
		if before[key] != row {
			changed = append(changed, key)
		}
	}
	for key := range before {
		if _, ok := after[key]; !ok {
			changed = append(changed, key)
		}
	}
	slices.Sort(changed)
	return changed
}

func assertSQLiteOwnerDefaults(t *testing.T, db *sql.DB, wantRows int, wantDefaults []string) {
	t.Helper()
	rows, err := db.Query(`SELECT scope_id, id, owner_id, is_default FROM spaces ORDER BY scope_id, id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var all []ownerDefaultSpaceRow
	var defaults []string
	for rows.Next() {
		var row ownerDefaultSpaceRow
		if err := rows.Scan(&row.ScopeID, &row.ID, &row.Owner, &row.IsDefault); err != nil {
			t.Fatal(err)
		}
		all = append(all, row)
		if row.IsDefault {
			defaults = append(defaults, row.ScopeID+"/"+row.ID)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	slices.Sort(defaults)
	if len(all) != wantRows || !slices.Equal(defaults, wantDefaults) {
		t.Fatalf("default spaces = %v of %d rows, want %v of %d; rows = %+v", defaults, len(all), wantDefaults, wantRows, all)
	}
}

// Handing a space over never takes the new owner's default away: the space
// stops being a default unless its new owner has none, the former owner gets
// a default back, and an automatic pick skips a space shared with everyone.
func TestSQLiteSetSpaceOwnerKeepsDefaults(t *testing.T) {
	t.Parallel()
	store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "owner-change.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	exerciseSetSpaceOwnerDefaults(t, store)
}

func exerciseSetSpaceOwnerDefaults(t *testing.T, store models.SpaceStore) {
	t.Helper()
	ctx, scope := context.Background(), models.Scope{Kind: "tenant", ID: "owner-change-" + strings.ReplaceAll(t.Name(), "/", "-")}
	clock := time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC)
	spaces := portfolio.NewSpaceService(store, models.WithClock(func() time.Time { clock = clock.Add(time.Minute); return clock }))
	create := func(name, owner string, access models.SpaceAccess) *models.Space {
		t.Helper()
		space, err := spaces.CreateSpace(ctx, portfolio.CreateSpaceRequest{Scope: scope, Name: name, Look: models.DefaultSpaceLook, OwnerID: owner})
		if err != nil {
			t.Fatal(err)
		}
		if access != models.SpaceAccessOwner {
			if space, err = spaces.SetSpaceAccess(ctx, scope, space.ID, access); err != nil {
				t.Fatal(err)
			}
		}
		return space
	}
	defaultOf := func(owner, want string) {
		t.Helper()
		space, err := spaces.DefaultSpace(ctx, scope, owner)
		if want == "" {
			if !errors.Is(err, models.ErrNoDefaultSpace) {
				t.Fatalf("default of %q = %+v, %v; want none", owner, space, err)
			}
			return
		}
		if err != nil || space.ID != want {
			t.Fatalf("default of %q = %+v, %v; want %s", owner, space, err, want)
		}
	}
	hand := func(space *models.Space, owner string, wantDefault bool) {
		t.Helper()
		moved, err := spaces.SetSpaceOwner(ctx, scope, space.ID, owner)
		if err != nil || moved.OwnerID != owner || moved.IsDefault != wantDefault {
			t.Fatalf("handed %s = %+v, %v; want owned by %s, default %v", space.Name, moved, err, owner, wantDefault)
		}
	}

	// The org-wide space is the tenant's oldest and its former owner's default.
	shared := create("Shared", "nobody", models.SpaceAccessEveryone)
	other := create("Other", "nobody", models.SpaceAccessOwner)
	mine := create("Mine", "7", models.SpaceAccessOwner)
	defaultOf("nobody", shared.ID)
	hand(shared, "7", false)
	defaultOf("7", mine.ID)
	defaultOf("nobody", other.ID)

	// Someone with no space gets the handed space as their default; the
	// former owner, left with none, has no default.
	hand(other, "9", true)
	defaultOf("9", other.ID)
	defaultOf("nobody", "")

	// Losing a default re-marks the oldest space not shared with everyone,
	// and only an owner of nothing else gets an everyone space.
	first := create("First", "5", models.SpaceAccessOwner)
	open := create("Open", "5", models.SpaceAccessEveryone)
	private := create("Private", "5", models.SpaceAccessOwner)
	defaultOf("5", first.ID)
	hand(first, "9", false)
	defaultOf("5", private.ID)
	defaultOf("9", other.ID)
	hand(private, "6", true)
	defaultOf("5", open.ID)

	// Handing over a space with a new access re-marks by that access.
	if moved, err := spaces.SetSpaceAccessState(ctx, portfolio.SetSpaceAccessStateRequest{Scope: scope, SpaceID: open.ID, OwnerID: "4", Access: models.SpaceAccessEveryone}); err != nil || !moved.IsDefault {
		t.Fatalf("open to an owner of nothing = %+v, %v; want their default", moved, err)
	}
	defaultOf("5", "")
	if _, err := spaces.SetSpaceOwner(ctx, scope, "missing", "7"); !errors.Is(err, models.ErrSpaceNotFound) {
		t.Fatalf("missing space = %v; want not found", err)
	}
}
