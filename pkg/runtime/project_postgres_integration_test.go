package runtime

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestPostgresProjectRestartAndAtomicActivity(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	schema := "project_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	var migrationName string
	if err = store.db.QueryRowContext(ctx, `SELECT name FROM `+store.table("schema_migrations")+` WHERE version=14`).Scan(&migrationName); err != nil || migrationName != "durable projects" {
		t.Fatalf("project migration=%q err=%v", migrationName, err)
	}
	if err = store.db.QueryRowContext(ctx, `SELECT name FROM `+store.table("schema_migrations")+` WHERE version=23`).Scan(&migrationName); err != nil || migrationName != "indexed project activity projections" {
		t.Fatalf("project activity migration=%q err=%v", migrationName, err)
	}
	if err = store.db.QueryRowContext(ctx, `SELECT name FROM `+store.table("schema_migrations")+` WHERE version=$1`, projectRunRefsMigrationVersion).Scan(&migrationName); err != nil || migrationName != "derive Project Runs from Objective lineage" {
		t.Fatalf("Project lineage migration=%q err=%v", migrationName, err)
	}
	scope := Scope{Kind: "tenant", ID: "restart"}
	seedProjectObjectives(t, store, scope)
	svc := NewProjectService(store, store)
	created, event, err := svc.Create(ctx, CreateProjectRequest{Project: projectFixture(scope), IdempotencyKey: "restart-key", Actor: ActivityActor{Type: "user", ID: "u1"}})
	if err != nil || event == nil {
		t.Fatalf("create event=%#v err=%v", event, err)
	}
	if _, err = store.db.ExecContext(ctx, `UPDATE `+store.table("projects")+` SET payload=jsonb_set(payload,'{runRefs}','["legacy-run"]'::jsonb) WHERE id=$1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.ExecContext(ctx, `DELETE FROM `+store.table("schema_migrations")+` WHERE version=$1`, projectRunRefsMigrationVersion); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	got, err := store.GetProject(ctx, scope, created.ID)
	if err != nil || got.Revision != 1 {
		t.Fatalf("restart project=%#v err=%v", got, err)
	}
	var migratedPayload string
	if err = store.db.QueryRowContext(ctx, `SELECT payload::text FROM `+store.table("projects")+` WHERE id=$1`, created.ID).Scan(&migratedPayload); err != nil || strings.Contains(migratedPayload, "runRefs") {
		t.Fatalf("Project lineage payload=%s err=%v", migratedPayload, err)
	}
	owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-a"}
	listed, err := store.ListProjects(ctx, ProjectFilter{Scope: scope, Owner: &owner, Statuses: []ProjectStatus{ProjectStatusDraft}, ObjectiveID: "objective-a", Limit: 1})
	if err != nil || len(listed) != 1 || listed[0].ID != created.ID {
		t.Fatalf("filtered list=%#v err=%v", listed, err)
	}
	feed, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, ProjectID: created.ID, Descending: true, Limit: 10})
	if err != nil || len(feed) != 1 || feed[0].ProjectID != created.ID {
		t.Fatalf("restart activity=%#v err=%v", feed, err)
	}
	got.Status = ProjectStatusActive
	got.Revision = 1
	updated, _, err := NewProjectService(store, store).Update(ctx, got, 1, ActivityActor{Type: "user", ID: "u1"}, ActivityVisibilityScope)
	if err != nil || updated.Revision != 2 {
		t.Fatalf("update=%#v err=%v", updated, err)
	}
}

func TestPostgresRepairsActivityProjectionWhenLedgerIsAheadOfSchema(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	schema := "activity_repair_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.ExecContext(ctx, `
		DROP INDEX IF EXISTS `+store.table("run_activity_project_feed_idx")+`;
		ALTER TABLE `+store.table("run_activity")+` DROP COLUMN project_id;
		DELETE FROM `+store.table("schema_migrations")+` WHERE version=$1
	`, activityProjectionRepairMigrationVersion); err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.Close()

	store, err = NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var columnExists bool
	if err = store.db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM information_schema.columns
		WHERE table_schema=$1 AND table_name='run_activity' AND column_name='project_id'
	)`, schema).Scan(&columnExists); err != nil || !columnExists {
		t.Fatalf("project_id exists=%v err=%v", columnExists, err)
	}
	var migrationName string
	if err = store.db.QueryRowContext(ctx, `SELECT name FROM `+store.table("schema_migrations")+` WHERE version=$1`, activityProjectionRepairMigrationVersion).Scan(&migrationName); err != nil || migrationName != "reconcile indexed activity projections" {
		t.Fatalf("activity repair migration=%q err=%v", migrationName, err)
	}
}
