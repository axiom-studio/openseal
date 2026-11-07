package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// repeatExecFixture is a claimed Run with its session in the memory store; it
// plays the LoopToolExecutor around a loopRepeatGuard.
type repeatExecFixture struct {
	*loopStoreFixture
	sessions    AgentSessionStore
	coordinator *ActionCoordinator
	step        int64
}

func newRepeatExecFixture(t *testing.T) *repeatExecFixture {
	t.Helper()
	store := NewMemoryStore()
	f := newLoopStoreFixture(t, store)
	lease := AgentRunLeaseGuard{WorkerID: loopStoreWorker, Now: f.now}
	if _, err := store.CreateRunAgentSession(f.ctx, CreateRunAgentSessionRequest{
		Scope: f.scope, OwnerRunID: f.run.ID, AgentID: f.run.AssignedAgentID, Lease: lease, Now: f.now,
	}); err != nil {
		t.Fatal(err)
	}
	f.run = f.currentRun()
	if f.run.SessionID == "" {
		t.Fatal("the Run has no session")
	}
	coordinator := NewActionCoordinator(store, store, nil, nil)
	coordinator.now = func() time.Time { return f.now }
	return &repeatExecFixture{loopStoreFixture: f, sessions: store, coordinator: coordinator}
}

type execCall struct {
	name     string
	args     string
	subject  RepeatSubject
	governed bool
}

func readCall(name, args string) execCall {
	return execCall{name: name, args: args, subject: RepeatSubject{
		Identity: "workspace." + name, Arguments: mustArgs(args), Effect: RepeatEffectRead,
	}}
}

func writeCall(name, args string) execCall {
	c := readCall(name, args)
	c.subject.Effect = RepeatEffectWrite
	return c
}

func governedReadCall(args string) execCall {
	return execCall{name: "repo_read_file", args: args, governed: true, subject: RepeatSubject{
		Identity: repeatGovernedIdentity("repo", "read_file", "binding-1"), Arguments: mustArgs(args), Effect: RepeatEffectRead,
	}}
}

func mustArgs(raw string) map[string]interface{} {
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		panic(err)
	}
	return out
}

func (f *repeatExecFixture) lease() AgentRunLeaseGuard {
	return AgentRunLeaseGuard{WorkerID: loopStoreWorker, Now: f.now}
}

// appendStep appends one step's assistant entry and its tool entries pending,
// stamped with their repeat fields.
func (f *repeatExecFixture) appendStep(calls ...execCall) []loopRepeatCall {
	f.t.Helper()
	f.step++
	stepID := fmt.Sprintf("step-%d", f.step)
	assistant := &SessionEntry{
		ID: SessionEntryID(f.run.SessionID, AssistantEntryKey(stepID)), StepID: stepID, Step: f.step, Type: SessionEntryAssistant,
		Assistant: &AssistantEntry{Provider: ModelStepProviderAnthropic, Model: "claude-x", StopReason: ModelStopToolCalls},
	}
	entries := []*SessionEntry{assistant}
	out := make([]loopRepeatCall, len(calls))
	for i, call := range calls {
		callID := fmt.Sprintf("toolu_%d_%d", f.step, i)
		assistant.Assistant.ToolCalls = append(assistant.Assistant.ToolCalls, ModelToolCall{ID: callID, Name: call.name, Arguments: json.RawMessage(call.args)})
		entry := &SessionEntry{
			ID: SessionEntryID(f.run.SessionID, ToolEntryKey(stepID, callID)), StepID: stepID, Step: f.step,
			Type: SessionEntryTool, State: ToolEntryPending, ToolCallID: callID,
			Tool: &ToolEntry{Name: call.name, Arguments: json.RawMessage(call.args), SideEffect: ToolSideEffectRead},
		}
		if err := stampRepeatFields(entry.Tool, call.subject); err != nil {
			f.t.Fatal(err)
		}
		entries = append(entries, entry)
		out[i] = loopRepeatCall{Entry: entry}
		if call.governed {
			out[i].Governed = &loopRepeatGovernedCall{
				DeploymentID: "agent", SkillID: "repo", SkillVersion: "1.0.0", Action: "read_file", BindingID: "binding-1", BindingRevision: 1,
				Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Arguments: call.subject.Arguments,
			}
		}
	}
	lease := f.lease()
	result, err := f.sessions.AppendAgentSessionEntries(f.ctx, AppendAgentSessionEntriesRequest{
		Scope: f.scope, SessionID: f.run.SessionID, RunID: f.run.ID, Lease: &lease, Entries: entries, Now: f.now,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	for i := range out {
		out[i].Entry = result.Entries[i+1]
	}
	return out
}

func (f *repeatExecFixture) guard(calls []loopRepeatCall, wait bool) *loopRepeatGuard {
	f.t.Helper()
	guard, err := startLoopRepeatGuard(f.ctx, loopRepeatGuardConfig{
		Sessions: f.sessions, Refusals: f.coordinator, Run: f.run, WorkerID: loopStoreWorker,
		WaitOffered: wait, Now: func() time.Time { return f.now },
	}, calls)
	if err != nil {
		f.t.Fatal(err)
	}
	return guard
}

// transition moves entry from its state to `to` through the executor path
// (withRepeatKernelFields), observes it and returns it.
func (f *repeatExecFixture) transition(guard *loopRepeatGuard, entry *SessionEntry, to ToolEntryState, req ToolEntryTransitionRequest) *SessionEntry {
	f.t.Helper()
	req.Scope, req.EntryID, req.RunID, req.Lease, req.Now = f.scope, entry.ID, f.run.ID, f.lease(), f.now
	req.From, req.ExpectedRevision, req.To = entry.State, entry.Revision, to
	result, err := f.sessions.TransitionToolEntry(f.ctx, withRepeatKernelFields(req))
	if err != nil {
		f.t.Fatalf("transition %s %s -> %s: %v", entry.ToolCallID, entry.State, to, err)
	}
	if guard != nil {
		guard.Observe(result.Entry)
	}
	return result.Entry
}

func textResult(text string, failed bool) *ToolResult {
	status := ToolResultSucceeded
	if failed {
		status = ToolResultFailed
	}
	return &ToolResult{Status: status, IsError: failed, Content: []ContentPart{testTextPart(text)}}
}

// runStep plays a whole step: plan, refuse, execute what may run with
// results[i] (by call), and finish.
func (f *repeatExecFixture) runStep(results []string, calls ...execCall) ([]*SessionEntry, *LoopFailure) {
	f.t.Helper()
	planned := f.appendStep(calls...)
	guard := f.guard(planned, false)
	if _, err := guard.ApplyRefusals(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	for i, call := range planned {
		if !guard.Dispatch(call.Entry.ID) {
			continue
		}
		f.transition(guard, call.Entry, ToolEntryCompleted, ToolEntryTransitionRequest{Result: textResult(results[i], false)})
	}
	failure, err := guard.Finish(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.stepEntries(planned), failure
}

func (f *repeatExecFixture) stepEntries(calls []loopRepeatCall) []*SessionEntry {
	f.t.Helper()
	out := make([]*SessionEntry, len(calls))
	for i, call := range calls {
		entry, err := f.sessions.GetSessionEntry(f.ctx, f.scope, call.Entry.ID)
		if err != nil {
			f.t.Fatal(err)
		}
		out[i] = entry
	}
	return out
}

func (f *repeatExecFixture) notes() []*SessionEntry {
	f.t.Helper()
	notes, err := f.sessions.ListAgentSessionEntries(f.ctx, SessionEntryFilter{
		Scope: f.scope, SessionID: f.run.SessionID, NoteKinds: []NoteKind{NoteRepeat},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return notes
}

func requireGuardRefusal(t *testing.T, entry *SessionEntry, kind string, count int, status ToolResultStatus, text string) {
	t.Helper()
	tool := entry.Tool
	if entry.State != ToolEntryError || tool.Guard == nil || tool.Guard.Kind != kind || tool.Guard.Count != count ||
		tool.Result == nil || !tool.Result.IsError || tool.Result.Status != status || tool.ResultDigest != "" {
		t.Fatalf("entry %s = state %s, tool %#v", entry.ToolCallID, entry.State, tool)
	}
	if text != "" && tool.Result.Content[0].Text != text {
		t.Fatalf("refusal text = %q, want %q", tool.Result.Content[0].Text, text)
	}
}

func TestStampRepeatFields(t *testing.T) {
	subject := readCall("read_file", `{"path":"a"}`).subject
	tool := &ToolEntry{}
	if err := stampRepeatFields(tool, subject); err != nil {
		t.Fatal(err)
	}
	key, _ := repeatKey(subject)
	if tool.ArgumentsDigest != key || tool.RepeatEffect != "read" {
		t.Fatalf("stamped %#v", tool)
	}
	subject.Effect = "sometimes"
	if err := stampRepeatFields(&ToolEntry{}, subject); err == nil {
		t.Fatal("an invalid effect was stamped")
	}
}

func TestWithRepeatKernelFields(t *testing.T) {
	result := textResult("ok", false)
	req := withRepeatKernelFields(ToolEntryTransitionRequest{From: ToolEntryRunning, To: ToolEntryCompleted, Result: result})
	if req.Kernel == nil || req.Kernel.ResultDigest != toolResultDigest(result) || req.Kernel.Parked != nil {
		t.Fatalf("executed terminal patch = %#v", req.Kernel)
	}
	original := &ToolEntryKernelPatch{OriginRef: "command:x"}
	req = withRepeatKernelFields(ToolEntryTransitionRequest{From: ToolEntryAwaitingChild, To: ToolEntryCompleted, Result: result, Kernel: original})
	if req.Kernel.Parked == nil || !*req.Kernel.Parked || req.Kernel.OriginRef != "command:x" || original.Parked != nil {
		t.Fatalf("parked patch = %#v (original %#v)", req.Kernel, original)
	}
	req = withRepeatKernelFields(ToolEntryTransitionRequest{From: ToolEntryAwaitingApproval, To: ToolEntryRunning})
	if req.Kernel == nil || req.Kernel.Parked == nil || req.Kernel.ResultDigest != "" {
		t.Fatalf("resume patch = %#v", req.Kernel)
	}
	req = withRepeatKernelFields(ToolEntryTransitionRequest{From: ToolEntryPending, To: ToolEntryError, Result: result,
		Kernel: &ToolEntryKernelPatch{Guard: &ToolGuardOutcome{Kind: RefusedLoopCallRepeat}}})
	if req.Kernel.ResultDigest != "" {
		t.Fatal("a refusal got a result digest")
	}
	if req := withRepeatKernelFields(ToolEntryTransitionRequest{From: ToolEntryPending, To: ToolEntryRunning}); req.Kernel != nil {
		t.Fatalf("start patch = %#v", req.Kernel)
	}
}

func TestRepeatGuardReadNoteRefusalThenFailure(t *testing.T) {
	f := newRepeatExecFixture(t)
	read := readCall("read_file", `{"path":"a.txt"}`)
	first, failure := f.runStep([]string{"same"}, read)
	if failure != nil || first[0].State != ToolEntryCompleted || first[0].Tool.ResultDigest == "" || len(f.notes()) != 0 {
		t.Fatalf("1st = %#v, %#v, notes %d", first[0], failure, len(f.notes()))
	}
	second, failure := f.runStep([]string{"same"}, read)
	notes := f.notes()
	if failure != nil || second[0].State != ToolEntryCompleted || len(notes) != 1 {
		t.Fatalf("2nd: failure %#v, notes %d", failure, len(notes))
	}
	note := notes[0]
	if note.Note.Text != repeatNoteText(1, false) || note.SourceKey != RepeatNoteSourceKey(second[0].ID) ||
		note.StepID != second[0].StepID || note.Sequence <= second[0].Sequence || note.Tokens == 0 {
		t.Fatalf("note = %#v", note)
	}
	third, failure := f.runStep(nil, read)
	if failure != nil {
		t.Fatalf("3rd failure = %#v", failure)
	}
	requireGuardRefusal(t, third[0], RefusedLoopCallRepeat, 3, ToolResultRefused, repeatRefusedText(3, 2))
	if third[0].Tool.DuplicateOfEntryID != second[0].ID || third[0].ActionCallID != "" {
		t.Fatalf("3rd duplicateOf = %q, action call %q", third[0].Tool.DuplicateOfEntryID, third[0].ActionCallID)
	}
	fourth, _ := f.runStep(nil, read)
	requireGuardRefusal(t, fourth[0], RefusedLoopCallRepeat, 4, ToolResultRefused, repeatRefusedText(4, 2))

	other := readCall("read_file", `{"path":"b.txt"}`)
	fifth, failure := f.runStep(nil, read, other)
	requireGuardRefusal(t, fifth[0], RefusedLoopCallRepeatLimit, 5, ToolResultRefused, repeatLimitText(5))
	requireGuardRefusal(t, fifth[1], ToolGuardRunStopping, 0, ToolResultRefused, repeatRunStoppingText)
	if failure == nil || failure.Kind != RunFailureKindRepeatLimit || failure.Code != RunFailureCodeRepeatLimit || !failure.Explainable ||
		failure.Message != runFailureMessageRepeatLimit {
		t.Fatalf("5th failure = %#v", failure)
	}
	if len(f.notes()) != 1 {
		t.Fatalf("notes = %d", len(f.notes()))
	}
}

func TestRepeatGuardSameStepDuplicateIsRefused(t *testing.T) {
	f := newRepeatExecFixture(t)
	read := readCall("read_file", `{"path":"a.txt"}`)
	entries, failure := f.runStep([]string{"one", "two"}, read, read)
	if failure != nil || entries[0].State != ToolEntryCompleted {
		t.Fatalf("first = %#v, %#v", entries[0], failure)
	}
	requireGuardRefusal(t, entries[1], ToolGuardSameStepDuplicate, 0, ToolResultRefused, repeatSameStepText(entries[0].ToolCallID))
	if entries[1].Tool.DuplicateOfEntryID != entries[0].ID {
		t.Fatalf("duplicateOf = %q", entries[1].Tool.DuplicateOfEntryID)
	}
}

func TestRepeatGuardSuppressesSucceededWrite(t *testing.T) {
	f := newRepeatExecFixture(t)
	write := writeCall("send", `{"to":"a"}`)
	first, _ := f.runStep([]string{"sent #1"}, write)
	second, failure := f.runStep(nil, write)
	if failure != nil {
		t.Fatalf("failure = %#v", failure)
	}
	requireGuardRefusal(t, second[0], RefusedLoopCallDuplicateSuppressed, 2, ToolResultDuplicateSuppressed, repeatSuppressedText(1))
	content := second[0].Tool.Result.Content
	if len(content) != 2 || content[1].Text != "sent #1" || second[0].Tool.DuplicateOfEntryID != first[0].ID {
		t.Fatalf("suppressed result = %#v", second[0].Tool)
	}
}

func TestRepeatGuardGovernedRefusalRecordsRefusedRow(t *testing.T) {
	f := newRepeatExecFixture(t)
	read := governedReadCall(`{"path":"a.txt"}`)
	var priorCallID string
	for i := 0; i < 2; i++ {
		planned := f.appendStep(read)
		guard := f.guard(planned, true)
		if refused, err := guard.ApplyRefusals(f.ctx); err != nil || len(refused) != 0 || !guard.Dispatch(planned[0].Entry.ID) {
			t.Fatalf("step %d refused %d, %v", i+1, len(refused), err)
		}
		call := f.propose(f.loopCall(fmt.Sprintf("call-%d", i+1)))
		priorCallID = call.ID
		f.transition(guard, planned[0].Entry, ToolEntryCompleted, ToolEntryTransitionRequest{ActionCallID: call.ID, Result: textResult("same", false)})
		if failure, err := guard.Finish(f.ctx); err != nil || failure != nil {
			t.Fatalf("finish = %#v, %v", failure, err)
		}
	}
	if notes := f.notes(); len(notes) != 1 || notes[0].Note.Text != repeatNoteText(1, true) {
		t.Fatalf("notes = %#v", notes)
	}
	planned := f.appendStep(read)
	guard := f.guard(planned, true)
	refused, err := guard.ApplyRefusals(f.ctx)
	if err != nil || len(refused) != 1 || guard.Dispatch(planned[0].Entry.ID) {
		t.Fatalf("refusals = %d, %v", len(refused), err)
	}
	entry := refused[0]
	requireGuardRefusal(t, entry, RefusedLoopCallRepeat, 3, ToolResultRefused, "")
	row, err := f.store.GetActionCall(f.ctx, f.scope, entry.ActionCallID)
	if err != nil {
		t.Fatalf("refused row %q: %v", entry.ActionCallID, err)
	}
	if row.Status != ActionCallStatusRefused || row.TurnID != entry.StepID || row.DuplicateOfActionCallID != priorCallID ||
		row.ErrorCode != RefusedLoopCallRepeat || row.Error != entry.Tool.Result.Content[0].Text || row.ErrorDetails["count"] != "3" ||
		row.ErrorDetails["step"] != "2" || row.BindingID != "binding-1" {
		t.Fatalf("refused row = %#v", row)
	}
	if failure, err := guard.Finish(f.ctx); err != nil || failure != nil {
		t.Fatalf("finish = %#v, %v", failure, err)
	}
}

func TestRepeatGuardGovernedRefusalNeedsCoordinator(t *testing.T) {
	f := newRepeatExecFixture(t)
	read := governedReadCall(`{"path":"a.txt"}`)
	for i := 0; i < 2; i++ {
		planned := f.appendStep(read)
		call := f.propose(f.loopCall(fmt.Sprintf("call-%d", i+1)))
		f.transition(nil, planned[0].Entry, ToolEntryCompleted, ToolEntryTransitionRequest{ActionCallID: call.ID, Result: textResult("same", false)})
	}
	planned := f.appendStep(read)
	guard, err := startLoopRepeatGuard(f.ctx, loopRepeatGuardConfig{Sessions: f.sessions, Run: f.run, WorkerID: loopStoreWorker}, planned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.ApplyRefusals(f.ctx); err == nil {
		t.Fatal("a governed refusal was written without its refused row")
	}
	if entry := f.stepEntries(planned)[0]; entry.State != ToolEntryPending {
		t.Fatalf("entry = %s", entry.State)
	}
}

func TestRepeatGuardResumeKeepsStepDecisions(t *testing.T) {
	t.Run("same-step duplicate after a crash", func(t *testing.T) {
		f := newRepeatExecFixture(t)
		read := readCall("read_file", `{"path":"a.txt"}`)
		planned := f.appendStep(read, read)
		// The first pass ran the first call, then crashed before refusing
		// the second.
		f.transition(nil, planned[0].Entry, ToolEntryCompleted, ToolEntryTransitionRequest{Result: textResult("one", false)})
		resumed := f.stepEntries(planned)
		guard := f.guard([]loopRepeatCall{{Entry: resumed[0]}, {Entry: resumed[1]}}, false)
		if _, ok := guard.Verdict(resumed[0].ID); ok || guard.Dispatch(resumed[0].ID) {
			t.Fatal("a terminal entry was evaluated again")
		}
		if _, err := guard.ApplyRefusals(f.ctx); err != nil {
			t.Fatal(err)
		}
		requireGuardRefusal(t, f.stepEntries(planned)[1], ToolGuardSameStepDuplicate, 0, ToolResultRefused, "")
	})
	t.Run("repeat limit before a crash stops the rest", func(t *testing.T) {
		f := newRepeatExecFixture(t)
		read := readCall("read_file", `{"path":"a.txt"}`)
		for i := 0; i < 4; i++ {
			f.runStep([]string{"same"}, read)
		}
		other := readCall("read_file", `{"path":"b.txt"}`)
		planned := f.appendStep(read, other)
		first := f.guard(planned, false)
		if !first.Failed() {
			t.Fatal("5th identical call did not fail the step")
		}
		// Refuse only the failing call, then crash.
		refusal, _ := first.verdicts[0].refusal()
		f.transition(nil, planned[0].Entry, ToolEntryError, ToolEntryTransitionRequest{
			Result: refusal.Result, Kernel: &ToolEntryKernelPatch{Guard: refusal.Guard, DuplicateOfEntryID: refusal.DuplicateOfEntryID},
		})
		resumed := f.stepEntries(planned)
		guard := f.guard([]loopRepeatCall{{Entry: resumed[0]}, {Entry: resumed[1]}}, false)
		if !guard.Failed() || guard.Dispatch(resumed[1].ID) {
			t.Fatal("resume dispatched a sibling of the failing call")
		}
		if _, err := guard.ApplyRefusals(f.ctx); err != nil {
			t.Fatal(err)
		}
		requireGuardRefusal(t, f.stepEntries(planned)[1], ToolGuardRunStopping, 0, ToolResultRefused, repeatRunStoppingText)
		failure, err := guard.Finish(f.ctx)
		if err != nil || failure == nil || failure.Code != RunFailureCodeRepeatLimit {
			t.Fatalf("finish = %#v, %v", failure, err)
		}
	})
}

func TestRepeatGuardFinishWaitsForOpenEntries(t *testing.T) {
	f := newRepeatExecFixture(t)
	read := readCall("read_file", `{"path":"a.txt"}`)
	f.runStep([]string{"same"}, read)
	approval := writeCall("deploy", `{"env":"prod"}`)
	planned := f.appendStep(read, approval)
	guard := f.guard(planned, false)
	if _, err := guard.ApplyRefusals(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.transition(guard, planned[0].Entry, ToolEntryCompleted, ToolEntryTransitionRequest{Result: textResult("same", false)})
	waiting := f.transition(guard, planned[1].Entry, ToolEntryAwaitingApproval, ToolEntryTransitionRequest{Wait: &ToolEntryWait{ApprovalID: "approval-1"}})
	if failure, err := guard.Finish(f.ctx); err != nil || failure != nil || len(f.notes()) != 0 {
		t.Fatalf("parked finish = %#v, %v, notes %d", failure, err, len(f.notes()))
	}
	// The resume settles the held call; a guard opened for it finishes the step.
	resumed := f.stepEntries(planned)
	again := f.guard([]loopRepeatCall{{Entry: resumed[0]}, {Entry: resumed[1]}}, false)
	running := f.transition(again, waiting, ToolEntryRunning, ToolEntryTransitionRequest{})
	if !running.Tool.Parked {
		t.Fatal("leaving awaiting_approval did not mark the entry parked")
	}
	f.transition(again, running, ToolEntryCompleted, ToolEntryTransitionRequest{Result: textResult("deployed", false)})
	for i := 0; i < 2; i++ {
		if failure, err := again.Finish(f.ctx); err != nil || failure != nil {
			t.Fatalf("finish %d = %#v, %v", i, failure, err)
		}
	}
	notes := f.notes()
	if len(notes) != 1 || notes[0].SourceKey != RepeatNoteSourceKey(resumed[0].ID) {
		t.Fatalf("notes = %#v", notes)
	}
}

func TestRepeatGuardErrorStreakFailsRun(t *testing.T) {
	f := newRepeatExecFixture(t)
	for i := 1; i <= loopErrorStreakLimit; i++ {
		planned := f.appendStep(readCall("read_file", fmt.Sprintf(`{"path":"missing-%d"}`, i)))
		guard := f.guard(planned, false)
		f.transition(guard, planned[0].Entry, ToolEntryError, ToolEntryTransitionRequest{Result: textResult("not found", true)})
		failure, err := guard.Finish(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if i < loopErrorStreakLimit && failure != nil {
			t.Fatalf("step %d failed the Run: %#v", i, failure)
		}
		if i == loopErrorStreakLimit && (failure == nil || failure.Code != RunFailureCodeErrorStreak || failure.Kind != RunFailureKindRepeatLimit || !failure.Explainable) {
			t.Fatalf("step %d failure = %#v", i, failure)
		}
	}
}

func TestRepeatGuardRejectsUnstampedEntries(t *testing.T) {
	f := newRepeatExecFixture(t)
	planned := f.appendStep(readCall("read_file", `{"path":"a"}`))
	unstamped := cloneSessionEntry(planned[0].Entry)
	unstamped.Tool.ArgumentsDigest = ""
	if _, err := startLoopRepeatGuard(f.ctx, loopRepeatGuardConfig{Sessions: f.sessions, Run: f.run, WorkerID: loopStoreWorker},
		[]loopRepeatCall{{Entry: unstamped}}); err == nil || !strings.Contains(err.Error(), "repeat key") {
		t.Fatalf("unstamped entry = %v", err)
	}
	if _, err := startLoopRepeatGuard(f.ctx, loopRepeatGuardConfig{Sessions: f.sessions, Run: f.run}, planned); err == nil {
		t.Fatal("a guard started without the lease owner")
	}
}

func TestRepeatGuardLostLeaseWritesNothing(t *testing.T) {
	f := newRepeatExecFixture(t)
	read := readCall("read_file", `{"path":"a.txt"}`)
	for i := 0; i < 2; i++ {
		f.runStep([]string{"same"}, read)
	}
	planned := f.appendStep(read)
	guard, err := startLoopRepeatGuard(f.ctx, loopRepeatGuardConfig{
		Sessions: f.sessions, Run: f.run, WorkerID: "other-worker", Now: func() time.Time { return f.now },
	}, planned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guard.ApplyRefusals(f.ctx); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("refusal under a foreign lease = %v", err)
	}
	if entry := f.stepEntries(planned)[0]; entry.State != ToolEntryPending {
		t.Fatalf("entry = %s", entry.State)
	}
}

func TestRepeatGuardObserveIsConcurrent(t *testing.T) {
	f := newRepeatExecFixture(t)
	calls := make([]execCall, 8)
	results := make([]string, 8)
	for i := range calls {
		calls[i] = readCall("read_file", fmt.Sprintf(`{"path":"f-%d"}`, i))
		results[i] = "ok"
	}
	planned := f.appendStep(calls...)
	guard := f.guard(planned, false)
	var wg sync.WaitGroup
	for _, call := range planned {
		wg.Add(1)
		go func(entry *SessionEntry) {
			defer wg.Done()
			req := withRepeatKernelFields(ToolEntryTransitionRequest{
				Scope: f.scope, EntryID: entry.ID, RunID: f.run.ID, Lease: f.lease(), Now: f.now,
				From: ToolEntryPending, To: ToolEntryCompleted, Result: textResult("ok", false),
			})
			result, err := f.sessions.TransitionToolEntry(context.Background(), req)
			if err != nil {
				t.Error(err)
				return
			}
			guard.Observe(result.Entry)
		}(call.Entry)
	}
	wg.Wait()
	if failure, err := guard.Finish(f.ctx); err != nil || failure != nil || len(f.notes()) != 0 {
		t.Fatalf("finish = %#v, %v", failure, err)
	}
}
