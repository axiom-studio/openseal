//go:build integration

package runtime

import (
	"context"
	"sort"
	"strings"
	"testing"
)

func newAgentSessionMigrationStore(t *testing.T, prefix string) (*PostgresStore, string, string) {
	t.Helper()
	pgSchema := newTestPostgresSchema(t, prefix)
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
	return store, dsn, schema
}

func agentSessionLogTablesPresent(t *testing.T, store *PostgresStore) map[string]bool {
	t.Helper()
	present := map[string]bool{}
	for _, table := range []string{"agent_sessions", "agent_session_entries"} {
		var relation *string
		if err := store.db.QueryRowContext(context.Background(), `SELECT to_regclass($1)::text`, store.schema+"."+table).Scan(&relation); err != nil {
			t.Fatal(err)
		}
		present[table] = relation != nil
	}
	return present
}

func agentSessionLogIndexes(t *testing.T, store *PostgresStore, table string) []string {
	t.Helper()
	rows, err := store.db.QueryContext(context.Background(), `SELECT indexname FROM pg_indexes WHERE schemaname = $1 AND tablename = $2 AND indexname NOT LIKE '%_pkey' AND indexname NOT LIKE '%_key'`, store.schema, table)
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	return names
}

func TestPostgresAgentSessionLogMigration(t *testing.T) {
	store, _, _ := newAgentSessionMigrationStore(t, "agent_session_log_")
	ctx := context.Background()
	if version, err := store.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("schema head = %d, %v; want %d", version, err, currentPostgresSchemaVersion)
	}
	current, _, err := store.postgresSchemaCurrent(ctx, store.db)
	if err != nil || !current {
		t.Fatalf("schema current = %v, %v", current, err)
	}
	for table, ok := range agentSessionLogTablesPresent(t, store) {
		if !ok {
			t.Fatalf("table %s missing", table)
		}
	}
	if got, want := strings.Join(agentSessionLogIndexes(t, store, "agent_sessions"), ","), "agent_sessions_lane_idx,agent_sessions_owner_run_idx,agent_sessions_parent_idx"; got != want {
		t.Fatalf("agent_sessions indexes = %s; want %s", got, want)
	}
	if got, want := strings.Join(agentSessionLogIndexes(t, store, "agent_session_entries"), ","), "agent_session_entries_action_idx,agent_session_entries_compaction_idx,agent_session_entries_open_tool_idx,agent_session_entries_run_idx,agent_session_entries_source_idx"; got != want {
		t.Fatalf("agent_session_entries indexes = %s; want %s", got, want)
	}
	var writerColumns int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = $1 AND table_name = 'agent_sessions' AND column_name = 'writer_run_id'`, store.schema).Scan(&writerColumns); err != nil || writerColumns != 0 {
		t.Fatalf("writer_run_id columns = %d, %v; want none", writerColumns, err)
	}

	// The constraints guard the row shape the stores rely on.
	sessions, entries := store.table("agent_sessions"), store.table("agent_session_entries")
	insertSession := `INSERT INTO ` + sessions + ` (scope_kind, scope_id, id, kind, lane_key, conversation_id, agent_id, owner_run_id, parent_session_id, root_session_id, depth, revision, created_at, updated_at, payload)
		VALUES ('workspace','w1',$1,$2,$3,'c1','agent-1',$4,$5,$6,$7,1,now(),now(),'{}')`
	if _, err := store.db.ExecContext(ctx, insertSession, "s1", "conversation", "lane-1", "", "", "s1", 0); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]any{
		"conversation without lane": {"s2", "conversation", "", "", "", "s2", 0},
		"run without owner":         {"s3", "run", "", "", "", "s3", 0},
		"fork without depth":        {"s4", "run", "", "run-4", "s1", "s1", 0},
		"duplicate lane":            {"s5", "conversation", "lane-1", "", "", "s5", 0},
	} {
		if _, err := store.db.ExecContext(ctx, insertSession, args...); err == nil {
			t.Fatalf("%s: insert succeeded", name)
		}
	}
	insertEntry := `INSERT INTO ` + entries + ` (scope_kind, scope_id, session_id, sequence, id, run_id, type, state, tool_call_id, revision, created_at, updated_at, payload)
		VALUES ('workspace','w1',$1,$2,$3,'run-1',$4,$5,$6,1,now(),now(),'{}')`
	if _, err := store.db.ExecContext(ctx, insertEntry, "s1", 1, "e1", "tool", "running", "call-1"); err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]any{
		"tool without state":   {"s1", 2, "e2", "tool", "", "call-2"},
		"note with state":      {"s1", 3, "e3", "note", "running", ""},
		"unknown state":        {"s1", 4, "e4", "tool", "paused", "call-4"},
		"duplicate sequence":   {"s1", 1, "e5", "note", "", ""},
		"missing session":      {"nope", 1, "e6", "note", "", ""},
		"duplicate entry id":   {"s1", 6, "e1", "note", "", ""},
		"unknown entry type":   {"s1", 7, "e7", "reasoning", "", ""},
		"zero sequence number": {"s1", 0, "e8", "note", "", ""},
	} {
		if _, err := store.db.ExecContext(ctx, insertEntry, args...); err == nil {
			t.Fatalf("%s: insert succeeded", name)
		}
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM `+sessions+` WHERE id = 's1'`); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+entries).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("entries after session delete = %d, %v; want cascade", remaining, err)
	}
}

func TestPostgresAgentSessionLogRollback(t *testing.T) {
	store, dsn, schema := newAgentSessionMigrationStore(t, "agent_session_rollback_")
	ctx := context.Background()
	countTables := func() int {
		t.Helper()
		var tables int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = $1`, store.schema).Scan(&tables); err != nil {
			t.Fatal(err)
		}
		return tables
	}
	before := countTables()
	if err := store.RollbackPostgresMigrations(ctx, 74); err != nil {
		t.Fatal(err)
	}
	for table, ok := range agentSessionLogTablesPresent(t, store) {
		if ok {
			t.Fatalf("table %s survived rollback to 74", table)
		}
	}
	if after, want := countTables(), before-4-len(routineEventsTables); after != want {
		t.Fatalf("tables after rollback = %d; want %d (only the two session tables, space_people, the working files and the routine event tables dropped)", after, want)
	}
	if version, err := store.PostgresSchemaVersion(ctx); err != nil || version != 74 {
		t.Fatalf("schema head after rollback = %d, %v; want 74", version, err)
	}
	var connectOnce int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+store.table("schema_migrations")+` WHERE version = $1`, connectOnceMigrationVersion).Scan(&connectOnce); err != nil || connectOnce != 1 {
		t.Fatalf("connect-once ledger rows = %d, %v; want 1", connectOnce, err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	store = reopened
	t.Cleanup(func() {
		_, _ = reopened.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+reopened.quotedSchema()+` CASCADE`)
		_ = reopened.Close()
	})
	for table, ok := range agentSessionLogTablesPresent(t, reopened) {
		if !ok {
			t.Fatalf("table %s not recreated on reopen", table)
		}
	}
	if version, err := reopened.PostgresSchemaVersion(ctx); err != nil || version != currentPostgresSchemaVersion {
		t.Fatalf("schema head after reopen = %d, %v; want %d", version, err, currentPostgresSchemaVersion)
	}
}
