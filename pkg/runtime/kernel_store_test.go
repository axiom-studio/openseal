package runtime

import (
	"testing"
)

// newTestKernelStore returns an empty in-memory kernel store that is closed
// when the test ends. runtimetest.NewKernelStore is its copy for other
// packages (importing runtimetest here would be a cycle); change both.
func newTestKernelStore(t testing.TB) *SQLiteStore {
	t.Helper()
	store, err := NewInMemorySQLiteStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
