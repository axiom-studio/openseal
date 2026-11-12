package runtime

import (
	"context"
	"database/sql"
	"testing"
)

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
