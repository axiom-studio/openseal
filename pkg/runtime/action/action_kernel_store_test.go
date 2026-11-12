package action

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

// newActionTestStore returns an empty in-memory kernel store that is closed
// when the test ends. It copies runtime's newTestKernelStore for the action
// tests that need action internals, which live with the action code and
// cannot see runtime's test helpers. Change the copies together.
func newActionTestStore(t testing.TB) *sqlite.Store {
	t.Helper()
	store, err := sqlite.NewInMemorySQLiteStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
