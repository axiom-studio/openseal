package runtime

import "github.com/axiom-studio/openseal/pkg/runtime/store/postgres"

// The Postgres store satisfies these runtime-owned interfaces. The checks
// live in runtime because the store package cannot import it.
var (
	_ WorkOutboxStore        = (*postgres.PostgresStore)(nil)
	_ channelActivationStore = (*postgres.PostgresStore)(nil)
)
