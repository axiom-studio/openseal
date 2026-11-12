// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/runtime/store/sqlite. Remove once in-flight branches have landed.
//
// Re-exports of the SQLite store, kept so that importers of pkg/runtime
// continue to compile.

package runtime

import "github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"

type SQLiteStore = sqlite.Store

// NewSQLiteStore forwards to sqlite.NewSQLiteStore.
func NewSQLiteStore(path string) (*sqlite.Store, error) {
	return sqlite.NewSQLiteStore(path)
}

// NewInMemorySQLiteStore forwards to sqlite.NewInMemorySQLiteStore.
func NewInMemorySQLiteStore() (*sqlite.Store, error) {
	return sqlite.NewInMemorySQLiteStore()
}
