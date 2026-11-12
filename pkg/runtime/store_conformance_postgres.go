package runtime

import (
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/workqueue"
)

// The Postgres store satisfies these runtime-owned interfaces. The checks
// live in runtime because the store package cannot import it.
var (
	_ workqueue.WorkOutboxStore = (*postgres.Store)(nil)
	_ channelActivationStore    = (*postgres.Store)(nil)
)
