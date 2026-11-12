package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// TestSQLiteAgentSessionStore runs the session store conformance suite
// (storetest.RunAgentSessionContract) against the SQLite backend.
func TestSQLiteAgentSessionStore(t *testing.T) {
	storetest.RunAgentSessionContract(t, func(t *testing.T) (storetest.AgentSessionContractStore, storetest.CreateRunSessionInOwnTx) {
		store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "sessions.db"))
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store, func(ctx context.Context, req models.CreateRunAgentSessionTxRequest) (*models.OpenAgentSessionResult, error) {
			return sqlite.CreateRunSessionInOwnTx(ctx, store, req)
		}
	})
}
