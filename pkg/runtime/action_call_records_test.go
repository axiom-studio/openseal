package runtime

import (
	"context"
	"slices"
	"sort"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
)

// actionCallRecords holds the action calls a test states for a feature that
// only lists them, so the test does not drive each call through the action
// workflow (or needs a state that workflow never leaves, such as a succeeded
// call under a pending approval). It lists them as the stores do: oldest
// first, then by ID, filtered by scope, Run and status, one page at a time.
type actionCallRecords struct {
	calls []*ActionCall
}

// put stores call as given, replacing the call with the same ID.
func (r *actionCallRecords) put(call *ActionCall) {
	for index, current := range r.calls {
		if current.ID == call.ID {
			r.calls[index] = models.CloneActionCall(call)
			return
		}
	}
	r.calls = append(r.calls, models.CloneActionCall(call))
}

// get returns a copy of a stored call.
func (r *actionCallRecords) get(t testing.TB, id string) *ActionCall {
	t.Helper()
	for _, call := range r.calls {
		if call.ID == id {
			return models.CloneActionCall(call)
		}
	}
	t.Fatalf("no action call %s", id)
	return nil
}

func (r *actionCallRecords) ListActionCalls(_ context.Context, filter ActionFilter) ([]*ActionCall, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	matched := make([]*ActionCall, 0)
	for _, call := range r.calls {
		if call.Scope == filter.Scope && (filter.RunID == "" || call.RunID == filter.RunID) &&
			(len(filter.Status) == 0 || slices.Contains(filter.Status, call.Status)) {
			matched = append(matched, models.CloneActionCall(call))
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.Before(matched[j].CreatedAt)
		}
		return matched[i].ID < matched[j].ID
	})
	return recordPage(matched, filter.Offset, filter.Limit), nil
}

// recordPage returns one page of a listed record set as the stores page it:
// from offset, at most limit records, 100 when limit is unset or over 500.
func recordPage[T any](records []T, offset, limit int) []T {
	offset = max(offset, 0)
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset >= len(records) {
		return []T{}
	}
	records = records[offset:]
	return records[:min(limit, len(records))]
}
