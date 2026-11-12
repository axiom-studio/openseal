//go:build integration

package runtime

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestPostgresTeamMembersMigrationReplacesRoleSlotsAndRollsBackTwice(t *testing.T) {
	pgSchema := storetest.NewPostgresSchema(t, "members_")
	dsn := pgSchema.DSN
	schema := pgSchema.Name
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pgSchema.DB.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+pgSchema.Quoted()+` CASCADE`)
		_ = store.Close()
	})
	// query qualifies the bare table names and numbers the placeholders.
	query := func(statement string) string {
		for _, table := range []string{"team_definition_amendments", "team_definitions", "team_deployments", "agent_requests"} {
			statement = strings.ReplaceAll(statement, " "+table+" ", " "+pgSchema.Table(table)+" ")
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
	storetest.SeedRoleSlotTeams(t, pgSchema.DB, query)
	if _, err := pgSchema.DB.ExecContext(ctx, `DELETE FROM `+pgSchema.Table("schema_migrations")+` WHERE version = $1`, teamMembersMigrationVersion); err != nil {
		t.Fatal(err)
	}
	reopen()
	storetest.RequireTeamRows(t, pgSchema.DB, query, "migrated", storetest.MemberTeamRows)
	storetest.RequireMemberTeamsLoad(t, store)
	// A second open finds the migration applied; one that lost only the
	// ledger row runs it again and changes nothing.
	reopen()
	if _, err := pgSchema.DB.ExecContext(ctx, `DELETE FROM `+pgSchema.Table("schema_migrations")+` WHERE version = $1`, teamMembersMigrationVersion); err != nil {
		t.Fatal(err)
	}
	reopen()
	storetest.RequireTeamRows(t, pgSchema.DB, query, "migrated again", storetest.MemberTeamRows)

	for _, stage := range []string{"rolled back", "rolled back again"} {
		if err := store.RollbackPostgresMigrations(ctx, teamMembersMigrationVersion-1); err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		storetest.RequireTeamRows(t, pgSchema.DB, query, stage, storetest.RolledBackTeamRows)
		if version, err := store.PostgresSchemaVersion(ctx); err != nil || version != teamMembersMigrationVersion-1 {
			t.Fatalf("%s: schema version = %d, %v", stage, version, err)
		}
	}
	reopen()
	storetest.RequireMemberTeamsLoad(t, store)
	// After a rollback past the Team tables, this rollback finds them gone.
	if err := store.RollbackPostgresMigrations(ctx, 10); err != nil {
		t.Fatal(err)
	}
	tx, err := pgSchema.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := store.rollbackTeamMembers(ctx, tx); err != nil {
		t.Fatal(err)
	}
}
