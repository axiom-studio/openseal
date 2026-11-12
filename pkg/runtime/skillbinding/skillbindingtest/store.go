package skillbindingtest

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

// NewStore returns an empty in-memory kernel store that is closed when the
// test ends. It is a copy of runtime's newTestKernelStore (runtime's own tests
// cannot import this package without a cycle); change both.
func NewStore(t testing.TB) *sqlite.Store {
	t.Helper()
	store, err := sqlite.NewInMemorySQLiteStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
