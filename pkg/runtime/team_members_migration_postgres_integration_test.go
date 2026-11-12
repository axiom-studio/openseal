//go:build integration

package runtime

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

func TestPostgresTeamMembersMigrationReplacesRoleSlotsAndRollsBackTwice(t *testing.T) {
	pgSchema := newTestPostgresSchema(t, "members_")
	dsn := pgSchema.dsn
	schema := pgSchema.name
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pgSchema.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+pgSchema.quoted()+` CASCADE`)
		_ = store.Close()
	})
	// query qualifies the bare table names and numbers the placeholders.
	query := func(statement string) string {
		for _, table := range []string{"team_definition_amendments", "team_definitions", "team_deployments", "agent_requests"} {
			statement = strings.ReplaceAll(statement, " "+table+" ", " "+pgSchema.table(table)+" ")
		}
		statement = strings.ReplaceAll(statement, "SELECT payload FROM", "SELECT payload::text FROM")
		for n := 1; strings.Contains(statement, "?"); n++ {
			statement = strings.Replace(statement, "?", "$"+strconv.Itoa(n), 1)
		}
		return statement
	}
	reopen := func() {
		t.Helper()
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if store, err = NewPostgresStore(ctx, dsn, WithPostgresSchema(schema)); err != nil {
			t.Fatal(err)
		}
	}
	seedRoleSlotTeams(t, pgSchema.db, query)
	if _, err := pgSchema.db.ExecContext(ctx, `DELETE FROM `+pgSchema.table("schema_migrations")+` WHERE version = $1`, teamMembersMigrationVersion); err != nil {
		t.Fatal(err)
	}
	reopen()
	requireTeamRows(t, pgSchema.db, query, "migrated", memberTeamRows)
	requireMemberTeamsLoad(t, store)
	// A second open finds the migration applied; one that lost only the
	// ledger row runs it again and changes nothing.
	reopen()
	if _, err := pgSchema.db.ExecContext(ctx, `DELETE FROM `+pgSchema.table("schema_migrations")+` WHERE version = $1`, teamMembersMigrationVersion); err != nil {
		t.Fatal(err)
	}
	reopen()
	requireTeamRows(t, pgSchema.db, query, "migrated again", memberTeamRows)

	for _, stage := range []string{"rolled back", "rolled back again"} {
		if err := store.RollbackPostgresMigrations(ctx, teamMembersMigrationVersion-1); err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		requireTeamRows(t, pgSchema.db, query, stage, rolledBackTeamRows)
		if version, err := store.PostgresSchemaVersion(ctx); err != nil || version != teamMembersMigrationVersion-1 {
			t.Fatalf("%s: schema version = %d, %v", stage, version, err)
		}
	}
	reopen()
	requireMemberTeamsLoad(t, store)
	// After a rollback past the Team tables, this rollback finds them gone.
	if err := store.RollbackPostgresMigrations(ctx, 10); err != nil {
		t.Fatal(err)
	}
	tx, err := pgSchema.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := store.rollbackTeamMembers(ctx, tx); err != nil {
		t.Fatal(err)
	}
}
