package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/axiom-studio/openseal/pkg/builtin"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
)

// fakeSessionEntryLister serves SessionEntryFilter pages from a fixed entry
// list in sequence order, honoring AfterSequence, Types and Limit.
type fakeSessionEntryLister struct {
	entries []*SessionEntry
	calls   int
}

func (f *fakeSessionEntryLister) ListAgentSessionEntries(_ context.Context, filter SessionEntryFilter) ([]*SessionEntry, error) {
	f.calls++
	normalized, err := normalizeSessionEntryFilter(filter)
	if err != nil {
		return nil, err
	}
	var page []*SessionEntry
	for _, entry := range f.entries {
		if sessionEntryMatchesFilter(entry, normalized) {
			page = append(page, entry)
			if len(page) == normalized.Limit {
				break
			}
		}
	}
	return page, nil
}

type errorStreakLog struct {
	scope   Scope
	entries []*SessionEntry
	seq     int64
}

func (l *errorStreakLog) add(entry *SessionEntry) {
	l.seq++
	entry.Scope, entry.SessionID, entry.RunID, entry.Sequence = l.scope, "session-1", "run-1", l.seq
	entry.ID = fmt.Sprintf("e_%d", l.seq)
	l.entries = append(l.entries, entry)
}

// step appends one step's tool entries; true = an is_error result.
func (l *errorStreakLog) step(step int64, results ...bool) {
	for i, isError := range results {
		state, status := ToolEntryCompleted, ToolResultSucceeded
		if isError {
			state, status = ToolEntryError, ToolResultFailed
		}
		l.add(&SessionEntry{
			Type: SessionEntryTool, Step: step, StepID: fmt.Sprintf("step-%d", step), State: state,
			ToolCallID: fmt.Sprintf("call-%d-%d", step, i),
			Tool: &ToolEntry{Name: "linear_get_issue", Result: &ToolResult{Status: status, IsError: isError,
				Content: []ContentPart{storetest.TextPart("result")}}},
		})
	}
}

func TestErrorStreakFailsRun(t *testing.T) {
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "guards"}
	log := &errorStreakLog{scope: scope}
	for step := int64(1); step < loopErrorStreakLimit; step++ {
		log.step(step, true, true)
	}
	store := &fakeSessionEntryLister{entries: log.entries}
	failure, err := loopErrorStreakFailure(ctx, store, scope, "session-1", "run-1")
	if err != nil || failure != nil {
		t.Fatalf("seven all-error steps: failure=%+v err=%v", failure, err)
	}

	log.step(loopErrorStreakLimit, true)
	store.entries = log.entries
	failure, err = loopErrorStreakFailure(ctx, store, scope, "session-1", "run-1")
	if err != nil || failure == nil {
		t.Fatalf("eighth all-error step: failure=%+v err=%v", failure, err)
	}
	if failure.Kind != modelstep.RunFailureKindRepeatLimit || failure.Code != modelstep.RunFailureCodeErrorStreak ||
		failure.Message != "The agent's tool calls kept failing without making progress, so the run was stopped." || !failure.Explainable {
		t.Fatalf("failure = %+v", failure)
	}

	cases := []struct {
		name  string
		build func(l *errorStreakLog)
		want  int
	}{
		{"a success in a step breaks it", func(l *errorStreakLog) {
			for s := int64(1); s <= 9; s++ {
				if s == 2 {
					l.step(s, true, false)
					continue
				}
				l.step(s, true)
			}
		}, 7},
		{"user guidance resets it", func(l *errorStreakLog) {
			for s := int64(1); s <= 9; s++ {
				if s == 4 {
					l.add(&SessionEntry{Type: SessionEntryUser, User: &UserEntry{}})
				}
				l.step(s, true)
			}
		}, 6},
		{"interrupted entries are neutral", func(l *errorStreakLog) {
			for s := int64(1); s <= 8; s++ {
				l.step(s, true)
				if s == 5 {
					l.add(&SessionEntry{Type: SessionEntryTool, Step: s, StepID: "step-5", State: ToolEntryInterrupted, ToolCallID: "lost",
						Tool: &ToolEntry{Name: "write", Result: &ToolResult{Status: ToolResultInterrupted, IsError: true}}})
				}
			}
			l.add(&SessionEntry{Type: SessionEntryTool, Step: 9, StepID: "step-9", State: ToolEntryInterrupted, ToolCallID: "lost-9",
				Tool: &ToolEntry{Name: "write", Result: &ToolResult{Status: ToolResultInterrupted, IsError: true}}})
		}, 8},
		{"an open entry breaks it", func(l *errorStreakLog) {
			for s := int64(1); s <= 8; s++ {
				l.step(s, true)
				if s == 1 {
					l.add(&SessionEntry{Type: SessionEntryTool, Step: s, StepID: "step-1", State: ToolEntryAwaitingApproval, ToolCallID: "held",
						Tool: &ToolEntry{Name: "send"}})
				}
			}
		}, 7},
	}
	for _, tc := range cases {
		l := &errorStreakLog{scope: scope}
		tc.build(l)
		if got := loopErrorStreak(l.entries); got != tc.want {
			t.Fatalf("%s: streak %d, want %d", tc.name, got, tc.want)
		}
	}

	// The loader pages through long Runs.
	long := &errorStreakLog{scope: scope}
	for s := int64(1); s <= 1200; s++ {
		long.step(s, s > 1200-loopErrorStreakLimit)
	}
	paged := &fakeSessionEntryLister{entries: long.entries}
	failure, err = loopErrorStreakFailure(ctx, paged, scope, "session-1", "run-1")
	if err != nil || failure == nil || failure.Code != modelstep.RunFailureCodeErrorStreak || paged.calls != 2 {
		t.Fatalf("paged: failure=%+v err=%v calls=%d", failure, err, paged.calls)
	}
}

type fakeActionCallLister struct {
	calls []*ActionCall
	pages int
}

func (f *fakeActionCallLister) ListActionCalls(_ context.Context, filter ActionFilter) ([]*ActionCall, error) {
	f.pages++
	var matched []*ActionCall
	for _, call := range f.calls {
		if call.Scope != filter.Scope || call.RunID != filter.RunID {
			continue
		}
		if len(filter.Status) > 0 && !slices.Contains(filter.Status, call.Status) {
			continue
		}
		matched = append(matched, call)
	}
	if filter.Offset >= len(matched) {
		return nil, nil
	}
	matched = matched[filter.Offset:]
	if filter.Limit > 0 && len(matched) > filter.Limit {
		matched = matched[:filter.Limit]
	}
	return matched, nil
}

func TestImageProviderRejectionRefusesFurtherGenerations(t *testing.T) {
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "guards"}
	target := imageGenerationTarget{SkillID: builtin.SkillID, Action: builtin.GenerateImage, BindingID: "binding-1", BindingRevision: 3}
	imageCall := func(id, runID string, status ActionCallStatus, code string) *ActionCall {
		return &ActionCall{ID: id, Scope: scope, RunID: runID, SkillID: builtin.SkillID, Action: builtin.GenerateImage,
			BindingID: "binding-1", BindingRevision: 3, Status: status, ErrorCode: code, Error: "image provider rejected the request"}
	}
	actions := &fakeActionCallLister{}

	// Nothing rejected yet: the call runs.
	actions.calls = []*ActionCall{
		imageCall("ac_ok", "run-1", ActionCallStatusSucceeded, ""),
		imageCall("ac_other_failure", "run-1", ActionCallStatusFailed, "provider_unavailable"),
		imageCall("ac_other_run", "run-2", ActionCallStatusFailed, ActionErrorCodeImageProviderRejected),
	}
	prior, err := priorImageProviderRejection(ctx, actions, scope, "run-1", target)
	if err != nil || prior != nil {
		t.Fatalf("before rejection: prior=%+v err=%v", prior, err)
	}

	// After a rejection on the binding, every later generation is refused.
	rejected := imageCall("ac_rejected", "run-1", ActionCallStatusFailed, ActionErrorCodeImageProviderRejected)
	actions.calls = append(actions.calls, rejected)
	prior, err = priorImageProviderRejection(ctx, actions, scope, "run-1", target)
	if err != nil || prior == nil || prior.ID != "ac_rejected" {
		t.Fatalf("after rejection: prior=%+v err=%v", prior, err)
	}
	result, guard := imageProviderRejectionRefusal(prior)
	if result.Status != ToolResultRefused || !result.IsError || len(result.Content) != 1 || result.Content[0].Type != ContentPartJSON {
		t.Fatalf("result = %+v", result)
	}
	var value map[string]string
	if err := json.Unmarshal(result.Content[0].Value, &value); err != nil {
		t.Fatal(err)
	}
	if value["status"] != "refused" || value["code"] != ActionErrorCodeImageProviderRejected ||
		value["error"] != "The image provider rejected this request; ask the user to change the image settings." ||
		value["priorEvidenceRef"] != "action-call:ac_rejected" {
		t.Fatalf("value = %v", value)
	}
	if guard == nil || guard.Kind != ToolGuardImageProviderRejected {
		t.Fatalf("guard = %+v", guard)
	}
	if _, err := validateContentParts(result.Content, contentPartLimits{}); err != nil {
		t.Fatalf("refusal content invalid: %v", err)
	}

	// Another binding, a new binding revision and other actions are not blocked.
	for name, other := range map[string]imageGenerationTarget{
		"other binding": {SkillID: builtin.SkillID, Action: builtin.GenerateImage, BindingID: "binding-2", BindingRevision: 3},
		"new revision":  {SkillID: builtin.SkillID, Action: builtin.GenerateImage, BindingID: "binding-1", BindingRevision: 4},
		"profile image": {SkillID: builtin.SkillID, Action: builtin.GenerateProfileImage, BindingID: "binding-1", BindingRevision: 3},
		"other skill":   {SkillID: "acme.images", Action: builtin.GenerateImage, BindingID: "binding-1", BindingRevision: 3},
	} {
		prior, err := priorImageProviderRejection(ctx, actions, scope, "run-1", other)
		if err != nil || prior != nil {
			t.Fatalf("%s: prior=%+v err=%v", name, prior, err)
		}
	}

	// A rejection past the first page is still found.
	many := &fakeActionCallLister{}
	for i := 0; i < loopActionCallPageSize; i++ {
		many.calls = append(many.calls, imageCall(fmt.Sprintf("ac_%d", i), "run-1", ActionCallStatusFailed, "provider_unavailable"))
	}
	many.calls = append(many.calls, rejected)
	prior, err = priorImageProviderRejection(ctx, many, scope, "run-1", target)
	if err != nil || prior == nil || prior.ID != "ac_rejected" || many.pages != 2 {
		t.Fatalf("paged: prior=%+v err=%v pages=%d", prior, err, many.pages)
	}
}
