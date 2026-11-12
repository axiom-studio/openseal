package runtime

import (
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

// The SQLite store satisfies these interfaces owned by feature packages.
// The checks live here because the store package cannot import them.
var _ externalconversation.ChannelActivationStore = (*sqlite.Store)(nil)
