package runtime

import (
	"context"
)

// WorkOutboxStore is a store whose committed work changes are recorded in a
// transactional outbox for relay into a WorkQueue.
type WorkOutboxStore interface {
	// WorkCommitted receives after this process commits a write that may
	// have added outbox rows. It is a coalescing hint, never closed.
	WorkCommitted() <-chan struct{}
	// RelayWorkOutbox removes up to limit outbox rows, from every process,
	// and hands them to publish in the same transaction: rows are only
	// removed once publish succeeds. It returns how many rows it removed.
	RelayWorkOutbox(ctx context.Context, limit int, publish func(context.Context, []WorkItem) error) (int, error)
	// ListPendingWork returns one item per family and scope with durable
	// work that may need a pass, at its earliest deadline. Recent marks a
	// rebuild after the queue lost its schedule: it adds scopes with recent
	// conversation activity, which the claimable tables do not cover.
	ListPendingWork(ctx context.Context, rebuild bool) ([]WorkItem, error)
}
