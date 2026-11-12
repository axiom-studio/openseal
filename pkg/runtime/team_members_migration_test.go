package runtime

import (
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestSQLiteTeamMembersMigrationReplacesRoleSlots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "teams.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	plain := func(statement string) string { return statement }
	storetest.SeedRoleSlotTeams(t, store.db, plain)
	reopen := func() {
		t.Helper()
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if store, err = NewSQLiteStore(path); err != nil {
			t.Fatal(err)
		}
	}
	reopen()
	defer func() { _ = store.Close() }()
	storetest.RequireTeamRows(t, store.db, plain, "migrated", storetest.MemberTeamRows)
	storetest.RequireMemberTeamsLoad(t, store)
	reopen()
	storetest.RequireTeamRows(t, store.db, plain, "migrated again", storetest.MemberTeamRows)

	// SQLite has no rollback command; the PostgreSQL rollback runs the same
	// transformation.
	for _, stage := range []string{"rolled back", "rolled back again"} {
		tx, err := store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := sqliteTeamMembersMigration(tx).Down(); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		storetest.RequireTeamRows(t, store.db, plain, stage, storetest.RolledBackTeamRows)
	}
	reopen()
	storetest.RequireMemberTeamsLoad(t, store)
}
