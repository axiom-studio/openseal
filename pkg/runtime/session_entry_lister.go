package runtime

import (
	"context"
)

type loopSessionEntryLister interface {
	ListAgentSessionEntries(ctx context.Context, filter SessionEntryFilter) ([]*SessionEntry, error)
}
