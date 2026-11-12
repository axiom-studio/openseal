package action

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

// newActionTestStore returns an empty in-memory kernel store that is closed
// when the test ends. It copies runtime's newTestKernelStore for the
// in-package action tests, which cannot see runtime's test helpers or import
// actiontest (it imports action). Change the copies together.
func newActionTestStore(t testing.TB) *sqlite.Store {
	t.Helper()
	store, err := sqlite.NewInMemorySQLiteStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
