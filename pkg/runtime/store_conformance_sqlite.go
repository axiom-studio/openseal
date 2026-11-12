package runtime

import "github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"

// The SQLite store satisfies these runtime-owned interfaces. The checks
// live in runtime because the store package cannot import it.
var _ channelActivationStore = (*sqlite.SQLiteStore)(nil)
