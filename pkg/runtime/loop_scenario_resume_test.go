package runtime

// Crash-resume journeys (run-loop section 12 §5.7, D14). Each subtest blocks
// or traps worker 1 at one point of a claim, kills it (crash: every later
// write of that worker is refused, no cleanup, no lease release), and lets
// worker 2 drive the Run to rest. What is asserted is what users and the
// model see: how many times an action was dispatched, the session log, the
// action_calls rows and the Run status.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// resumeTrapStore sits over the fixture's crash store and kills the worker
// at an exact write: when trap returns true for a write, the crash store is
// armed and that write and every later one are refused. The trap fires at
// most once.
type resumeTrapStore struct {
	*looptest.CrashStore
	mu    sync.Mutex
	trap  func(method string, arg any) bool
	fired bool
	// fail, when set, makes a write return its error without a crash (the
	// worker lives on).
	fail func(method string, arg any) error
}

// installResumeTrap puts a trap store under the fixture's worker-bound
// services. crash() rebuilds them over the same trap store.
func installResumeTrap(f *looptest.Fixture, trap func(method string, arg any) bool) *resumeTrapStore {
	f.T.Helper()
	s := &resumeTrapStore{CrashStore: f.Crashes, trap: trap}
	f.Store = s
	f.Build()
	return s
}

func (s *resumeTrapStore) didFire() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fired
}

func (s *resumeTrapStore) hit(method string, arg any) error {
	if err := s.CrashStore.Refused(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		if err := s.fail(method, arg); err != nil {
			return err
		}
	}
	if s.fired || s.trap == nil || !s.trap(method, arg) {
		return nil
	}
	s.fired = true
	s.CrashStore.Arm()
	return looptest.ErrSimulatedCrash
}

var _ KernelStore = (*resumeTrapStore)(nil)
var _ AgentSessionStore = (*resumeTrapStore)(nil)

func (s *resumeTrapStore) OpenConversationAgentSession(ctx context.Context, req OpenConversationAgentSessionRequest) (*OpenAgentSessionResult, error) {
	if err := s.hit("OpenConversationAgentSession", req); err != nil {
		return nil, err
	}
	return s.CrashStore.OpenConversationAgentSession(ctx, req)
}

func (s *resumeTrapStore) CreateRunAgentSession(ctx context.Context, req CreateRunAgentSessionRequest) (*OpenAgentSessionResult, error) {
	if err := s.hit("CreateRunAgentSession", req); err != nil {
		return nil, err
	}
	return s.CrashStore.CreateRunAgentSession(ctx, req)
}

func (s *resumeTrapStore) AppendAgentSessionEntries(ctx context.Context, req AppendAgentSessionEntriesRequest) (*AppendAgentSessionEntriesResult, error) {
	if err := s.hit("AppendAgentSessionEntries", req); err != nil {
		return nil, err
	}
	return s.CrashStore.AppendAgentSessionEntries(ctx, req)
}

func (s *resumeTrapStore) TransitionToolEntry(ctx context.Context, req ToolEntryTransitionRequest) (*ToolEntryTransitionResult, error) {
	if err := s.hit("TransitionToolEntry", req); err != nil {
		return nil, err
	}
	return s.CrashStore.TransitionToolEntry(ctx, req)
}

func (s *resumeTrapStore) CreateAgentRun(ctx context.Context, run *AgentRun) error {
	if err := s.hit("CreateAgentRun", run); err != nil {
		return err
	}
	return s.CrashStore.CreateAgentRun(ctx, run)
}

func (s *resumeTrapStore) CreateAgentRunWithEvent(ctx context.Context, run *AgentRun, event *ActivityEvent) (*ActivityEvent, error) {
	if err := s.hit("CreateAgentRunWithEvent", run); err != nil {
		return nil, err
	}
	return s.CrashStore.CreateAgentRunWithEvent(ctx, run, event)
}

func (s *resumeTrapStore) UpdateAgentRunWithEvent(ctx context.Context, run *AgentRun, expectedRevision int64, event *ActivityEvent, lease *AgentRunLeaseGuard) (*ActivityEvent, error) {
	if err := s.hit("UpdateAgentRunWithEvent", run); err != nil {
		return nil, err
	}
	return s.CrashStore.UpdateAgentRunWithEvent(ctx, run, expectedRevision, event, lease)
}

func (s *resumeTrapStore) AppendActivity(ctx context.Context, event *ActivityEvent) (*ActivityEvent, error) {
	if err := s.hit("AppendActivity", event); err != nil {
		return nil, err
	}
	return s.CrashStore.AppendActivity(ctx, event)
}

func (s *resumeTrapStore) ClaimNextAgentRun(ctx context.Context, claim AgentRunClaim) (*AgentRun, error) {
	if err := s.hit("ClaimNextAgentRun", claim); err != nil {
		return nil, err
	}
	return s.CrashStore.ClaimNextAgentRun(ctx, claim)
}

func (s *resumeTrapStore) RenewAgentRunLease(ctx context.Context, scope Scope, runID, workerID string, now time.Time, leaseDuration time.Duration) (*AgentRun, error) {
	if err := s.hit("RenewAgentRunLease", runID); err != nil {
		return nil, err
	}
	return s.CrashStore.RenewAgentRunLease(ctx, scope, runID, workerID, now, leaseDuration)
}

func (s *resumeTrapStore) CreateAgentTurn(ctx context.Context, turn *AgentTurn) (*AgentTurn, error) {
	if err := s.hit("CreateAgentTurn", turn); err != nil {
		return nil, err
	}
	return s.CrashStore.CreateAgentTurn(ctx, turn)
}

func (s *resumeTrapStore) UpdateAgentTurn(ctx context.Context, turn *AgentTurn, expectedRevision int64, workerID string) error {
	if err := s.hit("UpdateAgentTurn", turn); err != nil {
		return err
	}
	return s.CrashStore.UpdateAgentTurn(ctx, turn, expectedRevision, workerID)
}

func (s *resumeTrapStore) ClaimAgentTurn(ctx context.Context, scope Scope, turnID, workerID string, now time.Time, leaseDuration time.Duration) (*AgentTurn, error) {
	if err := s.hit("ClaimAgentTurn", turnID); err != nil {
		return nil, err
	}
	return s.CrashStore.ClaimAgentTurn(ctx, scope, turnID, workerID, now, leaseDuration)
}

func (s *resumeTrapStore) RecordAgentTurnStepModel(ctx context.Context, turn *AgentTurn, expectedRevision int64, events []*ActivityEvent) error {
	if err := s.hit("RecordAgentTurnStepModel", turn); err != nil {
		return err
	}
	return s.CrashStore.RecordAgentTurnStepModel(ctx, turn, expectedRevision, events)
}

func (s *resumeTrapStore) CreateActionProposal(ctx context.Context, proposal ActionProposalRecord) (*ActionProposalResult, error) {
	if err := s.hit("CreateActionProposal", proposal); err != nil {
		return nil, err
	}
	return s.CrashStore.CreateActionProposal(ctx, proposal)
}

func (s *resumeTrapStore) ResolveApproval(ctx context.Context, resolution ApprovalResolutionRecord) (*ApprovalResolutionResult, error) {
	if err := s.hit("ResolveApproval", resolution); err != nil {
		return nil, err
	}
	return s.CrashStore.ResolveApproval(ctx, resolution)
}

func (s *resumeTrapStore) ClaimNextAction(ctx context.Context, claim ActionClaim) (*ActionCall, error) {
	if err := s.hit("ClaimNextAction", claim); err != nil {
		return nil, err
	}
	return s.CrashStore.ClaimNextAction(ctx, claim)
}

func (s *resumeTrapStore) RenewActionLease(ctx context.Context, scope Scope, actionID, workerID string, now time.Time, leaseDuration time.Duration) (*ActionCall, error) {
	if err := s.hit("RenewActionLease", actionID); err != nil {
		return nil, err
	}
	return s.CrashStore.RenewActionLease(ctx, scope, actionID, workerID, now, leaseDuration)
}

func (s *resumeTrapStore) PersistActionExecution(ctx context.Context, execution ActionExecutionRecord) (*ActionExecutionResult, error) {
	if err := s.hit("PersistActionExecution", execution); err != nil {
		return nil, err
	}
	return s.CrashStore.PersistActionExecution(ctx, execution)
}

// ---------------------------------------------------------------------------
// Helpers

// resumeStartClaim runs one claim of the Run in a goroutine; done closes when it
// returns.
func resumeStartClaim(f *looptest.Fixture, runID string) (context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.RunClaimContext(ctx, runID)
	}()
	return cancel, done
}

// resumeWait waits for a signal from the blocked claim.
func resumeWait(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// resumeEventually polls a store condition the blocked claim reaches.
func resumeEventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// resumeDispatchesWithID counts dispatches of action whose id argument is id.
func resumeDispatchesWithID(d *looptest.FakeDispatcher, action, id string) int {
	d.Mu.Lock()
	defer d.Mu.Unlock()
	n := 0
	for _, in := range d.Calls {
		if in.Bound != nil && in.Bound.Action.Name == action && in.Arguments["id"] == id {
			n++
		}
	}
	return n
}

// resumeToolEntries is the Run's tool entries whose name ends with action.
func resumeToolEntries(f *looptest.Fixture, runID, action string) []*SessionEntry {
	f.T.Helper()
	var out []*SessionEntry
	for _, e := range f.Entries(runID) {
		if e.Type == SessionEntryTool && e.Tool != nil && strings.HasSuffix(e.Tool.Name, action) {
			out = append(out, e)
		}
	}
	return out
}

// resumeActionCalls is the Run's action calls of action.
func resumeActionCalls(f *looptest.Fixture, runID, action string) []*ActionCall {
	f.T.Helper()
	var out []*ActionCall
	for _, c := range f.Calls(runID) {
		if c.Action == action {
			out = append(out, c)
		}
	}
	return out
}

func resumeEntryText(e *SessionEntry) string {
	if e == nil || e.Tool == nil || e.Tool.Result == nil {
		return ""
	}
	return looptest.PartsText(e.Tool.Result.Content)
}

func resumeLog(f *looptest.Fixture, runID string) string {
	return strings.Join(f.EntryTypes(runID), ",")
}

func resumeMustLog(t *testing.T, f *looptest.Fixture, runID, want string) {
	t.Helper()
	if got := resumeLog(f, runID); got != want {
		t.Fatalf("log = %s, want %s", got, want)
	}
}

func resumeHasToolResult(v looptest.StepView, action string) bool {
	_, _, ok := v.ToolResult(looptest.GovernedTool(v, action))
	return ok
}

// sendRules: step 1 sends, then the model answers from what the send
// returned.
func sendRules() []*looptest.ModelRule {
	return []*looptest.ModelRule{
		{Name: "send", When: func(v looptest.StepView) bool { return !resumeHasToolResult(v, "send") },
			Reply: func(v looptest.StepView) ModelStepResult {
				return looptest.Act("Sending it.", looptest.ReviewedCall(looptest.GovernedTool(v, "send"), map[string]any{"to": "bo", "text": "hi"}, "Send hi to bo"))
			}},
		{Name: "answer", Reply: func(v looptest.StepView) ModelStepResult {
			content, isError, _ := v.ToolResult(looptest.GovernedTool(v, "send"))
			if isError {
				return looptest.FinalAnswer(map[string]any{"answer": "I may have sent it; please check: " + content})
			}
			return looptest.FinalAnswer(map[string]any{"answer": "Sent."})
		}},
	}
}

// ---------------------------------------------------------------------------
// Scenarios

// resumeScenario runs one crash-resume subtest on a fresh store of every kind.
func resumeScenario(t *testing.T, name string, test func(t *testing.T, store KernelStore)) {
	t.Run(name, func(t *testing.T) { looptest.EachStore(t, "loop_resume_"+name, test) })
}

func TestLoopResumeAfterCrash(t *testing.T) {
	resumeScenario(t, "assistant_missing_redoes_model_call", func(t *testing.T, store KernelStore) {
		entered := make(chan struct{})
		var once sync.Once
		f := looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "lookup", When: func(v looptest.StepView) bool { return v.Step() == 1 },
				Reply: func(v looptest.StepView) ModelStepResult {
					return looptest.Act("Looking it up.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "1"}))
				}},
			&looptest.ModelRule{Name: "blocked", Times: 1, When: func(v looptest.StepView) bool { return v.Step() == 2 },
				Before: func(ctx context.Context, _ looptest.StepView) error {
					once.Do(func() { close(entered) })
					<-ctx.Done()
					return ctx.Err()
				},
				Reply: func(looptest.StepView) ModelStepResult { return looptest.Say("never") }},
			&looptest.ModelRule{Name: "answer", When: func(v looptest.StepView) bool { return v.Step() == 2 },
				Reply: func(looptest.StepView) ModelStepResult {
					return looptest.FinalAnswer(map[string]any{"answer": "Issue 1 is open."})
				}},
		))
		run := f.StartRun(RunKindAgentWork, "Check issue 1")
		cancel, done := resumeStartClaim(f, run.ID)
		resumeWait(t, entered, "step 2's model call")
		f.Crash(cancel, done)

		final := f.DriveToRest(run.ID, 4)
		f.MustStatus(final, AgentRunStatusCompleted)
		requests := f.Model.Calls(ModelStepPurposeStep)
		if len(requests) != 3 {
			t.Fatalf("model step requests = %d, want 3 (step 1, step 2 unanswered, step 2 again)", len(requests))
		}
		if v := (looptest.StepView{Req: requests[1]}); v.Step() != 2 || (looptest.StepView{Req: requests[2]}).Step() != 2 {
			t.Fatalf("redo requests are not both step 2: %s / %s", v.Describe(), looptest.StepView{Req: requests[2]}.Describe())
		}
		if requests[1].StepID != requests[2].StepID {
			t.Fatalf("redo used step %s, the crashed call step %s: the running step row is reused", requests[2].StepID, requests[1].StepID)
		}
		resumeMustLog(t, f, run.ID, "user,assistant,tool:completed,assistant,tool:completed")
		if steps := f.Steps(run.ID); len(steps) != 2 {
			t.Fatalf("step rows = %d, want 2", len(steps))
		}
		if f.Dispatch.Count("lookup") != 1 {
			t.Fatalf("lookup dispatched %d times, want 1", f.Dispatch.Count("lookup"))
		}
	})

	resumeScenario(t, "read_tool_pending_runs", func(t *testing.T, store KernelStore) {
		entered := make(chan struct{})
		unblock := make(chan struct{})
		var blockOnce sync.Once
		f := looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "lookups", When: func(v looptest.StepView) bool { return v.Step() == 1 },
				Reply: func(v looptest.StepView) ModelStepResult {
					name := looptest.GovernedTool(v, "lookup")
					return looptest.Act("Looking both up.", looptest.Call(name, map[string]any{"id": "1"}), looptest.Call(name, map[string]any{"id": "2"}))
				}},
			&looptest.ModelRule{Name: "answer", Reply: func(v looptest.StepView) ModelStepResult {
				return looptest.FinalAnswer(map[string]any{"answer": fmt.Sprintf("%d issues looked up.", len(v.ToolResultsSinceUser()))})
			}},
		))
		f.Dispatch.On("lookup", func(in agentrun.ActionDispatchInput) (map[string]interface{}, error) {
			id, _ := in.Arguments["id"].(string)
			blocked := false
			if id == "2" {
				blockOnce.Do(func() { blocked = true })
			}
			if blocked {
				close(entered)
				<-unblock
				return nil, context.Canceled
			}
			return map[string]interface{}{"id": id, "status": "open", "title": "Issue " + id}, nil
		})
		run := f.StartRun(RunKindAgentWork, "Check issues 1 and 2")
		cancel, done := resumeStartClaim(f, run.ID)
		resumeWait(t, entered, "lookup{2} dispatch")
		resumeEventually(t, "lookup{1} to succeed", func() bool {
			for _, c := range resumeActionCalls(f, run.ID, "lookup") {
				if c.Arguments["id"] == "1" && c.Status == ActionCallStatusSucceeded {
					return true
				}
			}
			return false
		})
		// Kill the worker before the blocked dispatch returns, so nothing
		// it does afterwards is written.
		f.Crashes.Arm()
		cancel()
		close(unblock)
		f.Crash(nil, done)

		final := f.DriveToRest(run.ID, 4)
		f.MustStatus(final, AgentRunStatusCompleted)
		if n := resumeDispatchesWithID(f.Dispatch, "lookup", "1"); n != 1 {
			t.Fatalf("lookup{1} dispatched %d times, want 1: its finished call is not redispatched", n)
		}
		if n := resumeDispatchesWithID(f.Dispatch, "lookup", "2"); n != 2 {
			t.Fatalf("lookup{2} dispatched %d times, want 2: an interrupted read runs again", n)
		}
		calls := resumeActionCalls(f, run.ID, "lookup")
		if len(calls) != 2 {
			t.Fatalf("lookup calls = %d, want 2", len(calls))
		}
		for _, c := range calls {
			if c.Status != ActionCallStatusSucceeded {
				t.Fatalf("lookup{%v} call status = %s, want succeeded", c.Arguments["id"], c.Status)
			}
			if recovered := c.Arguments["id"] == "2"; c.RecoveredRunning != recovered {
				t.Fatalf("lookup{%v} RecoveredRunning = %v, want %v", c.Arguments["id"], c.RecoveredRunning, recovered)
			}
		}
		resumeMustLog(t, f, run.ID, "user,assistant,tool:completed,tool:completed,assistant,tool:completed")
		for _, e := range resumeToolEntries(f, run.ID, "lookup") {
			if !strings.Contains(resumeEntryText(e), "Issue ") {
				t.Fatalf("lookup entry %s result = %s", e.ID, resumeEntryText(e))
			}
		}
	})

	resumeScenario(t, "write_running_becomes_interrupted", func(t *testing.T, store KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(sendRules()...))
		release := f.Dispatch.Hold("send")
		defer close(release)
		run := f.StartRun(RunKindAgentWork, "Send hi to bo")
		cancel, done := resumeStartClaim(f, run.ID)
		resumeEventually(t, "send dispatch", func() bool { return f.Dispatch.Count("send") == 1 })
		f.Crash(cancel, done)

		final := f.DriveToRest(run.ID, 4)
		f.MustStatus(final, AgentRunStatusCompleted)
		if n := f.Dispatch.Count("send"); n != 1 {
			t.Fatalf("send dispatched %d times, want 1 (the blocked one): a write whose outcome is unknown is never sent again", n)
		}
		resumeMustLog(t, f, run.ID, "user,assistant,tool:interrupted,assistant,tool:completed")
		entries := resumeToolEntries(f, run.ID, "send")
		if len(entries) != 1 || !strings.Contains(resumeEntryText(entries[0]), "may already have taken effect") {
			t.Fatalf("send entry = %v", entries)
		}
		calls := resumeActionCalls(f, run.ID, "send")
		if len(calls) != 1 {
			t.Fatalf("send calls = %d, want 1", len(calls))
		}
		c := calls[0]
		if c.Status != ActionCallStatusFailed || c.ErrorCode != ActionErrorCodeInterrupted || !c.RecoveredRunning || c.FailurePhase != ActionFailureAfterDispatch {
			t.Fatalf("send call = status %s code %q recovered %v phase %q, want failed interrupted recovered after_dispatch", c.Status, c.ErrorCode, c.RecoveredRunning, c.FailurePhase)
		}
		last := looptest.StepView{Req: f.Model.Calls(ModelStepPurposeStep)[len(f.Model.Calls(ModelStepPurposeStep))-1]}
		if _, isError, ok := last.ToolResult(looptest.GovernedTool(last, "send")); !ok || !isError {
			t.Fatalf("the model's next step did not see the interrupted send as an error: %s", last.Describe())
		}
		if answer := resumeFinalAnswer(f, run.ID); !strings.Contains(answer, "I may have sent it; please check") {
			t.Fatalf("final answer = %q", answer)
		}
	})

	resumeScenario(t, "write_pending_without_call_is_proposed", func(t *testing.T, store KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(sendRules()...))
		trap := installResumeTrap(f, func(method string, _ any) bool { return method == "CreateActionProposal" })
		run := f.StartRun(RunKindAgentWork, "Send hi to bo")
		cancel, done := resumeStartClaim(f, run.ID)
		resumeWait(t, done, "the trapped claim")
		if !trap.didFire() {
			t.Fatal("the claim never proposed the send")
		}
		// The entry is open (the executor marks it running before it
		// proposes) and no action call was made for it.
		resumeMustLog(t, f, run.ID, "user,assistant,tool:running")
		if entries := resumeToolEntries(f, run.ID, "send"); len(entries) != 1 || entries[0].ActionCallID != "" {
			t.Fatalf("send entry at the crash = %#v, want one with no action call", entries)
		}
		if calls := f.Calls(run.ID); len(calls) != 0 {
			t.Fatalf("action calls at the crash = %d, want 0", len(calls))
		}
		f.Crash(cancel, done)

		final := f.DriveToRest(run.ID, 4)
		f.MustStatus(final, AgentRunStatusCompleted)
		if n := f.Dispatch.Count("send"); n != 1 {
			t.Fatalf("send dispatched %d times, want 1", n)
		}
		calls := resumeActionCalls(f, run.ID, "send")
		if len(calls) != 1 || calls[0].Status != ActionCallStatusSucceeded || calls[0].RecoveredRunning {
			t.Fatalf("send calls = %#v, want one succeeded call proposed on resume", calls)
		}
		resumeMustLog(t, f, run.ID, "user,assistant,tool:completed,assistant,tool:completed")
		if answer := resumeFinalAnswer(f, run.ID); answer != "Sent." {
			t.Fatalf("final answer = %q", answer)
		}
	})

	resumeScenario(t, "finished_call_appends_result", func(t *testing.T, store KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(sendRules()...))
		trap := installResumeTrap(f, func(method string, arg any) bool {
			req, ok := arg.(ToolEntryTransitionRequest)
			return ok && method == "TransitionToolEntry" && req.To == ToolEntryCompleted
		})
		run := f.StartRun(RunKindAgentWork, "Send hi to bo")
		cancel, done := resumeStartClaim(f, run.ID)
		resumeWait(t, done, "the trapped claim")
		if !trap.didFire() {
			t.Fatal("the send entry never completed")
		}
		if calls := resumeActionCalls(f, run.ID, "send"); len(calls) != 1 || calls[0].Status != ActionCallStatusSucceeded {
			t.Fatalf("send calls at the crash = %#v, want one succeeded", calls)
		}
		if entries := resumeToolEntries(f, run.ID, "send"); len(entries) != 1 || entries[0].State.Terminal() {
			t.Fatalf("send entry at the crash = %v, want it still open", f.EntryTypes(run.ID))
		}
		f.Crash(cancel, done)

		final := f.DriveToRest(run.ID, 4)
		f.MustStatus(final, AgentRunStatusCompleted)
		if n := f.Dispatch.Count("send"); n != 1 {
			t.Fatalf("send dispatched %d times, want 1", n)
		}
		resumeMustLog(t, f, run.ID, "user,assistant,tool:completed,assistant,tool:completed")
		entries := resumeToolEntries(f, run.ID, "send")
		if len(entries) != 1 || !strings.Contains(resumeEntryText(entries[0]), `"sent":true`) {
			t.Fatalf("send entry result = %q, want the call's output", resumeEntryText(entries[0]))
		}
		if entries[0].ActionCallID != resumeActionCalls(f, run.ID, "send")[0].ID {
			t.Fatalf("send entry action call = %s", entries[0].ActionCallID)
		}
	})

	resumeScenario(t, "completed_step_is_not_redone", func(t *testing.T, store KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "lookup", When: func(v looptest.StepView) bool { return v.Step() == 1 },
				Reply: func(v looptest.StepView) ModelStepResult {
					return looptest.Act("Looking it up.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "4"}))
				}},
			&looptest.ModelRule{Name: "answer", Reply: func(looptest.StepView) ModelStepResult {
				return looptest.FinalAnswer(map[string]any{"answer": "Issue 4 is open."})
			}},
		))
		var rows int
		trap := installResumeTrap(f, func(method string, _ any) bool {
			if method != "CreateAgentTurn" {
				return false
			}
			rows++
			return rows == 2
		})
		run := f.StartRun(RunKindAgentWork, "Check issue 4")
		cancel, done := resumeStartClaim(f, run.ID)
		resumeWait(t, done, "the trapped claim")
		if !trap.didFire() {
			t.Fatal("step 2 never started")
		}
		resumeMustLog(t, f, run.ID, "user,assistant,tool:completed")
		f.Crash(cancel, done)

		final := f.DriveToRest(run.ID, 4)
		f.MustStatus(final, AgentRunStatusCompleted)
		requests := f.Model.Calls(ModelStepPurposeStep)
		stepOne := 0
		for _, req := range requests {
			if (looptest.StepView{Req: req}).Step() == 1 {
				stepOne++
			}
		}
		if stepOne != 1 || len(requests) != 2 {
			t.Fatalf("model step requests = %d with %d for step 1, want 2 with 1", len(requests), stepOne)
		}
		if f.Dispatch.Count("lookup") != 1 {
			t.Fatalf("lookup dispatched %d times, want 1", f.Dispatch.Count("lookup"))
		}
		resumeMustLog(t, f, run.ID, "user,assistant,tool:completed,assistant,tool:completed")
		if steps := f.Steps(run.ID); len(steps) != 2 {
			t.Fatalf("step rows = %d, want 2", len(steps))
		}
	})

	resumeScenario(t, "final_of_failed_step_is_not_run", func(t *testing.T, store KernelStore) {
		// Worker 1 dies after step 1's final_answer is appended; worker 2's
		// resume of step 1 hits a host outage and fails the step, so its
		// final_answer never settles. The next step refuses it as not run
		// and the model answers again.
		sawRefusal := false
		f := looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "first", When: func(v looptest.StepView) bool { return v.Step() == 1 },
				Reply: func(looptest.StepView) ModelStepResult {
					return looptest.FinalAnswer(map[string]any{"answer": "First."})
				}},
			&looptest.ModelRule{Name: "second", Reply: func(v looptest.StepView) ModelStepResult {
				_, isError, ok := v.ToolResult(string(ControlToolFinalAnswer))
				sawRefusal = ok && isError
				return looptest.FinalAnswer(map[string]any{"answer": "Second."})
			}},
		))
		appended := false
		trap := installResumeTrap(f, func(method string, arg any) bool {
			if req, ok := arg.(AppendAgentSessionEntriesRequest); ok {
				for _, e := range req.Entries {
					if e.Type == SessionEntryTool && e.Tool != nil && e.Tool.Name == string(ControlToolFinalAnswer) {
						appended = true
					}
				}
				return false
			}
			return appended && method == "RecordAgentTurnStepModel"
		})
		run := f.StartRun(RunKindAgentWork, "Say something")
		cancel, done := resumeStartClaim(f, run.ID)
		resumeWait(t, done, "the trapped claim")
		if !trap.didFire() {
			t.Fatal("step 1 never recorded its model call")
		}
		f.Crash(cancel, done)
		failed := false
		trap.fail = func(method string, _ any) error {
			if method != "RecordAgentTurnStepModel" || failed {
				return nil
			}
			failed = true
			return fmt.Errorf("record step: %w", modelstep.ErrTurnHostUnavailable)
		}

		final := f.DriveToRest(run.ID, 6)
		f.MustStatus(final, AgentRunStatusCompleted)
		if !failed {
			t.Fatal("the resumed step never failed")
		}
		steps := f.Steps(run.ID)
		if len(steps) != 2 || steps[0].Status != AgentTurnStatusFailed || steps[1].Status != AgentTurnStatusCompleted {
			t.Fatalf("steps = %#v, want failed then completed", steps)
		}
		finals := resumeToolEntries(f, run.ID, string(ControlToolFinalAnswer))
		if len(finals) != 2 || finals[0].State != ToolEntryError || finals[1].State != ToolEntryCompleted {
			t.Fatalf("final_answer entries = %d, log %s", len(finals), resumeLog(f, run.ID))
		}
		if text := resumeEntryText(finals[0]); !strings.Contains(text, "not run") {
			t.Fatalf("step 1's final_answer result = %q, want a not-run refusal", text)
		}
		if !sawRefusal {
			t.Fatal("step 2's model call did not see the refused final_answer")
		}
		if requests := f.Model.Calls(ModelStepPurposeStep); len(requests) != 2 {
			t.Fatalf("model step requests = %d, want 2", len(requests))
		}
		if answer := resumeFinalAnswer(f, run.ID); answer != "Second." {
			t.Fatalf("final answer = %q", answer)
		}
	})

	resumeScenario(t, "approval_answered_while_owner_dead", func(t *testing.T, store KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithApprovalForWrites(), looptest.WithRules(sendRules()...))
		run := f.StartRun(RunKindAgentWork, "Send hi to bo")
		parked := f.DriveToRest(run.ID, 3)
		f.MustStatus(parked, AgentRunStatusWaitingForApproval)
		if parked.LeaseOwner != "" {
			t.Fatalf("parked Run still leased by %s", parked.LeaseOwner)
		}
		f.Crash(nil, nil)
		f.Decide(run.ID, ApprovalDecisionApprove, "go ahead")
		if f.Dispatch.Count("send") != 0 {
			t.Fatal("the approval dispatched the send without a worker")
		}

		final := f.DriveToRest(run.ID, 4)
		f.MustStatus(final, AgentRunStatusCompleted)
		if f.WorkerID != looptest.FixtureWorker2 {
			t.Fatalf("worker = %s, want %s", f.WorkerID, looptest.FixtureWorker2)
		}
		if n := f.Dispatch.Count("send"); n != 1 {
			t.Fatalf("send dispatched %d times, want 1", n)
		}
		calls := resumeActionCalls(f, run.ID, "send")
		if len(calls) != 1 || calls[0].Status != ActionCallStatusSucceeded {
			t.Fatalf("send calls = %#v", calls)
		}
		if answer := resumeFinalAnswer(f, run.ID); answer != "Sent." {
			t.Fatalf("final answer = %q", answer)
		}
	})
}

// resumeFinalAnswer is the answer argument of the Run's final answer call.
func resumeFinalAnswer(f *looptest.Fixture, runID string) string {
	f.T.Helper()
	entries := resumeToolEntries(f, runID, string(ControlToolFinalAnswer))
	if len(entries) == 0 {
		return ""
	}
	var args struct {
		Answer string `json:"answer"`
	}
	if err := json.Unmarshal(entries[len(entries)-1].Tool.Arguments, &args); err != nil {
		f.T.Fatalf("final answer arguments: %v", err)
	}
	return args.Answer
}

// TestLoopResumeNeverDispatchesAWriteTwice kills worker 1 at a write of a
// three-step script (read, send, answer) and lets worker 2 finish. The send
// is dispatched at most once, its tool entry ends exactly once and the Run
// completes. The script has fewer crash points than the 30 random offsets
// the spec asks for, so every one of them is tried; every store in the
// matrix runs it, Postgres when OPENSEAL_TEST_POSTGRES_DSN is set.
func TestLoopResumeNeverDispatchesAWriteTwice(t *testing.T) {
	rules := func() []*looptest.ModelRule {
		return []*looptest.ModelRule{
			{Name: "lookup", When: func(v looptest.StepView) bool { return !resumeHasToolResult(v, "lookup") },
				Reply: func(v looptest.StepView) ModelStepResult {
					return looptest.Act("Looking it up.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "3"}))
				}},
			{Name: "send", When: func(v looptest.StepView) bool { return !resumeHasToolResult(v, "send") },
				Reply: func(v looptest.StepView) ModelStepResult {
					return looptest.Act("Sending it.", looptest.ReviewedCall(looptest.GovernedTool(v, "send"), map[string]any{"to": "bo", "text": "issue 3 is open"}, "Tell bo"))
				}},
			{Name: "answer", Reply: func(looptest.StepView) ModelStepResult {
				return looptest.FinalAnswer(map[string]any{"answer": "Done."})
			}},
		}
	}
	// Writes the fixture's own helpers make (the claim and the first-claim
	// session) are not crash points: a failure there is a fixture failure.
	eligible := func(method string) bool {
		switch method {
		case "ClaimNextAgentRun", "CreateRunAgentSession", "OpenConversationAgentSession":
			return false
		}
		return true
	}
	for _, c := range looptest.StoreMatrix(t, "loop_resume_write") {
		c := c
		t.Run(c.Name, func(t *testing.T) {
			// A clean run measures how many crash points the script has.
			var writes int
			{
				f := looptest.NewFixture(t, c.Open(t), looptest.WithRules(rules()...))
				installResumeTrap(f, func(method string, _ any) bool {
					if eligible(method) {
						writes++
					}
					return false
				})
				run := f.StartRun(RunKindAgentWork, "Check issue 3 and tell bo")
				f.MustStatus(f.DriveToRest(run.ID, 4), AgentRunStatusCompleted)
				if writes < 5 {
					t.Fatalf("clean run made %d writes", writes)
				}
			}
			for offset := 1; offset <= writes; offset++ {
				t.Run(fmt.Sprintf("crash_at_write_%02d", offset), func(t *testing.T) {
					f := looptest.NewFixture(t, c.Open(t), looptest.WithRules(rules()...))
					seen := 0
					var crashedAt string
					trap := installResumeTrap(f, func(method string, _ any) bool {
						if !eligible(method) {
							return false
						}
						seen++
						if seen == offset {
							crashedAt = method
							return true
						}
						return false
					})
					run := f.StartRun(RunKindAgentWork, "Check issue 3 and tell bo")
					for claims := 0; !trap.didFire(); claims++ {
						if claims == 6 {
							t.Fatalf("the trap at write %d never fired", offset)
						}
						if isTerminalAgentRunStatus(f.MustRun(run.ID).Status) {
							t.Fatalf("the Run ended before write %d", offset)
						}
						cancel, done := resumeStartClaim(f, run.ID)
						resumeWait(t, done, "the claim")
						if trap.didFire() {
							f.Crash(cancel, done)
						} else {
							cancel()
							if current := f.MustRun(run.ID); current.AvailableAt.After(f.Clock.Now()) {
								f.Clock.Advance(current.AvailableAt.Sub(f.Clock.Now()) + time.Millisecond)
							}
						}
					}

					final := f.DriveToRest(run.ID, 6)
					if final.Status != AgentRunStatusCompleted {
						t.Fatalf("crash at %s (write %d): run %s (%#v), log %s", crashedAt, offset, final.Status, final.FailureExplanation, resumeLog(f, run.ID))
					}
					if n := f.Dispatch.Count("send"); n > 1 {
						t.Fatalf("crash at %s (write %d): send dispatched %d times", crashedAt, offset, n)
					}
					terminal := 0
					for _, e := range resumeToolEntries(f, run.ID, "send") {
						if e.State.Terminal() {
							terminal++
						}
					}
					if terminal != 1 {
						t.Fatalf("crash at %s (write %d): %d terminal send entries, want 1; log %s", crashedAt, offset, terminal, resumeLog(f, run.ID))
					}
				})
			}
		})
	}
}
