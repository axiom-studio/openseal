package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// Self-tests of the run-loop test kit (run-loop section 12 §3.6).

// recordingTB captures Errorf so a test can assert that the kit failed loudly.
type recordingTB struct {
	testing.TB
	mu     sync.Mutex
	errors []string
}

func (r *recordingTB) Helper() {}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recordingTB) recorded() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.errors...)
}

func kitStepRequest(stepID string, entries ...SessionEntryView) ModelStepRequest {
	return ModelStepRequest{
		APIVersion: ModelStepAPIVersion, Purpose: ModelStepPurposeStep,
		Scope: Scope{Kind: "tenant", ID: "kit"}, RunID: "run-1", StepID: stepID, SessionID: "session-1",
		Tools:   []ModelStepTool{{Name: "lookup", Group: ModelStepToolCore}},
		Entries: entries,
	}
}

func kitUserView(seq int64, text string, author *EntryAuthor) SessionEntryView {
	return SessionEntryView{ID: fmt.Sprintf("e%d", seq), Sequence: seq, Type: SessionEntryUser,
		User: &UserEntryView{Author: author, Content: []ContentPart{{Type: "text", Text: text}}}}
}

func TestLoopTestkitUnmatchedStepFailsLoudly(t *testing.T) {
	rec := &recordingTB{TB: t}
	model := newScriptedModel(rec, &modelRule{name: "never", when: func(stepView) bool { return false }, reply: func(stepView) ModelStepResult { return say("no") }})

	done := make(chan error, 1)
	go func() {
		_, err := model.ExecuteModelStep(context.Background(), kitStepRequest("step-1", kitUserView(1, "hello", nil)))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("unmatched step returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("unmatched step hung")
	}
	errs := rec.recorded()
	if len(errs) != 1 || !strings.Contains(errs[0], "no scripted rule for step step-1") || !strings.Contains(errs[0], `lastUser="hello"`) {
		t.Fatalf("recorded errors = %q", errs)
	}
	if got := len(model.calls("")); got != 1 {
		t.Fatalf("recorded requests = %d, want 1", got)
	}
}

func TestLoopTestkitRulesAreOrderedAndCounted(t *testing.T) {
	model := newScriptedModel(t,
		&modelRule{name: "first", times: 1, reply: func(stepView) ModelStepResult { return say("one") }},
		&modelRule{name: "second", reply: func(stepView) ModelStepResult { return say("two") }},
		&modelRule{name: "shadowed", reply: func(stepView) ModelStepResult { return say("three") }},
	)
	var got []string
	for i := 0; i < 3; i++ {
		res, err := model.ExecuteModelStep(context.Background(), kitStepRequest(fmt.Sprintf("step-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, res.Assistant.Text)
	}
	if strings.Join(got, ",") != "one,two,two" {
		t.Fatalf("replies = %v, want one,two,two", got)
	}

	failing := &modelstep.TurnHostFailure{Code: modelstep.TurnHostContextExceeded, Message: "too long"}
	model = newScriptedModel(t, &modelRule{name: "step", when: func(v stepView) bool { return v.Purpose() == ModelStepPurposeStep }, reply: func(stepView) ModelStepResult { return say("ok") }})
	model.add(&modelRule{name: "overflow", when: func(v stepView) bool { return v.Purpose() == ModelStepPurposeSummary }, err: failing})
	summary := kitStepRequest("step-sum")
	summary.Purpose = ModelStepPurposeSummary
	if _, err := model.ExecuteModelStep(context.Background(), summary); !errors.Is(err, failing) {
		t.Fatalf("summary err = %v, want the scripted failure", err)
	}
	if _, err := model.ExecuteModelStep(context.Background(), kitStepRequest("step-4")); err != nil {
		t.Fatal(err)
	}
	if got := len(model.calls(ModelStepPurposeSummary)); got != 1 {
		t.Fatalf("summary calls = %d, want 1", got)
	}
	if got := len(model.calls(ModelStepPurposeStep)); got != 1 {
		t.Fatalf("step calls = %d, want 1", got)
	}
}

func TestLoopTestkitBeforeHookAndCancellation(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	model := newScriptedModel(t, &modelRule{name: "block",
		before: func(ctx context.Context, v stepView) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return errors.New("released by cancel")
			}
		},
		reply: func(stepView) ModelStepResult { return say("late") },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := model.ExecuteModelStep(ctx, kitStepRequest("step-1"))
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("before hook never ran")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled step hung")
	}
}

func TestLoopTestkitStepViewReadsRenderedEntries(t *testing.T) {
	args := []byte(`{"id":"7"}`)
	req := kitStepRequest("step-3",
		SessionEntryView{ID: "e0", Sequence: 1, Type: SessionEntryCompaction, Compaction: &CompactionEntryView{Summary: "earlier work"}},
		kitUserView(2, "find issue 7", &EntryAuthor{Kind: EntryAuthorPerson, ID: "ana", Label: "Ana"}),
		SessionEntryView{ID: "e3", Sequence: 3, Type: SessionEntryAssistant, Assistant: &AssistantEntry{Provider: kitModelProvider, Model: kitModelName,
			ToolCalls: []ModelToolCall{{ID: "c1-1", Name: "lookup", Arguments: args}}, StopReason: ModelStopToolCalls}},
		SessionEntryView{ID: "e4", Sequence: 4, Type: SessionEntryTool, Tool: &ToolEntryView{CallID: "c1-1", Name: "lookup", Arguments: args,
			State: ToolEntryCompleted, Result: ToolResultView{Content: []ContentPart{{Type: "json", Value: []byte(`{"status":"open"}`)}}}}},
		SessionEntryView{ID: "e5", Sequence: 5, Type: SessionEntryNote, Note: &NoteEntryView{Kind: "repeat", Text: "you already looked"}},
		kitUserView(6, "and close it", nil),
	)
	v := stepView{req: req}
	if v.Step() != 2 {
		t.Fatalf("Step = %d, want 2", v.Step())
	}
	if summary, ok := v.Compaction(); !ok || summary != "earlier work" {
		t.Fatalf("Compaction = %q, %v", summary, ok)
	}
	if v.LastUserText() != "and close it" || strings.Join(v.UserTexts(), "|") != "find issue 7|and close it" {
		t.Fatalf("user texts = %q / %q", v.LastUserText(), v.UserTexts())
	}
	if who, ok := v.UserAuthor("issue 7"); !ok || who != "person:ana" {
		t.Fatalf("UserAuthor = %q, %v", who, ok)
	}
	if who, _ := v.UserAuthor("close it"); who != "requester" {
		t.Fatalf("UserAuthor without author = %q", who)
	}
	if content, isErr, ok := v.ToolResult("lookup"); !ok || isErr || content != `{"status":"open"}` {
		t.Fatalf("ToolResult = %q, %v, %v", content, isErr, ok)
	}
	if got := v.ToolResultsSinceUser(); len(got) != 0 {
		t.Fatalf("ToolResultsSinceUser after a user entry = %d", len(got))
	}
	if strings.Join(v.Notes(), "|") != "you already looked" || !v.HasTool("lookup") || v.HasTool("send") {
		t.Fatalf("notes/tools = %q", v.Notes())
	}
	if last := v.LastEntry(); last == nil || last.ID != "e6" {
		t.Fatalf("LastEntry = %#v", last)
	}
	if d := v.describe(); !strings.Contains(d, "compaction user assistant tool:lookup:completed note:repeat user") {
		t.Fatalf("describe = %s", d)
	}
}

func TestLoopTestkitResultBuildersPassValidation(t *testing.T) {
	cases := map[string]ModelStepResult{
		"say":       withUsage(say("done"), 10, 2),
		"act":       withReasoning(act("Looking it up.", call("lookup", map[string]any{"id": "7"}), call("list_items", nil)), "think-1"),
		"final":     finalAnswer(map[string]any{"answer": "Closed."}),
		"emptyText": act(""),
	}
	for name, built := range cases {
		model := newScriptedModel(t, &modelRule{name: name, reply: func(stepView) ModelStepResult { return built }})
		req := kitStepRequest("step-x", kitUserView(1, "go", nil))
		res, err := model.ExecuteModelStep(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := modelstep.ValidateModelStepResult(req, res); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Model.ContextTokens != kitDefaultContextWindow || res.Assistant.Model != "fake-model" {
			t.Fatalf("%s: model info = %#v", name, res.Model)
		}
	}

	model := newScriptedModel(t, &modelRule{reply: func(stepView) ModelStepResult {
		return act("two", call("lookup", map[string]any{"id": "1"}), call("lookup", map[string]any{"id": "2"}))
	}})
	model.setContextWindow(4096)
	res, err := model.ExecuteModelStep(context.Background(), kitStepRequest("step-2", kitUserView(1, "go", nil),
		SessionEntryView{ID: "a", Sequence: 2, Type: SessionEntryAssistant, Assistant: &AssistantEntry{Provider: kitModelProvider, Model: kitModelName, StopReason: ModelStopEnd}}))
	if err != nil {
		t.Fatal(err)
	}
	if ids := res.Assistant.ToolCalls[0].ID + "," + res.Assistant.ToolCalls[1].ID; ids != "c2-1,c2-2" {
		t.Fatalf("call ids = %s", ids)
	}
	if res.Narration != "two" || res.Assistant.StopReason != ModelStopToolCalls || res.Model.ContextTokens != 4096 {
		t.Fatalf("result = %#v", res)
	}
	if res := finalAnswer(nil); res.Assistant.ToolCalls[0].Name != string(ControlToolFinalAnswer) {
		t.Fatalf("finalAnswer calls %s", res.Assistant.ToolCalls[0].Name)
	}
}

func TestLoopTestkitFakeHostTools(t *testing.T) {
	rec := &recordingTB{TB: t}
	tools := newFakeHostTools(rec)
	release := make(chan struct{})
	tools.handle("read_file", func(ctx context.Context, req HostToolRequest) (HostToolResult, error) {
		<-release
		return hostText("contents of " + fmt.Sprint(req.Arguments["path"])), nil
	})
	host := fakeStepHost{scriptedModel: newScriptedModel(t), fakeHostTools: tools}

	var wg sync.WaitGroup
	results := make([]HostToolResult, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := host.ExecuteHostTool(context.Background(), HostToolRequest{InvocationID: fmt.Sprintf("entry-%d", i), Operation: "read_file", Arguments: map[string]interface{}{"path": "a.txt"}})
			if err != nil {
				t.Error(err)
			}
			results[i] = res
		}(i)
	}
	deadline := time.After(5 * time.Second)
	for tools.peakInFlight() < 2 {
		select {
		case <-deadline:
			t.Fatal("host tool calls did not overlap")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(release)
	wg.Wait()
	if results[0].APIVersion != HostToolAPIVersion || results[1].InvocationID != "entry-1" || kitPartsText(results[0].Content) != "contents of a.txt" {
		t.Fatalf("results = %#v", results)
	}

	res, err := host.ExecuteHostTool(context.Background(), HostToolRequest{InvocationID: "entry-9", Operation: "rm_rf"})
	if err != nil || !res.IsError {
		t.Fatalf("unknown tool = %#v, %v", res, err)
	}
	if errs := rec.recorded(); len(errs) != 1 || !strings.Contains(errs[0], `"rm_rf"`) {
		t.Fatalf("recorded = %q", errs)
	}
	if got := len(tools.requests("read_file")); got != 2 {
		t.Fatalf("read_file requests = %d", got)
	}
}

func TestLoopTestkitFakeClock(t *testing.T) {
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	clock := newFakeClock(start)
	clock.Advance(90 * time.Second)
	if got := clock.Now(); !got.Equal(start.Add(90 * time.Second)) {
		t.Fatalf("Now = %v", got)
	}
}

func TestLoopTestkitStoreMatrixRunsEveryAvailableStore(t *testing.T) {
	want := []string{"sqlite"}
	if os.Getenv("OPENSEAL_TEST_POSTGRES_DSN") != "" {
		want = append(want, "postgres")
	}
	var got []string
	eachStore(t, "kit_matrix", func(t *testing.T, store KernelStore) {
		if store == nil {
			t.Fatal("nil store")
		}
		if _, ok := store.(AgentSessionStore); !ok {
			t.Fatalf("%T is not an AgentSessionStore", store)
		}
		got = append(got, t.Name()[strings.LastIndex(t.Name(), "/")+1:])
	})
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("stores = %v, want %v", got, want)
	}
}

func TestLoopTestkitCrashStoreRefusesWritesAfterArm(t *testing.T) {
	eachStore(t, "kit_crash", func(t *testing.T, base KernelStore) {
		ctx := context.Background()
		scope := Scope{Kind: "tenant", ID: "kit-crash"}
		now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
		store := newCrashStore(t, base)
		portfolio := NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
		create := func() (*AgentRun, error) {
			return portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
				Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "finn"}, AssignedAgentID: "finn",
				Goal: "crash me", Source: RunSourceObjective,
			})
		}
		claim := AgentRunClaim{Scope: scope, WorkerID: "loop-worker-1", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute}

		run, err := create()
		if err != nil {
			t.Fatalf("create before arm: %v", err)
		}
		store.arm()
		if _, err := create(); !errors.Is(err, errSimulatedCrash) {
			t.Fatalf("create after arm = %v, want errSimulatedCrash", err)
		}
		if _, err := store.ClaimNextAgentRun(ctx, claim); !errors.Is(err, errSimulatedCrash) {
			t.Fatalf("claim after arm = %v", err)
		}
		if _, err := store.CreateRunAgentSession(ctx, CreateRunAgentSessionRequest{}); !errors.Is(err, errSimulatedCrash) {
			t.Fatalf("create session after arm = %v", err)
		}
		if _, err := store.AppendAgentSessionEntries(ctx, AppendAgentSessionEntriesRequest{}); !errors.Is(err, errSimulatedCrash) {
			t.Fatalf("append after arm = %v", err)
		}
		if _, err := store.TransitionToolEntry(ctx, ToolEntryTransitionRequest{}); !errors.Is(err, errSimulatedCrash) {
			t.Fatalf("transition after arm = %v", err)
		}
		if err := store.RecordAgentTurnStepModel(ctx, &AgentTurn{}, 0, nil); !errors.Is(err, errSimulatedCrash) {
			t.Fatalf("record step after arm = %v", err)
		}
		if _, err := store.PersistActionExecution(ctx, ActionExecutionRecord{}); !errors.Is(err, errSimulatedCrash) {
			t.Fatalf("persist action after arm = %v", err)
		}
		if got, err := store.GetAgentRun(ctx, scope, run.ID); err != nil || got.ID != run.ID {
			t.Fatalf("read after arm = %v, %v", got, err)
		}
		if runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: scope}); err != nil || len(runs) != 1 {
			t.Fatalf("list after arm = %d, %v", len(runs), err)
		}
		if _, err := store.ListAgentSessionEntries(ctx, SessionEntryFilter{Scope: scope, SessionID: "none"}); errors.Is(err, errSimulatedCrash) {
			t.Fatalf("session read refused after arm")
		}

		store.disarm()
		claimed, err := store.ClaimNextAgentRun(ctx, claim)
		if err != nil || claimed == nil || claimed.ID != run.ID {
			t.Fatalf("claim after disarm = %#v, %v", claimed, err)
		}
	})
}

// TestLoopTestkitHasNoCheckpointAccess keeps the kit and the scenarios on
// what the model sees: none of them may touch run-level engine state or the
// old hosted turn form. The child scenario drives runbook engine state and is
// exempt by name.
func TestLoopTestkitHasNoCheckpointAccess(t *testing.T) {
	forbidden := []string{
		"Continuation" + "Checkpoint",
		".Check" + "point[",
		"HostedTurn" + "Form",
		"_open" + "seal",
		"_at" + "las",
	}
	exempt := map[string]bool{"loop_scenario_child_test.go": true}
	var files []string
	for _, pattern := range []string{"loop_testkit_*_test.go", "loop_testkit_test.go", "loop_scenario_*_test.go"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, matches...)
	}
	if len(files) < 3 {
		t.Fatalf("kit files = %v, want at least the model, fixture and self-test files", files)
	}
	for _, name := range files {
		if exempt[name] {
			continue
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range forbidden {
			if strings.Contains(string(raw), needle) {
				t.Errorf("%s contains %q: the kit must not read old-path state", name, needle)
			}
		}
	}
}

func TestLoopTestkitFixtureObjectiveRunReadsThenAnswers(t *testing.T) {
	eachStore(t, "kit_fixture_objective", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(
			&modelRule{name: "lookup", times: 1,
				reply: func(v stepView) ModelStepResult {
					return act("Looking it up.", call(kitGovernedTool(v, "lookup"), map[string]any{"id": "7"}))
				}},
			&modelRule{name: "answer", reply: func(v stepView) ModelStepResult {
				for _, e := range v.ToolResultsSinceUser() {
					if strings.Contains(kitPartsText(e.Tool.Result.Content), "Issue 7") {
						return finalAnswer(map[string]any{"answer": "Issue 7 is open."})
					}
				}
				return say("I could not find it.")
			}},
		))
		run := f.startRun(RunKindAgentWork, "Check issue 7")
		done := f.driveToRest(run.ID, 3)
		f.mustStatus(done, AgentRunStatusCompleted)
		if got, want := strings.Join(f.entryTypes(run.ID), ","), "user,assistant,tool:completed,assistant,tool:completed"; got != want {
			t.Fatalf("log = %s, want %s", got, want)
		}
		if f.dispatch.count("lookup") != 1 {
			t.Fatalf("lookup dispatched %d times, want 1", f.dispatch.count("lookup"))
		}
		if calls := f.calls(run.ID); len(calls) != 1 || calls[0].Status != ActionCallStatusSucceeded {
			t.Fatalf("action calls = %#v", calls)
		}
		if steps := f.steps(run.ID); len(steps) != 2 {
			t.Fatalf("steps = %d, want 2", len(steps))
		}
		if len(f.events(run.ID, "turn.narration")) != 1 {
			t.Fatalf("narration events = %d, want 1", len(f.events(run.ID, "turn.narration")))
		}
		if users := f.model.calls(ModelStepPurposeStep)[0]; !strings.Contains(stepView{req: users}.LastUserText(), "Check issue 7") {
			t.Fatalf("first step did not see the goal: %s", stepView{req: users}.describe())
		}
	})
}

func TestLoopTestkitFixtureChatPostsTheAnswer(t *testing.T) {
	eachStore(t, "kit_fixture_chat", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withRules(&modelRule{name: "hello",
			when:  func(v stepView) bool { return strings.Contains(v.LastUserText(), "hello") },
			reply: func(stepView) ModelStepResult { return say("Hi Ana.") }}))
		conversation, run := f.startChat("hello finn")
		done := f.driveToRest(run.ID, 3)
		f.mustStatus(done, AgentRunStatusCompleted)
		messages := f.messages(conversation.ID)
		if len(messages) != 2 || messages[1].Sender.ID != loopFixtureAgent || !strings.Contains(messages[1].Content, "Hi Ana.") {
			t.Fatalf("channel = %s", kitDescribeMessages(messages))
		}
		if first := (stepView{req: f.model.calls(ModelStepPurposeStep)[0]}); !strings.Contains(first.LastUserText(), "hello finn") {
			t.Fatalf("first step did not see the trigger: %s", first.describe())
		}
	})
}

func TestLoopTestkitFixtureApprovalParksAndResumes(t *testing.T) {
	eachStore(t, "kit_fixture_approval", func(t *testing.T, store KernelStore) {
		f := newLoopFixture(t, store, withApprovalForWrites(), withRules(
			&modelRule{name: "update", times: 1, reply: func(v stepView) ModelStepResult {
				return act("Closing it.", reviewedCall(kitGovernedTool(v, "update_status"), map[string]any{"id": "7", "status": "closed"}, "Close issue 7"))
			}},
			&modelRule{name: "done", reply: func(stepView) ModelStepResult { return finalAnswer(map[string]any{"answer": "Closed issue 7."}) }},
		))
		run := f.startRun(RunKindAgentWork, "Close issue 7")
		parked := f.driveToRest(run.ID, 3)
		f.mustStatus(parked, AgentRunStatusWaitingForApproval)
		if f.dispatch.count("update_status") != 0 {
			t.Fatal("the write ran before approval")
		}
		f.decide(run.ID, ApprovalDecisionApprove, "go ahead")
		done := f.driveToRest(run.ID, 3)
		f.mustStatus(done, AgentRunStatusCompleted)
		if f.dispatch.count("update_status") != 1 {
			t.Fatalf("update_status dispatched %d times, want 1", f.dispatch.count("update_status"))
		}
	})
}

func TestLoopTestkitFixtureCrashResumesOnSecondWorker(t *testing.T) {
	eachStore(t, "kit_fixture_crash", func(t *testing.T, store KernelStore) {
		entered := make(chan struct{})
		f := newLoopFixture(t, store, withRules(
			&modelRule{name: "lookup", times: 1, reply: func(v stepView) ModelStepResult {
				return act("Looking it up.", call(kitGovernedTool(v, "lookup"), map[string]any{"id": "9"}))
			}},
			&modelRule{name: "answer", reply: func(stepView) ModelStepResult { return finalAnswer(map[string]any{"answer": "Issue 9 is open."}) }},
		))
		release := f.dispatch.hold("lookup")
		f.dispatch.on("lookup", func(in agentrun.ActionDispatchInput) (map[string]interface{}, error) {
			return map[string]interface{}{"id": "9", "status": "open", "title": "Issue 9"}, nil
		})
		go func() {
			for f.dispatch.count("lookup") == 0 {
				time.Sleep(time.Millisecond)
			}
			close(entered)
		}()
		run := f.startRun(RunKindAgentWork, "Check issue 9")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.runClaimContext(ctx, run.ID)
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("dispatch never started")
		}
		f.crash(cancel, done)
		close(release)
		if f.workerID != loopFixtureWorker2 {
			t.Fatalf("worker after crash = %s", f.workerID)
		}
		final := f.driveToRest(run.ID, 4)
		f.mustStatus(final, AgentRunStatusCompleted)
		if final.LeaseOwner != "" && final.LeaseOwner != loopFixtureWorker2 {
			t.Fatalf("lease owner = %s", final.LeaseOwner)
		}
	})
}
