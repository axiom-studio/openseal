package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
)

// The raw-SQL helpers work on a connection the test owns (newTestSQLiteStore,
// testPostgresSchema.db), never on a store's own connection. They are for the
// rows no store method reads or writes: a tampered payload, a row count, a
// projection the store keeps to itself.

// execSQL runs a statement on db.
func execSQL(t testing.TB, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// countRows counts the rows of table; table is used as given, so a
// Postgres test passes testPostgresSchema.table(name).
func countRows(t testing.TB, db *sql.DB, table string) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*) FROM `+table).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// rewriteRowPayload decodes the JSON payload of one scoped row of a SQLite
// table, applies edit and writes it back. It bypasses every store rule, so a
// test can model a tampered or inconsistent row that no store method would
// produce. Indexed columns are not touched; edit only payload-only fields
// with it.
func rewriteRowPayload[T any](t testing.TB, db *sql.DB, table string, scope Scope, id string, edit func(*T)) {
	t.Helper()
	ctx := context.Background()
	var payload []byte
	if err := db.QueryRowContext(ctx, `SELECT payload FROM `+table+` WHERE scope_kind = ? AND scope_id = ? AND id = ?`, scope.Kind, scope.ID, id).Scan(&payload); err != nil {
		t.Fatalf("read %s %s: %v", table, id, err)
	}
	var value T
	if err := json.Unmarshal(payload, &value); err != nil {
		t.Fatalf("decode %s %s: %v", table, id, err)
	}
	edit(&value)
	encoded, err := json.Marshal(&value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE `+table+` SET payload = ? WHERE scope_kind = ? AND scope_id = ? AND id = ?`, string(encoded), scope.Kind, scope.ID, id); err != nil {
		t.Fatalf("write %s %s: %v", table, id, err)
	}
}

// storedRoutineEvent is one row of routine_events. KeepsData reports whether
// the row still holds the event's payload, summary and fields.
type storedRoutineEvent struct {
	ActivationID, EventID string
	State                 RoutineEventState
	DroppedReason         string
	KeepsData             bool
}

// listRoutineEventRows reads the routine_events rows of scope from a SQLite
// database.
func listRoutineEventRows(t testing.TB, db *sql.DB, scope Scope) []storedRoutineEvent {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `SELECT activation_id, event_id, state, COALESCE(dropped_reason, ''), payload IS NOT NULL
		FROM routine_events WHERE scope_kind = ? AND scope_id = ? ORDER BY activation_id, event_id`, scope.Kind, scope.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var events []storedRoutineEvent
	for rows.Next() {
		var event storedRoutineEvent
		if err := rows.Scan(&event.ActivationID, &event.EventID, &event.State, &event.DroppedReason, &event.KeepsData); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}
