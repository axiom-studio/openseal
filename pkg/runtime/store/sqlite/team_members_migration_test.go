package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestSQLiteTeamMembersMigrationReplacesRoleSlots(t *testing.T) {
	path := filepath.Join(t.TempDir(), "teams.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	db := openTestSQLiteDB(t, path)
	plain := func(statement string) string { return statement }
	storetest.SeedRoleSlotTeams(t, db, plain)
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
	storetest.RequireTeamRows(t, db, plain, "migrated", storetest.MemberTeamRows)
	storetest.RequireMemberTeamsLoad(t, store)
	reopen()
	storetest.RequireTeamRows(t, db, plain, "migrated again", storetest.MemberTeamRows)

	// SQLite has no rollback command; the PostgreSQL rollback runs the same
	// transformation.
	for _, stage := range []string{"rolled back", "rolled back again"} {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := sqliteTeamMembersRollback(tx).Down(); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		storetest.RequireTeamRows(t, db, plain, stage, storetest.RolledBackTeamRows)
	}
	reopen()
	storetest.RequireMemberTeamsLoad(t, store)
}

// sqliteTeamMembersRollback is the shared team members migration on a SQLite
// transaction, configured as the SQLite store configures it. The store never
// runs Down, so the test builds its own to check the rollback transformation
// on SQLite rows.
func sqliteTeamMembersRollback(tx *sql.Tx) sqlstore.TeamMembersMigration {
	return sqlstore.TeamMembersMigration{
		Ctx: context.Background(), DB: tx, Table: func(name string) string { return name }, Placeholder: func(int) string { return "?" },
		Exists: func(table string) (bool, error) {
			var count int
			err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count)
			return count > 0, err
		},
	}
}
