// Package kerneltest opens kernel stores for tests.
package kerneltest

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

// NewStore returns an empty in-memory kernel store that is closed when
// the test ends.
func NewStore(t testing.TB) *sqlite.Store {
	t.Helper()
	store, err := sqlite.NewInMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}
