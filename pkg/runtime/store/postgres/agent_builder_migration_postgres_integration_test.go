//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// Store test (schema migrations), with the cutover fixture of storetest.

// seedPostgresChangeSets turns a current PostgreSQL schema back into a v75
// one: the change-set table, its indexes and outbox triggers, the agent_runs
// outbox functions with the authoring CASE, a running workforce_authoring
// Run, deployments with an activation continuation and the ledger rows of
// the retired migrations.
func seedPostgresChangeSets(t *testing.T, store *PostgresStore) string {
	t.Helper()
	ctx := context.Background()
	run := storetest.QueuedAgentRun(storetest.BuilderCutoverScope, "agent", "author", time.Now())
	run.AssignedAgentID, run.Source = "", models.RunSourceManual
	if err := store.CreateAgentRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	table := store.table(sqlstore.RetiredChangeSetTable)
	statements := []string{
		`CREATE TABLE ` + table + ` (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			parent_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, revision BIGINT NOT NULL,
			idempotency_key TEXT NOT NULL, request_digest TEXT NOT NULL, candidate_digest TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL, payload JSONB NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id),
			UNIQUE (scope_kind, scope_id, idempotency_key))`,
		`CREATE INDEX workforce_change_sets_parent_idx ON ` + table + ` (scope_kind, scope_id, parent_id, created_at)`,
		`CREATE INDEX workforce_change_sets_status_idx ON ` + table + ` (scope_kind, scope_id, status, updated_at)`,
		`CREATE INDEX workforce_change_sets_global_recovery_idx ON ` + table + ` (status, scope_kind, scope_id)`,
		`CREATE INDEX workforce_change_sets_pending_work_idx ON ` + table + ` (scope_kind,scope_id) WHERE status IN ('evaluating','review')`,
		`CREATE INDEX agent_runs_global_recovery_idx ON ` + store.table("agent_runs") + ` (status, available_at, lease_expires_at, scope_kind, scope_id)
			WHERE COALESCE(payload->>'kind', 'agent_work') = 'workforce_authoring'`,
	}
	for _, name := range []string{"work_outbox_insert", "work_outbox_update"} {
		event, when := "INSERT", ""
		if name == "work_outbox_update" {
			event, when = "UPDATE", statusChanged
		}
		statements = append(statements, store.workOutboxTriggerStatements(workOutboxTrigger{
			table: sqlstore.RetiredChangeSetTable, name: name, event: event, when: when, rows: []string{`('authoring', NULL, false)`},
		})...)
	}
	runFamily := func(family string) string {
		return `CASE WHEN NEW.payload->>'kind' = 'workforce_authoring' THEN 'authoring' ELSE '` + family + `' END`
	}
	for _, trigger := range workOutboxTriggers() {
		if trigger.table != "agent_runs" || trigger.name == "work_outbox_unpause" {
			continue
		}
		trigger.rows = []string{
			`(` + runFamily("run") + `, CASE WHEN NEW.status = 'running' THEN NULL ELSE NEW.available_at END, false)`,
			`(` + runFamily("run-due") + `, CASE WHEN NEW.status = 'running' THEN NEW.lease_expires_at END, true)`,
		}
		statements = append(statements, store.workOutboxTriggerStatements(trigger)...)
	}
	for _, statement := range statements {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	for index, row := range storetest.CutoverLineageRows {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO `+table+` (scope_kind, scope_id, id, parent_id, status, revision, idempotency_key, request_digest, candidate_digest, created_at, updated_at, payload)
			VALUES ($1,$2,$3,$4,$5,1,$6,'digest','digest',$7,$8,$9::jsonb)`, storetest.BuilderCutoverScope.Kind, storetest.BuilderCutoverScope.ID, row.ID, row.ParentID, row.Status,
			"key-"+row.ID, now.Add(time.Duration(index)*time.Second), now, row.Payload(t, storetest.BuilderCutoverScope)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE `+store.table("agent_runs")+` SET status = 'running', lease_owner = 'authoring-worker', lease_expires_at = now() + interval '1 minute',
		payload = payload || '{"kind":"workforce_authoring","status":"running","leaseOwner":"authoring-worker"}'::jsonb WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	for _, deployments := range []string{"agent_deployments", "team_deployments"} {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO `+store.table(deployments)+` (scope_kind, scope_id, id, definition_id, active_version, revision, updated_at, payload)
			VALUES ($1,$2,$3,'definition-1','1',1,$4,$5::jsonb)`, storetest.BuilderCutoverScope.Kind, storetest.BuilderCutoverScope.ID, "paused-"+deployments, now,
			`{"id":"paused-`+deployments+`","rolloutStatus":"paused","status":"paused","activation":{"changeSetId":"cs-applied"}}`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM `+store.table("schema_migrations")+` WHERE version = $1`, agentBuilderMigrationVersion); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int64{13, 29, 30, 31} {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO `+store.table("schema_migrations")+` (version, name) VALUES ($1, 'retired')`, version); err != nil {
			t.Fatal(err)
		}
	}
	return run.ID
}

func postgresRelationPresent(t *testing.T, store *PostgresStore, name string) bool {
	t.Helper()
	var relation sql.NullString
	if err := store.db.QueryRowContext(context.Background(), `SELECT to_regclass($1)::text`, store.schema+"."+name).Scan(&relation); err != nil {
		t.Fatal(err)
	}
	return relation.Valid
}

func postgresCount(t *testing.T, store *PostgresStore, query string, args ...interface{}) int {
	t.Helper()
	var count int
	if err := store.db.QueryRowContext(context.Background(), query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func assertPostgresLedgerCurrent(t *testing.T, store *PostgresStore) {
	t.Helper()
	current, version, err := store.postgresSchemaCurrent(context.Background(), store.db)
	if err != nil || !current || version != currentPostgresSchemaVersion {
		t.Fatalf("schema current = %v, version %d, %v; want current at %d", current, version, err, currentPostgresSchemaVersion)
	}
}

func TestPostgresAgentBuilderMigrationKeepsLeafDraftsAndRetiresChangeSets(t *testing.T) {
	store, _, _ := newAgentSessionMigrationStore(t, "agent_builder_migration_")
	ctx := context.Background()
	runID := seedPostgresChangeSets(t, store)
	if current, _, err := store.postgresSchemaCurrent(ctx, store.db); err != nil || current {
		t.Fatalf("seeded v75 schema current = %v, %v; want behind", current, err)
	}
	if err := store.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	assertPostgresLedgerCurrent(t, store)

	if postgresRelationPresent(t, store, sqlstore.RetiredChangeSetTable) || !postgresRelationPresent(t, store, sqlstore.AgentBuilderCutoverTable) {
		t.Fatal("change-set table was not renamed to the cutover table")
	}
	for _, index := range append([]string{"agent_runs_global_recovery_idx"}, retiredChangeSetIndexes...) {
		if postgresRelationPresent(t, store, index) {
			t.Fatalf("index %s survived", index)
		}
	}
	if triggers := postgresCount(t, store, `SELECT COUNT(*) FROM pg_trigger WHERE tgrelid = to_regclass($1) AND NOT tgisinternal`, store.schema+"."+sqlstore.AgentBuilderCutoverTable); triggers != 0 {
		t.Fatalf("cutover table keeps %d outbox triggers", triggers)
	}
	if functions := postgresCount(t, store, `SELECT COUNT(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = $1 AND p.proname LIKE 'work_outbox_workforce_change_sets_%'`, store.schema); functions != 0 {
		t.Fatalf("%d change-set outbox functions survived", functions)
	}
	if authoring := postgresCount(t, store, `SELECT COUNT(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = $1 AND p.proname LIKE 'work_outbox_agent_runs_%' AND p.prosrc LIKE '%workforce_authoring%'`, store.schema); authoring != 0 {
		t.Fatalf("%d agent_runs outbox functions still route authoring Runs", authoring)
	}
	if bodies := postgresCount(t, store, `SELECT COUNT(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = $1 AND p.proname IN ('work_outbox_agent_runs_insert', 'work_outbox_agent_runs_update')`, store.schema); bodies != 2 {
		t.Fatalf("agent_runs outbox functions = %d; want insert and update", bodies)
	}

	drafts, err := store.ListAgentBuilderCutoverDrafts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	storetest.AssertCutoverLeafDrafts(t, drafts)

	run, err := store.GetAgentRun(ctx, storetest.BuilderCutoverScope, runID)
	if err != nil || run.Status != models.AgentRunStatusCanceled || run.LeaseOwner != "" || run.LeaseExpiresAt != nil {
		t.Fatalf("authoring run = %#v, %v; want canceled without lease", run, err)
	}
	if status := postgresCount(t, store, `SELECT COUNT(*) FROM `+store.table("agent_runs")+` WHERE id = $1 AND status = 'canceled' AND lease_owner = '' AND lease_expires_at IS NULL`, runID); status != 1 {
		t.Fatal("authoring run columns were not canceled")
	}
	for _, deployments := range []string{"agent_deployments", "team_deployments"} {
		if left := postgresCount(t, store, `SELECT COUNT(*) FROM `+store.table(deployments)+` WHERE payload ? 'activation'`); left != 0 {
			t.Fatalf("%s keeps %d activations", deployments, left)
		}
	}

	// A second migrate is a no-op.
	if err := store.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if again, err := store.ListAgentBuilderCutoverDrafts(ctx); err != nil || len(again) != 2 {
		t.Fatalf("drafts after re-migrate = %d, %v", len(again), err)
	}

	if dropped, err := store.DropAgentBuilderCutoverTable(ctx); err != nil || dropped {
		t.Fatalf("drop with drafts left = %v, %v; want kept", dropped, err)
	}
	for _, draft := range drafts {
		if err := store.DeleteAgentBuilderCutoverDraft(ctx, draft.Scope, draft.ID); err != nil {
			t.Fatal(err)
		}
	}
	if dropped, err := store.DropAgentBuilderCutoverTable(ctx); err != nil || !dropped {
		t.Fatalf("drop of empty table = %v, %v; want dropped", dropped, err)
	}
	if dropped, err := store.DropAgentBuilderCutoverTable(ctx); err != nil || dropped {
		t.Fatalf("second drop = %v, %v; want nothing to drop", dropped, err)
	}
	if left, err := store.ListAgentBuilderCutoverDrafts(ctx); err != nil || len(left) != 0 {
		t.Fatalf("drafts after drop = %#v, %v", left, err)
	}
	if err := store.DeleteAgentBuilderCutoverDraft(ctx, storetest.BuilderCutoverScope, "cs-amend"); err != nil {
		t.Fatalf("delete after drop = %v", err)
	}
	assertPostgresLedgerCurrent(t, store)
}

func TestPostgresAgentBuilderMigrationFreshSchemaNeverHasChangeSets(t *testing.T) {
	store, _, _ := newAgentSessionMigrationStore(t, "agent_builder_fresh_")
	ctx := context.Background()
	assertPostgresLedgerCurrent(t, store)
	for _, name := range []string{sqlstore.RetiredChangeSetTable, sqlstore.AgentBuilderCutoverTable, "agent_runs_global_recovery_idx"} {
		if postgresRelationPresent(t, store, name) {
			t.Fatalf("fresh schema has %s", name)
		}
	}
	if retired := postgresCount(t, store, `SELECT COUNT(*) FROM `+store.table("schema_migrations")+` WHERE version IN (13, 29, 30, 31, 34)`); retired != 0 {
		t.Fatalf("fresh ledger records %d retired migrations", retired)
	}
	if functions := postgresCount(t, store, `SELECT COUNT(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = $1 AND (p.proname LIKE '%workforce_change_sets%' OR (p.proname LIKE 'work_outbox_%' AND p.prosrc LIKE '%authoring%'))`, store.schema); functions != 0 {
		t.Fatalf("fresh schema has %d authoring outbox functions", functions)
	}
	if drafts, err := store.ListAgentBuilderCutoverDrafts(ctx); err != nil || len(drafts) != 0 {
		t.Fatalf("fresh drafts = %#v, %v", drafts, err)
	}
	if dropped, err := store.DropAgentBuilderCutoverTable(ctx); err != nil || dropped {
		t.Fatalf("fresh drop = %v, %v", dropped, err)
	}
	if err := store.RollbackPostgresMigrations(ctx, agentBuilderMigrationVersion-1); err != nil {
		t.Fatal(err)
	}
	if version, err := store.PostgresSchemaVersion(ctx); err != nil || version != agentBuilderMigrationVersion-1 {
		t.Fatalf("rolled back version = %d, %v", version, err)
	}
	if err := store.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	assertPostgresLedgerCurrent(t, store)
}
