package sqlite_test

import (
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// Store test: the SQLite store keeps a Run's ActingUser fixed. The Postgres
// half of this test is in store/postgres.

// TestAgentRunActingUserIsImmutable runs the acting-user contract on the
// SQLite store.
func TestAgentRunActingUserIsImmutable(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		store, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "acting-user.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		storetest.RunActingUserContract(t, store)
	})
}
