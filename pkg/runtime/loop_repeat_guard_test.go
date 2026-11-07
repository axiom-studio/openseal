package runtime

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// repeatSim plays the executor's side of the ledger (section 08 §5.5): it
// appends a step's calls pending, plans them, settles the run verdicts with
// the given results and the others with their refusals, and collects notes.
type repeatSim struct {
	t      *testing.T
	scope  Scope
	seq    int64
	ledger *repeatLedger
	byID   map[string]*SessionEntry
	order  []string
}

func newRepeatSim(t *testing.T) *repeatSim {
	return &repeatSim{t: t, scope: Scope{Kind: "tenant", ID: "repeat"}, ledger: newRepeatLedger(), byID: map[string]*SessionEntry{}}
}

type simCall struct {
	subject     RepeatSubject
	result      string // executed result text
	failed      bool   // executed with an error result
	parked      bool   // the entry left awaiting_* before completing
	interrupted bool   // the executed call was interrupted
}

type simOutcome struct {
	verdict repeatVerdict
	refusal *repeatRefusal
	entry   *SessionEntry
	note    string
}

func (s *repeatSim) store(entry *SessionEntry) {
	if _, ok := s.byID[entry.ID]; !ok {
		s.order = append(s.order, entry.ID)
	}
	s.byID[entry.ID] = entry
	s.ledger.observe(entry)
}

func (s *repeatSim) entries() []*SessionEntry {
	out := make([]*SessionEntry, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, s.byID[id])
	}
	return out
}

func (s *repeatSim) user(text string) {
	s.seq++
	s.store(&SessionEntry{
		ID: fmt.Sprintf("e_%d", s.seq), Scope: s.scope, SessionID: "session-1", RunID: "run-1", Sequence: s.seq,
		Type: SessionEntryUser, User: &UserEntry{Origin: "steering", Content: []ContentPart{testTextPart(text)}},
	})
}

func (s *repeatSim) step(step int64, waitOffered bool, calls ...simCall) ([]simOutcome, bool) {
	s.t.Helper()
	planned := make([]repeatStepCall, len(calls))
	for i, call := range calls {
		key, err := repeatKey(call.subject)
		if err != nil {
			s.t.Fatalf("repeat key: %v", err)
		}
		s.seq++
		entry := &SessionEntry{
			ID: fmt.Sprintf("e_%d", s.seq), Scope: s.scope, SessionID: "session-1", RunID: "run-1", Sequence: s.seq,
			StepID: fmt.Sprintf("step-%d", step), Step: step, Type: SessionEntryTool, State: ToolEntryPending,
			ToolCallID: fmt.Sprintf("call_%d_%d", step, i), ActionCallID: fmt.Sprintf("ac_%d", s.seq), Revision: 1,
			Tool: &ToolEntry{Name: call.subject.Identity, ArgumentsDigest: key, RepeatEffect: string(call.subject.Effect)},
		}
		s.store(entry)
		planned[i] = repeatStepCall{Entry: entry, Subject: call.subject, Key: key}
	}
	verdicts, failed := s.ledger.planStep(planned)
	outcomes := make([]simOutcome, len(calls))
	for i, call := range calls {
		pending := planned[i].Entry
		next := *pending
		tool := *pending.Tool
		next.Tool, next.Revision = &tool, pending.Revision+1
		outcome := simOutcome{verdict: verdicts[i]}
		if refusal, ok := verdicts[i].refusal(); ok {
			next.State = ToolEntryError
			tool.Result, tool.Guard, tool.DuplicateOfEntryID = refusal.Result, refusal.Guard, refusal.DuplicateOfEntryID
			outcome.refusal = refusal
			s.store(&next)
			outcome.entry = &next
			outcomes[i] = outcome
			continue
		}
		switch {
		case call.interrupted:
			next.State = ToolEntryInterrupted
			tool.Result = &ToolResult{Status: ToolResultInterrupted, IsError: true, Content: []ContentPart{testTextPart("interrupted")}}
		case call.failed:
			next.State = ToolEntryError
			tool.Result = &ToolResult{Status: ToolResultFailed, IsError: true, Content: []ContentPart{testTextPart(call.result)}}
		default:
			next.State = ToolEntryCompleted
			tool.Result = &ToolResult{Status: ToolResultSucceeded, Content: []ContentPart{testTextPart(call.result)}}
		}
		tool.Parked = call.parked
		tool.ResultDigest = toolResultDigest(tool.Result)
		s.store(&next)
		outcome.entry = &next
		if note, ok := s.ledger.noteAfter(&next, waitOffered); ok {
			outcome.note = note
		}
		outcomes[i] = outcome
	}
	return outcomes, failed
}

// one runs a single-call step and returns its outcome.
func (s *repeatSim) one(step int64, call simCall) simOutcome {
	s.t.Helper()
	outcomes, _ := s.step(step, false, call)
	return outcomes[0]
}

func hostCall(operation string, args map[string]interface{}) RepeatSubject {
	return RepeatSubject{Identity: operation, Arguments: args, Effect: repeatEffectOf(LoopToolHost, "", false, operation)}
}

func governedCall(skillID, action string, sideEffect capability.SideEffect, repeatable bool, args map[string]interface{}) RepeatSubject {
	return RepeatSubject{
		Identity:  repeatGovernedIdentity(skillID, action, "binding-1"),
		Arguments: args,
		Effect:    repeatEffectOf(LoopToolGoverned, sideEffect, repeatable, repeatGovernedOperation(skillID, action)),
	}
}

func kernelCall(name string, args map[string]interface{}) RepeatSubject {
	return RepeatSubject{Identity: name, Arguments: args, Effect: repeatEffectOf(LoopToolKernel, "", false, name)}
}

func linearGet(id string) RepeatSubject {
	return governedCall("linear", "get_issue", capability.SideEffectRead, false, map[string]interface{}{"id": id})
}

func slackSend(text string) RepeatSubject {
	return governedCall("slack", "send_message", capability.SideEffectExternal, false, map[string]interface{}{"text": text})
}

func expectRun(t *testing.T, label string, outcome simOutcome) {
	t.Helper()
	if outcome.verdict.Kind != repeatRun {
		t.Fatalf("%s: verdict = %+v, want run", label, outcome.verdict)
	}
}

func expectRefused(t *testing.T, label string, outcome simOutcome, kind repeatVerdictKind, code string, count int) {
	t.Helper()
	if outcome.verdict.Kind != kind || outcome.verdict.Code != code || outcome.verdict.Count != count {
		t.Fatalf("%s: verdict = %+v, want kind %d code %s count %d", label, outcome.verdict, kind, code, count)
	}
	if outcome.refusal == nil || !outcome.refusal.Result.IsError || outcome.entry.State != ToolEntryError {
		t.Fatalf("%s: refusal = %+v entry state %s", label, outcome.refusal, outcome.entry.State)
	}
}

func TestRepeatReadNoteThenRefusalThenFailure(t *testing.T) {
	sim := newRepeatSim(t)
	read := simCall{subject: linearGet("LIN-6815"), result: "issue LIN-6815"}

	first := sim.one(1, read)
	expectRun(t, "1st", first)
	if first.note != "" {
		t.Fatalf("1st call got a note: %q", first.note)
	}
	second := sim.one(2, read)
	expectRun(t, "2nd", second)
	if want := "You already made this exact call at step 1 and got the same result. Use that result instead of calling again."; second.note != want {
		t.Fatalf("2nd note = %q, want %q", second.note, want)
	}
	third := sim.one(3, read)
	expectRefused(t, "3rd", third, repeatRefuse, RefusedLoopCallRepeat, 3)
	if want := "Not run: this exact call was already made twice in this run with the same result (last at step 2). Use the earlier result, try a different step, or say what is blocking you."; third.refusal.Message != want {
		t.Fatalf("3rd text = %q", third.refusal.Message)
	}
	if third.refusal.Result.Status != ToolResultRefused || third.refusal.ActionCallCode != RefusedLoopCallRepeat {
		t.Fatalf("3rd refusal = %+v", third.refusal)
	}
	if third.refusal.DuplicateOfEntryID != second.entry.ID || third.refusal.DuplicateOfActionCallID != second.entry.ActionCallID ||
		third.refusal.Guard.PriorEntryID != second.entry.ID || third.refusal.PriorStep != 2 {
		t.Fatalf("3rd prior = %+v, want entry %s", third.refusal, second.entry.ID)
	}
	if third.note != "" {
		t.Fatalf("refused call got a note")
	}
	fourth := sim.one(4, read)
	expectRefused(t, "4th", fourth, repeatRefuse, RefusedLoopCallRepeat, 4)
	if !strings.Contains(fourth.refusal.Message, "already made 3 times") || !strings.Contains(fourth.refusal.Message, "last at step 2") {
		t.Fatalf("4th text = %q", fourth.refusal.Message)
	}
	outcomes, failed := sim.step(5, false, read)
	if !failed {
		t.Fatalf("5th identical call did not fail the Run")
	}
	expectRefused(t, "5th", outcomes[0], repeatFailRun, RefusedLoopCallRepeatLimit, 5)
	if want := "Not run: this exact call has now been repeated 5 times without progress, so this run is stopping."; outcomes[0].refusal.Message != want {
		t.Fatalf("5th text = %q", outcomes[0].refusal.Message)
	}
	if outcomes[0].refusal.ActionCallCode != RefusedLoopCallRepeatLimit {
		t.Fatalf("5th refusal gets the repeat_limit row: %+v", outcomes[0].refusal)
	}
	failure := repeatLimitRunFailure()
	if failure.Kind != RunFailureKindRepeatLimit || failure.Code != RunFailureCodeRepeatLimit || !failure.Explainable {
		t.Fatalf("failure = %+v", failure)
	}
}

func TestRepeatReadChangedResultRestartsStreak(t *testing.T) {
	sim := newRepeatSim(t)
	poll := func(result string) simCall { return simCall{subject: linearGet("LIN-1"), result: result} }
	expectRun(t, "1st", sim.one(1, poll("queued")))
	second := sim.one(2, poll("running"))
	expectRun(t, "2nd", second)
	if second.note != "" {
		t.Fatalf("changed result got a note: %q", second.note)
	}
	expectRun(t, "3rd", sim.one(3, poll("done")))
	third := sim.one(4, poll("done"))
	expectRun(t, "4th", third)
	if !strings.Contains(third.note, "at step 3") {
		t.Fatalf("note = %q", third.note)
	}
	expectRefused(t, "5th", sim.one(5, poll("done")), repeatRefuse, RefusedLoopCallRepeat, 3)
}

func TestRepeatWriteResetsReadStreak(t *testing.T) {
	sim := newRepeatSim(t)
	read := simCall{subject: linearGet("LIN-2"), result: "open"}
	expectRun(t, "1st", sim.one(1, read))
	expectRun(t, "2nd", sim.one(2, read))
	expectRun(t, "write", sim.one(3, simCall{subject: slackSend("hi"), result: "sent"}))
	after := sim.one(4, read)
	expectRun(t, "read after write", after)
	if after.note != "" {
		t.Fatalf("read after a write got a note: %q", after.note)
	}
	// A failed write is not a reset point.
	expectRun(t, "2nd after", sim.one(5, read))
	expectRun(t, "failed write", sim.one(6, simCall{subject: slackSend("other"), failed: true, result: "rate limited"}))
	expectRefused(t, "3rd after", sim.one(7, read), repeatRefuse, RefusedLoopCallRepeat, 3)
}

func TestRepeatWriteDuplicateSuppressedWithPriorResult(t *testing.T) {
	sim := newRepeatSim(t)
	send := simCall{subject: slackSend("deploy done"), result: "message ts 123"}
	first := sim.one(2, send)
	expectRun(t, "1st", first)
	sim.user("send it again please")
	second := sim.one(3, send)
	expectRefused(t, "2nd", second, repeatSuppressWrite, RefusedLoopCallDuplicateSuppressed, 2)
	result := second.refusal.Result
	if result.Status != ToolResultDuplicateSuppressed || len(result.Content) != 2 {
		t.Fatalf("suppressed result = %+v", result)
	}
	if want := "Not run: this exact call already succeeded at step 2, so it was not repeated. If it really has to happen again, tell the user; a new request can do it. Its result was:"; result.Content[0].Text != want {
		t.Fatalf("suppressed text = %q", result.Content[0].Text)
	}
	if !reflect.DeepEqual(result.Content[1], first.entry.Tool.Result.Content[0]) {
		t.Fatalf("prior result block = %+v", result.Content[1])
	}
	if second.refusal.DuplicateOfEntryID != first.entry.ID || second.refusal.DuplicateOfActionCallID != first.entry.ActionCallID ||
		second.refusal.ActionCallCode != RefusedLoopCallDuplicateSuppressed {
		t.Fatalf("suppressed refusal = %+v", second.refusal)
	}
}

func TestRepeatWriteFifthIdenticalFailsRun(t *testing.T) {
	sim := newRepeatSim(t)
	send := simCall{subject: slackSend("x"), result: "ok"}
	expectRun(t, "1st", sim.one(1, send))
	for n := 2; n <= 4; n++ {
		// Reset points never lift write suppression.
		sim.one(int64(n*10), simCall{subject: slackSend(fmt.Sprintf("other %d", n)), result: "ok"})
		sim.user("again")
		expectRefused(t, fmt.Sprintf("call %d", n), sim.one(int64(n*10+1), send), repeatSuppressWrite, RefusedLoopCallDuplicateSuppressed, n)
	}
	outcomes, failed := sim.step(50, false, send)
	if !failed {
		t.Fatalf("5th identical write did not fail the Run")
	}
	expectRefused(t, "5th", outcomes[0], repeatFailRun, RefusedLoopCallRepeatLimit, 5)
}

func TestRepeatFailedWriteUsesStreakRule(t *testing.T) {
	sim := newRepeatSim(t)
	send := simCall{subject: slackSend("x"), failed: true, result: "channel_not_found"}
	expectRun(t, "1st", sim.one(1, send))
	second := sim.one(2, send)
	expectRun(t, "2nd", second)
	if second.note == "" {
		t.Fatalf("same failed write twice got no note")
	}
	expectRefused(t, "3rd", sim.one(3, send), repeatRefuse, RefusedLoopCallRepeat, 3)

	// A retry that then succeeds becomes suppressible.
	sim2 := newRepeatSim(t)
	expectRun(t, "fail", sim2.one(1, send))
	expectRun(t, "succeed", sim2.one(2, simCall{subject: slackSend("x"), result: "ok"}))
	expectRefused(t, "again", sim2.one(3, send), repeatSuppressWrite, RefusedLoopCallDuplicateSuppressed, 2)
}

func TestRepeatParkedWaitResetsPolling(t *testing.T) {
	sim := newRepeatSim(t)
	check := simCall{subject: linearGet("LIN-3"), result: "pending"}
	wait := simCall{subject: kernelCall("wait", map[string]interface{}{"seconds": 60}), result: "elapsed", parked: true}
	expectRun(t, "check 1", sim.one(1, check))
	outcomes, _ := sim.step(2, true, check)
	if !strings.HasSuffix(outcomes[0].note, " If you are waiting for something to change, use wait before checking again.") {
		t.Fatalf("note with wait offered = %q", outcomes[0].note)
	}
	for step := int64(3); step < 20; step += 2 {
		expectRun(t, "wait", sim.one(step, wait))
		expectRun(t, fmt.Sprintf("check at %d", step+1), sim.one(step+1, check))
	}
	// A wait that did not park (no elapsed wait) is no reset point.
	sim2 := newRepeatSim(t)
	expectRun(t, "a", sim2.one(1, check))
	expectRun(t, "b", sim2.one(2, check))
	expectRun(t, "unparked wait", sim2.one(3, simCall{subject: wait.subject, result: "elapsed"}))
	expectRefused(t, "c", sim2.one(4, check), repeatRefuse, RefusedLoopCallRepeat, 3)
}

func TestRepeatUserEntryResetsReadsNotWrites(t *testing.T) {
	sim := newRepeatSim(t)
	read := simCall{subject: linearGet("LIN-4"), result: "x"}
	send := simCall{subject: slackSend("x"), result: "ok"}
	expectRun(t, "read 1", sim.one(1, read))
	expectRun(t, "read 2", sim.one(2, read))
	expectRun(t, "send", sim.one(3, send))
	expectRun(t, "read 3", sim.one(4, read))
	expectRun(t, "read 4", sim.one(5, read))
	sim.user("try again")
	expectRun(t, "read after user", sim.one(6, read))
	expectRefused(t, "send after user", sim.one(7, send), repeatSuppressWrite, RefusedLoopCallDuplicateSuppressed, 2)
}

func browserClick() RepeatSubject {
	return governedCall("live-browser", "click", capability.SideEffectWrite, true, map[string]interface{}{"selector": "a.next"})
}

func browserSnapshot() RepeatSubject {
	return governedCall("live-browser", "snapshot", capability.SideEffectRead, false, map[string]interface{}{})
}

func TestRepeatBrowserClickPaginationNotRefused(t *testing.T) {
	sim := newRepeatSim(t)
	if browserClick().Effect != RepeatEffectStateful {
		t.Fatalf("repeatable click effect = %s", browserClick().Effect)
	}
	step := int64(0)
	for page := 1; page <= 8; page++ {
		step++
		expectRun(t, fmt.Sprintf("click %d", page), sim.one(step, simCall{subject: browserClick(), result: "clicked"}))
		step++
		expectRun(t, fmt.Sprintf("snapshot %d", page), sim.one(step, simCall{subject: browserSnapshot(), result: fmt.Sprintf("page %d", page+1)}))
	}
}

func TestRepeatBrowserStuckClickRefused(t *testing.T) {
	sim := newRepeatSim(t)
	click := simCall{subject: browserClick(), result: "clicked"}
	snapshot := simCall{subject: browserSnapshot(), result: "page 1"}
	var refused *simOutcome
	for step := int64(1); step <= 12 && refused == nil; step += 2 {
		outcome := sim.one(step, click)
		if outcome.verdict.Kind != repeatRun {
			refused = &outcome
			break
		}
		sim.one(step+1, snapshot)
	}
	if refused == nil || refused.verdict.Code != RefusedLoopCallRepeat {
		t.Fatalf("a click that never changes the page was not refused: %+v", refused)
	}
}

func TestRepeatRunCommandAfterEditRuns(t *testing.T) {
	sim := newRepeatSim(t)
	test := simCall{subject: hostCall(workspace.OperationRunCommand, map[string]interface{}{"command": "go test ./..."}), failed: true, result: "FAIL"}
	edit := func(n int) simCall {
		return simCall{subject: hostCall(workspace.OperationEditFile, map[string]interface{}{"path": "a.go", "edit": n}), result: "edited"}
	}
	if test.subject.Effect != RepeatEffectStateful {
		t.Fatalf("run_command effect = %s", test.subject.Effect)
	}
	for n := 1; n <= 6; n++ {
		expectRun(t, fmt.Sprintf("test %d", n), sim.one(int64(2*n), test))
		expectRun(t, fmt.Sprintf("edit %d", n), sim.one(int64(2*n+1), edit(n)))
	}
}

func TestRepeatRunCommandSameOutputRefused(t *testing.T) {
	sim := newRepeatSim(t)
	test := simCall{subject: hostCall(workspace.OperationRunCommand, map[string]interface{}{"command": "go test ./..."}), failed: true, result: "FAIL"}
	expectRun(t, "1st", sim.one(1, test))
	if second := sim.one(2, test); second.note == "" {
		t.Fatalf("same output twice got no note")
	}
	expectRefused(t, "3rd", sim.one(3, test), repeatRefuse, RefusedLoopCallRepeat, 3)
}

func TestRepeatSameStepDuplicateNotCounted(t *testing.T) {
	sim := newRepeatSim(t)
	read := simCall{subject: linearGet("LIN-5"), result: "x"}
	outcomes, failed := sim.step(1, false, read, read, read)
	if failed {
		t.Fatalf("same-step duplicates failed the Run")
	}
	expectRun(t, "first of step", outcomes[0])
	for i := 1; i < 3; i++ {
		expectRefused(t, "duplicate", outcomes[i], repeatRefuse, ToolGuardSameStepDuplicate, 0)
		if want := "Not run: this is the same call as call_1_0 earlier in this step. Use its result."; outcomes[i].refusal.Message != want {
			t.Fatalf("same-step text = %q", outcomes[i].refusal.Message)
		}
		if outcomes[i].refusal.ActionCallCode != "" || outcomes[i].refusal.DuplicateOfEntryID != outcomes[0].entry.ID {
			t.Fatalf("same-step refusal = %+v", outcomes[i].refusal)
		}
	}
	// Only the executed call counts: the next step is the 2nd identical call.
	second := sim.one(2, read)
	expectRun(t, "next step", second)
	if second.note == "" {
		t.Fatalf("2nd counted call got no note")
	}
}

func TestRepeatInterruptedNotCounted(t *testing.T) {
	sim := newRepeatSim(t)
	read := simCall{subject: linearGet("LIN-6"), result: "x"}
	expectRun(t, "1", sim.one(1, read))
	expectRun(t, "interrupted", sim.one(2, simCall{subject: read.subject, interrupted: true}))
	expectRun(t, "interrupted again", sim.one(3, simCall{subject: read.subject, interrupted: true}))
	expectRun(t, "2", sim.one(4, read))
	expectRefused(t, "3", sim.one(5, read), repeatRefuse, RefusedLoopCallRepeat, 3)
}

func TestRepeatControlToolsNotCounted(t *testing.T) {
	sim := newRepeatSim(t)
	for _, name := range []string{"wait", "turn_report_progress", "load_capabilities", "turn_final_answer"} {
		call := simCall{subject: kernelCall(name, map[string]interface{}{"x": 1}), result: "ok"}
		for step := int64(1); step <= 6; step++ {
			outcome := sim.one(step, call)
			expectRun(t, name, outcome)
			if outcome.note != "" {
				t.Fatalf("%s got a note", name)
			}
		}
		outcomes, _ := sim.step(7, false, call, call)
		expectRun(t, name+" same step", outcomes[1])
	}
	card := governedCall(SkillManagementSkillID, SkillActionRequestCredential, capability.SideEffectWrite, false, map[string]interface{}{"x": 1})
	if card.Effect != RepeatEffectControl {
		t.Fatalf("card effect = %s", card.Effect)
	}
	for step := int64(10); step <= 15; step++ {
		expectRun(t, "card", sim.one(step, simCall{subject: card, result: "ok"}))
	}
}

func TestRepeatFailRunStopsSiblings(t *testing.T) {
	sim := newRepeatSim(t)
	read := simCall{subject: linearGet("LIN-7"), result: "x"}
	send := simCall{subject: slackSend("done"), result: "ok"}
	expectRun(t, "send", sim.one(1, send))
	for step := int64(2); step <= 5; step++ {
		sim.one(step, read)
	}
	other := simCall{subject: linearGet("LIN-8"), result: "y"}
	wait := simCall{subject: kernelCall("wait", nil), result: "ok"}
	outcomes, failed := sim.step(6, false, other, read, send, wait)
	if !failed {
		t.Fatalf("5th identical read did not fail the Run")
	}
	expectRefused(t, "sibling read", outcomes[0], repeatRefuse, ToolGuardRunStopping, 0)
	expectRefused(t, "failing", outcomes[1], repeatFailRun, RefusedLoopCallRepeatLimit, 5)
	expectRefused(t, "suppressed sibling", outcomes[2], repeatSuppressWrite, RefusedLoopCallDuplicateSuppressed, 2)
	expectRefused(t, "control sibling", outcomes[3], repeatRefuse, ToolGuardRunStopping, 0)
	if outcomes[0].refusal.Message != "Not run: this run is stopping because a call was repeated without progress." ||
		outcomes[0].refusal.ActionCallCode != "" || outcomes[0].refusal.DuplicateOfEntryID != "" {
		t.Fatalf("run_stopping refusal = %+v", outcomes[0].refusal)
	}
	// run_stopping entries are not counted: LIN-8 is still a 1st call.
	sim2Entries := sim.entries()
	ledger, err := loadRepeatLedger(context.Background(), &fakeSessionEntryLister{entries: sim2Entries}, sim.scope, "session-1", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	key, _ := repeatKey(other.subject)
	if verdict := ledger.evaluate(other.subject, key, nil); verdict.Kind != repeatRun {
		t.Fatalf("call refused as run_stopping was counted: %+v", verdict)
	}
}

// repeatScenario builds a mixed log through observe.
func repeatScenario(t *testing.T) *repeatSim {
	sim := newRepeatSim(t)
	read := simCall{subject: linearGet("LIN-9"), result: "x"}
	sim.one(1, read)
	sim.one(2, read)
	sim.one(3, simCall{subject: slackSend("a"), result: "ok"})
	sim.one(4, read)
	sim.one(5, read)
	sim.step(6, false, read, simCall{subject: slackSend("a"), result: "ok"})
	sim.user("again")
	sim.one(7, simCall{subject: browserClick(), result: "clicked"})
	sim.one(8, simCall{subject: browserSnapshot(), result: "p1"})
	sim.one(9, simCall{subject: browserClick(), result: "clicked"})
	return sim
}

func TestRepeatLedgerLoadMatchesIncrementalObserve(t *testing.T) {
	sim := repeatScenario(t)
	loaded, err := loadRepeatLedger(context.Background(), &fakeSessionEntryLister{entries: sim.entries()}, sim.scope, "session-1", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.entries) != len(sim.ledger.entries) {
		t.Fatalf("loaded %d entries, observed %d", len(loaded.entries), len(sim.ledger.entries))
	}
	for i := range loaded.entries {
		if loaded.entries[i] != sim.ledger.entries[i] {
			t.Fatalf("entry %d differs: %s vs %s", i, loaded.entries[i].ID, sim.ledger.entries[i].ID)
		}
	}
	for _, subject := range []RepeatSubject{linearGet("LIN-9"), slackSend("a"), browserClick(), browserSnapshot(), linearGet("new")} {
		key, _ := repeatKey(subject)
		if a, b := loaded.evaluate(subject, key, nil), sim.ledger.evaluate(subject, key, nil); !reflect.DeepEqual(a, b) {
			t.Fatalf("%s: loaded %+v, observed %+v", subject.Identity, a, b)
		}
	}
}

func TestRepeatLedgerPagesPastOneThousandEntries(t *testing.T) {
	sim := newRepeatSim(t)
	read := simCall{subject: linearGet("LIN-10"), result: "x"}
	sim.one(1, read)
	sim.one(2, read)
	for i := 0; i < 1200; i++ {
		sim.one(int64(10+i), simCall{subject: linearGet(fmt.Sprintf("other-%d", i)), result: "y"})
	}
	store := &fakeSessionEntryLister{entries: sim.entries()}
	ledger, err := loadRepeatLedger(context.Background(), store, sim.scope, "session-1", "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if store.calls < 2 || len(ledger.entries) != 1202 {
		t.Fatalf("calls %d, entries %d", store.calls, len(ledger.entries))
	}
	key, _ := repeatKey(read.subject)
	if verdict := ledger.evaluate(read.subject, key, nil); verdict.Kind != repeatRefuse || verdict.Count != 3 {
		t.Fatalf("verdict after paging = %+v", verdict)
	}
}

func TestRepeatTimes(t *testing.T) {
	if repeatTimes(2) != "twice" || repeatTimes(3) != "3 times" || repeatTimes(5) != "5 times" {
		t.Fatalf("repeatTimes")
	}
}
