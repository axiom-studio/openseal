// Package runtimetest provides kernel stores for the tests of packages built
// on runtime. Tests inside package runtime use their own newTestKernelStore,
// since importing this package from there would be a cycle.
package runtimetest

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

// NewKernelStore returns an empty in-memory kernel store that is closed when
// the test ends.
func NewKernelStore(t testing.TB) *runtime.SQLiteStore {
	t.Helper()
	store, err := runtime.NewInMemorySQLiteStore()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
