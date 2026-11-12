// Package runtimetest provides kernel stores for the tests of packages built
// on the runtime packages.
package runtimetest

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

// NewKernelStore returns an empty in-memory kernel store that is closed when
// the test ends.
func NewKernelStore(t testing.TB) *sqlite.Store {
	t.Helper()
	store, err := sqlite.NewInMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
