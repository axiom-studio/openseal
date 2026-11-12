package runtime

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

func TestSQLiteRoutineEventStore(t *testing.T) {
	storetest.RunRoutineEventContract(t, func(t *testing.T) (models.KernelStore, storetest.RoutineEventPayloadProbe) {
		path := filepath.Join(t.TempDir(), "routine-events.db")
		store, err := sqlite.New(path)
		if err != nil {
			t.Fatal(err)
		}
		sqlDB := openTestSQLiteDB(t, path)
		t.Cleanup(func() { _ = store.Close() })
		return store, func(t *testing.T, scope models.Scope, activationID, eventID string) bool {
			var hasPayload bool
			if err := sqlDB.QueryRow(`SELECT payload IS NOT NULL FROM routine_events WHERE scope_kind = ? AND scope_id = ? AND activation_id = ? AND event_id = ?`,
				scope.Kind, scope.ID, activationID, eventID).Scan(&hasPayload); err != nil {
				t.Fatalf("read event %s: %v", eventID, err)
			}
			return hasPayload
		}
	})
}

// A SQLite store opened before migration 80 gains the index columns, and an
// event Task written before them is filled in.
func TestSQLiteRoutineEventsMigrationFillsIndexColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB := openTestSQLiteDB(t, path)
	scope := models.Scope{Kind: "tenant", ID: "migrate"}
	activation := storetest.RoutineEventActivation(scope, "activation-old", time.Now().UTC())
	if err := store.CreateRunbookActivation(t.Context(), activation); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`UPDATE runbook_activations SET event_kind = NULL, event_source_ref = NULL`); err != nil {
		t.Fatal(err)
	}
	_ = store.Close()
	reopened, err := sqlite.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	keys, err := reopened.ListRoutineEventSourceKeysInScope(t.Context(), scope)
	if err != nil || len(keys) != 1 || keys[0].SourceRef != activation.Task.Event.SourceRef {
		t.Fatalf("keys after reopen = %#v, %v", keys, err)
	}
}
