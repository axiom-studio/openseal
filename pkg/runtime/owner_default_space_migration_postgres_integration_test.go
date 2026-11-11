//go:build integration

package runtime

import (
	"context"
	"testing"
	"time"
)

// Migration 86 on a database with one default space per scope: the per-owner
// index replaces the per-scope one, the scope's default space keeps the flag
// for the owner it has, and the oldest space of every other owner gets it.
// Running it again leaves an owner's default space as it is.
func TestPostgresOwnerDefaultSpaceMigration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	store, dsn, schema := openSpacesPostgresStore(t, "openseal_owner_default_")
	scope := Scope{Kind: "tenant", ID: "legacy"}
	peopleOnly := Scope{Kind: "tenant", ID: "people-only"}
	spaces := NewSpaceService(store)
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	spaces.now = func() time.Time { at = at.Add(time.Minute); return at }
	create := func(scope Scope, name, ownerID string) *Space {
		t.Helper()
		space, err := spaces.CreateSpace(ctx, CreateSpaceRequest{Scope: scope, Name: name, Look: DefaultSpaceLook, OwnerID: ownerID})
		if err != nil {
			t.Fatal(err)
		}
		return space
	}
	work := create(scope, "Work", "7")
	side := create(scope, "Side", "7")
	lab := create(scope, "Lab", "8")
	solo := create(peopleOnly, "Solo", "7")
	later := create(peopleOnly, "Later", "8")
	legacyID := legacyDefaultSpaceID(scope)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO `+store.table("spaces")+` (`+postgresSpaceColumns+`)
		VALUES ($1, $2, $3, 'Default', 'tidepool', 5, TRUE, 'everyone', $4, $5, $5)`,
		legacyID, scope.Kind, scope.ID, legacyDefaultSpaceOwner, work.CreatedAt.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	indexes := func(store *PostgresStore) (perScope, perOwner bool) {
		t.Helper()
		if err := store.db.QueryRowContext(ctx, `SELECT
			EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = $1 AND tablename = 'spaces' AND indexname = 'spaces_default_idx'),
			EXISTS (SELECT 1 FROM pg_indexes WHERE schemaname = $1 AND tablename = 'spaces' AND indexname = 'spaces_owner_default_idx')`,
			schema).Scan(&perScope, &perOwner); err != nil {
			t.Fatal(err)
		}
		return perScope, perOwner
	}
	defaults := func(store *PostgresStore) map[string]bool {
		t.Helper()
		rows, err := store.db.QueryContext(ctx, `SELECT id FROM `+store.table("spaces")+` WHERE is_default`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		result := map[string]bool{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			result[id] = true
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	assertDefaults := func(store *PostgresStore, when string, want ...string) {
		t.Helper()
		got := defaults(store)
		if len(got) != len(want) {
			t.Fatalf("default spaces %s = %v, want %v", when, got, want)
		}
		for _, id := range want {
			if !got[id] {
				t.Fatalf("default spaces %s = %v, want %v", when, got, want)
			}
		}
	}

	// Rolling back 86 leaves one default space per scope, the oldest.
	if err := store.RollbackPostgresMigrations(ctx, ownerDefaultSpaceMigrationVersion-1); err != nil {
		t.Fatal(err)
	}
	if perScope, perOwner := indexes(store); !perScope || perOwner {
		t.Fatalf("indexes after rollback: per scope = %v, per owner = %v", perScope, perOwner)
	}
	assertDefaults(store, "after rollback", legacyID, solo.ID)

	reopened, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if version, err := reopened.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}
	if perScope, perOwner := indexes(reopened); perScope || !perOwner {
		t.Fatalf("indexes after migration: per scope = %v, per owner = %v", perScope, perOwner)
	}
	// Each owner's oldest space is their default; Side stays an ordinary one.
	assertDefaults(reopened, "after migration", legacyID, work.ID, lab.ID, solo.ID, later.ID)
	for _, expected := range []struct {
		scope   Scope
		ownerID string
		spaceID string
	}{
		{scope, legacyDefaultSpaceOwner, legacyID},
		{scope, "7", work.ID},
		{scope, "8", lab.ID},
		{peopleOnly, "7", solo.ID},
		{peopleOnly, "8", later.ID},
	} {
		if space, err := reopened.DefaultSpace(ctx, expected.scope, expected.ownerID); err != nil || space.ID != expected.spaceID {
			t.Fatalf("default space of %q in %s = %+v, err = %v", expected.ownerID, expected.scope.ID, space, err)
		}
	}
	// The index holds: an owner cannot have two default spaces.
	if _, err := reopened.db.ExecContext(ctx, `UPDATE `+reopened.table("spaces")+` SET is_default = TRUE WHERE id = $1`, side.ID); err == nil {
		t.Fatal("a second default space for one owner was stored")
	}

	// Run again on a database that lost only the ledger row: an owner who has
	// a default space keeps it, even when it is not their oldest space.
	for _, statement := range []string{
		`UPDATE ` + reopened.table("spaces") + ` SET is_default = FALSE WHERE id = '` + work.ID + `'`,
		`UPDATE ` + reopened.table("spaces") + ` SET is_default = TRUE WHERE id = '` + side.ID + `'`,
		`DELETE FROM ` + reopened.table("schema_migrations") + ` WHERE version = 86`,
	} {
		if _, err := reopened.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	again, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if version, err := again.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("schema version after the second run = %d, err = %v", version, err)
	}
	if perScope, perOwner := indexes(again); perScope || !perOwner {
		t.Fatalf("indexes after the second run: per scope = %v, per owner = %v", perScope, perOwner)
	}
	assertDefaults(again, "after the second run", legacyID, side.ID, lab.ID, solo.ID, later.ID)
}
