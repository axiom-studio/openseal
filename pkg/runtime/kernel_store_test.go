package runtime

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

// getStoredRun reads a Run that the test knows exists.
func getStoredRun(t testing.TB, store *sqlite.Store, scope models.Scope, id string) *models.AgentRun {
	t.Helper()
	run, err := store.GetAgentRun(context.Background(), scope, id)
	if err != nil {
		t.Fatalf("get run %s: %v", id, err)
	}
	return run
}
