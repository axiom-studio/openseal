package runtime

import (
	"context"
	"sort"
	"sync"
	"time"
)

// RunActionHistory reads one Run's governed action calls for proposal
// validators. It replaces the model-written checkpoint history.
type RunActionHistory interface {
	// Calls returns the Run's non-VM action calls in terminal states
	// (succeeded, failed, denied, canceled, refused), newest CompletedAt first.
	Calls(ctx context.Context) ([]*ActionCall, error)
}

// runActionHistoryStatuses are the terminal statuses a history lists.
var runActionHistoryStatuses = []ActionCallStatus{
	ActionCallStatusSucceeded, ActionCallStatusFailed, ActionCallStatusDenied, ActionCallStatusCanceled, ActionCallStatusRefused,
}

// runActionHistoryPage is the largest page every ActionStore honors.
const runActionHistoryPage = 500

// listAllRunActionHistory pages through the Run's terminal calls, oldest
// first, until a short page; Runs are unbounded, so one page is not enough.
func listAllRunActionHistory(ctx context.Context, store ActionStore, scope Scope, runID string) ([]*ActionCall, error) {
	var all []*ActionCall
	for offset := 0; ; offset += runActionHistoryPage {
		page, err := store.ListActionCalls(ctx, ActionFilter{
			Scope: scope, RunID: runID, Status: runActionHistoryStatuses,
			Limit: runActionHistoryPage, Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < runActionHistoryPage {
			return all, nil
		}
	}
}

// newRunActionHistory memoizes the paged ActionStore.ListActionCalls{Scope, RunID,
// Status: terminal} per Propose; VM-origin calls are filtered out.
func newRunActionHistory(store ActionStore, scope Scope, runID string) RunActionHistory {
	return &storeRunActionHistory{store: store, scope: scope, runID: runID}
}

type storeRunActionHistory struct {
	store ActionStore
	scope Scope
	runID string

	mu     sync.Mutex
	loaded bool
	calls  []*ActionCall
}

func (h *storeRunActionHistory) Calls(ctx context.Context) ([]*ActionCall, error) {
	if h == nil || h.store == nil {
		return nil, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.loaded {
		listed, err := listAllRunActionHistory(ctx, h.store, h.scope, h.runID)
		if err != nil {
			return nil, err
		}
		calls := make([]*ActionCall, 0, len(listed))
		// Stores list oldest first; walking backwards keeps the newest
		// created first among calls that completed at the same instant.
		for i := len(listed) - 1; i >= 0; i-- {
			call := listed[i]
			if call == nil || call.Origin == ActionOriginVM {
				continue
			}
			calls = append(calls, call)
		}
		sort.SliceStable(calls, func(i, j int) bool {
			return runActionHistoryCompletedAt(calls[i]).After(runActionHistoryCompletedAt(calls[j]))
		})
		h.calls, h.loaded = calls, true
	}
	result := make([]*ActionCall, len(h.calls))
	for i, call := range h.calls {
		result[i] = cloneActionCall(call)
	}
	return result, nil
}

// runActionHistoryCompletedAt orders a terminal call; a row without
// CompletedAt sorts by its last update.
func runActionHistoryCompletedAt(call *ActionCall) time.Time {
	if call.CompletedAt != nil {
		return *call.CompletedAt
	}
	return call.UpdatedAt
}
