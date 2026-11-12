package runtime

import (
	"path/filepath"
	"testing"
)

func TestSQLiteTeamMembersMigrationReplacesRoleSlots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "teams.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	plain := func(statement string) string { return statement }
	seedRoleSlotTeams(t, store.db, plain)
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
	requireTeamRows(t, store.db, plain, "migrated", memberTeamRows)
	requireMemberTeamsLoad(t, store)
	reopen()
	requireTeamRows(t, store.db, plain, "migrated again", memberTeamRows)

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
		requireTeamRows(t, store.db, plain, stage, rolledBackTeamRows)
	}
	reopen()
	requireMemberTeamsLoad(t, store)
}
