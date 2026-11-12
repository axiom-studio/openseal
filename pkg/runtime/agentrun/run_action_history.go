package agentrun

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

// RunActionHistory reads one Run's governed action calls for proposal
// validators. It replaces the model-written checkpoint history.
type RunActionHistory interface {
	// Calls returns the Run's non-VM action calls in terminal states
	// (succeeded, failed, denied, canceled, refused), newest CompletedAt first.
	Calls(ctx context.Context) ([]*models.ActionCall, error)
}

// runActionHistoryStatuses are the terminal statuses a history lists.
var runActionHistoryStatuses = []models.ActionCallStatus{
	models.ActionCallStatusSucceeded, models.ActionCallStatusFailed, models.ActionCallStatusDenied, models.ActionCallStatusCanceled, models.ActionCallStatusRefused,
}

// runActionHistoryPage is the largest page every ActionStore honors.
const runActionHistoryPage = 500

// listAllRunActionHistory pages through the Run's terminal calls, oldest
// first, until a short page; Runs are unbounded, so one page is not enough.
func ListAllRunActionHistory(ctx context.Context, store models.ActionStore, scope models.Scope, runID string) ([]*models.ActionCall, error) {
	var all []*models.ActionCall
	for offset := 0; ; offset += runActionHistoryPage {
		page, err := store.ListActionCalls(ctx, models.ActionFilter{
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
func NewRunActionHistory(store models.ActionStore, scope models.Scope, runID string) RunActionHistory {
	return &storeRunActionHistory{store: store, scope: scope, runID: runID}
}

type storeRunActionHistory struct {
	store models.ActionStore
	scope models.Scope
	runID string

	mu     sync.Mutex
	loaded bool
	calls  []*models.ActionCall
}

func (h *storeRunActionHistory) Calls(ctx context.Context) ([]*models.ActionCall, error) {
	if h == nil || h.store == nil {
		return nil, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.loaded {
		listed, err := ListAllRunActionHistory(ctx, h.store, h.scope, h.runID)
		if err != nil {
			return nil, err
		}
		calls := make([]*models.ActionCall, 0, len(listed))
		// Stores list oldest first; walking backwards keeps the newest
		// created first among calls that completed at the same instant.
		for i := len(listed) - 1; i >= 0; i-- {
			call := listed[i]
			if call == nil || call.Origin == models.ActionOriginVM {
				continue
			}
			calls = append(calls, call)
		}
		sort.SliceStable(calls, func(i, j int) bool {
			return runActionHistoryCompletedAt(calls[i]).After(runActionHistoryCompletedAt(calls[j]))
		})
		h.calls, h.loaded = calls, true
	}
	result := make([]*models.ActionCall, len(h.calls))
	for i, call := range h.calls {
		result[i] = models.CloneActionCall(call)
	}
	return result, nil
}

// runActionHistoryCompletedAt orders a terminal call; a row without
// CompletedAt sorts by its last update.
func runActionHistoryCompletedAt(call *models.ActionCall) time.Time {
	if call.CompletedAt != nil {
		return *call.CompletedAt
	}
	return call.UpdatedAt
}
