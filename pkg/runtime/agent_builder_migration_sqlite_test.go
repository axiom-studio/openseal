package runtime

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// SQLite store test (schema migrations): it moves to store/sqlite in U2.9,
// with its fixture from agent_builder_migration_fixture_test.go (storetest).
// It seeds its Run through NewPortfolioService, which a store package cannot
// import: the move seeds it with the store's CreateAgentRun instead.

// seedSQLiteChangeSets turns a current SQLite file back into a v75 one: the
// change-set table and indexes, a running workforce_authoring Run and
// deployments carrying an activation continuation.
func seedSQLiteChangeSets(t *testing.T, path string) string {
	t.Helper()
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := openTestSQLiteDB(t, path)
	defer store.Close()
	ctx := context.Background()
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: builderCutoverScope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "author", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	statements := []string{
		`CREATE TABLE workforce_change_sets (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			parent_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, revision INTEGER NOT NULL,
			idempotency_key TEXT NOT NULL, request_digest TEXT NOT NULL, candidate_digest TEXT NOT NULL,
			created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id),
			UNIQUE (scope_kind, scope_id, idempotency_key))`,
		`CREATE INDEX idx_workforce_change_sets_parent ON workforce_change_sets(scope_kind, scope_id, parent_id, created_at)`,
		`CREATE INDEX idx_workforce_change_sets_status ON workforce_change_sets(scope_kind, scope_id, status, updated_at)`,
		`CREATE INDEX idx_workforce_change_sets_global_recovery ON workforce_change_sets(status, scope_kind, scope_id)`,
		`CREATE INDEX idx_agent_runs_global_recovery ON agent_runs(status, available_at, lease_expires_at, scope_kind, scope_id)
			WHERE COALESCE(json_extract(payload, '$.kind'), 'agent_work') = 'workforce_authoring'`,
	}
	for _, statement := range statements {
		if _, err := sqlDB.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for index, row := range cutoverLineageRows {
		if _, err := sqlDB.Exec(`INSERT INTO workforce_change_sets (scope_kind, scope_id, id, parent_id, status, revision, idempotency_key, request_digest, candidate_digest, created_at, updated_at, payload)
			VALUES (?,?,?,?,?,1,?,'digest','digest',?,?,?)`, builderCutoverScope.Kind, builderCutoverScope.ID, row.id, row.parentID, row.status,
			"key-"+row.id, now.Add(time.Duration(index)*time.Second), now, row.payload(t, builderCutoverScope)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := sqlDB.Exec(`UPDATE agent_runs SET status = 'running', lease_owner = 'authoring-worker',
		payload = json_set(payload, '$.kind', 'workforce_authoring', '$.status', 'running', '$.leaseOwner', 'authoring-worker') WHERE id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"agent_deployments", "team_deployments"} {
		if _, err := sqlDB.Exec(`INSERT INTO `+table+` (scope_kind, scope_id, id, definition_id, active_version, revision, updated_at, payload)
			VALUES (?,?,?,'definition-1','1',1,?,?)`, builderCutoverScope.Kind, builderCutoverScope.ID, "paused-"+table, now,
			`{"id":"paused-`+table+`","rolloutStatus":"paused","status":"paused","activation":{"changeSetId":"cs-applied"}}`); err != nil {
			t.Fatal(err)
		}
	}
	return run.ID
}

func sqliteObjectNames(t *testing.T, db *sql.DB, kind, prefix string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = ? AND name LIKE ? ORDER BY name`, kind, prefix+"%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return names
}

func TestAgentBuilderMigrationSQLiteKeepsLeafDraftsAndRetiresChangeSets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	runID := seedSQLiteChangeSets(t, path)
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := openTestSQLiteDB(t, path)
	defer store.Close()
	ctx := context.Background()

	if names := sqliteObjectNames(t, sqlDB, "table", "workforce_change_sets"); len(names) != 0 {
		t.Fatalf("change-set table survived: %v", names)
	}
	if names := sqliteObjectNames(t, sqlDB, "index", "idx_workforce_change_sets"); len(names) != 0 {
		t.Fatalf("change-set indexes survived: %v", names)
	}
	if names := sqliteObjectNames(t, sqlDB, "index", "idx_agent_runs_global_recovery"); len(names) != 0 {
		t.Fatalf("authoring recovery index survived: %v", names)
	}
	drafts, err := store.ListAgentBuilderCutoverDrafts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertCutoverLeafDrafts(t, drafts)

	run, err := store.GetAgentRun(ctx, builderCutoverScope, runID)
	if err != nil || run.Status != AgentRunStatusCanceled || run.LeaseOwner != "" {
		t.Fatalf("authoring run = %#v, %v; want canceled without lease", run, err)
	}
	var status, leaseOwner string
	if err := sqlDB.QueryRow(`SELECT status, lease_owner FROM agent_runs WHERE id = ?`, runID).Scan(&status, &leaseOwner); err != nil || status != "canceled" || leaseOwner != "" {
		t.Fatalf("authoring run columns = %q %q, %v", status, leaseOwner, err)
	}
	for _, table := range []string{"agent_deployments", "team_deployments"} {
		var activation sql.NullString
		if err := sqlDB.QueryRow(`SELECT json_type(payload, '$.activation') FROM `+table+` WHERE id = ?`, "paused-"+table).Scan(&activation); err != nil || activation.Valid {
			t.Fatalf("%s activation = %v, %v; want removed", table, activation, err)
		}
	}

	// A later open changes nothing.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := store.ListAgentBuilderCutoverDrafts(ctx); err != nil || !reflect.DeepEqual(again, drafts) {
		t.Fatalf("drafts after reopen = %#v, %v", again, err)
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
	if err := store.DeleteAgentBuilderCutoverDraft(ctx, builderCutoverScope, "cs-amend"); err != nil {
		t.Fatalf("delete after drop = %v", err)
	}
}

func TestAgentBuilderMigrationSQLiteFreshStoreHasNoChangeSets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := openTestSQLiteDB(t, path)
	defer store.Close()
	for _, prefix := range []string{"workforce_change_sets", agentBuilderCutoverTable} {
		if names := sqliteObjectNames(t, sqlDB, "table", prefix); len(names) != 0 {
			t.Fatalf("fresh store has %v", names)
		}
	}
	if drafts, err := store.ListAgentBuilderCutoverDrafts(context.Background()); err != nil || len(drafts) != 0 {
		t.Fatalf("fresh drafts = %#v, %v", drafts, err)
	}
	if dropped, err := store.DropAgentBuilderCutoverTable(context.Background()); err != nil || dropped {
		t.Fatalf("fresh drop = %v, %v", dropped, err)
	}
}
