package runtime

import (
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
)

// The Postgres store satisfies these interfaces owned by feature packages.
// The checks live here because the store package cannot import them.
var (
	_ workqueue.WorkOutboxStore                   = (*postgres.Store)(nil)
	_ externalconversation.ChannelActivationStore = (*postgres.Store)(nil)
)
