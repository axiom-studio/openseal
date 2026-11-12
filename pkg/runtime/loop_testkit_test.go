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
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
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

func kitStepRequest(stepID string, entries ...models.SessionEntryView) models.ModelStepRequest {
	return models.ModelStepRequest{
		APIVersion: models.ModelStepAPIVersion, Purpose: models.ModelStepPurposeStep,
		Scope: models.Scope{Kind: "tenant", ID: "kit"}, RunID: "run-1", StepID: stepID, SessionID: "session-1",
		Tools:   []models.ModelStepTool{{Name: "lookup", Group: models.ModelStepToolCore}},
		Entries: entries,
	}
}

func kitUserView(seq int64, text string, author *models.EntryAuthor) models.SessionEntryView {
	return models.SessionEntryView{ID: fmt.Sprintf("e%d", seq), Sequence: seq, Type: models.SessionEntryUser,
		User: &models.UserEntryView{Author: author, Content: []models.ContentPart{{Type: "text", Text: text}}}}
}

func TestLoopTestkitUnmatchedStepFailsLoudly(t *testing.T) {
	rec := &recordingTB{TB: t}
	model := looptest.NewScriptedModel(rec, &looptest.ModelRule{Name: "never", When: func(looptest.StepView) bool { return false }, Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("no") }})

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
	if got := len(model.Calls("")); got != 1 {
		t.Fatalf("recorded requests = %d, want 1", got)
	}
}

func TestLoopTestkitRulesAreOrderedAndCounted(t *testing.T) {
	model := looptest.NewScriptedModel(t,
		&looptest.ModelRule{Name: "first", Times: 1, Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("one") }},
		&looptest.ModelRule{Name: "second", Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("two") }},
		&looptest.ModelRule{Name: "shadowed", Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("three") }},
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
	model = looptest.NewScriptedModel(t, &looptest.ModelRule{Name: "step", When: func(v looptest.StepView) bool { return v.Purpose() == models.ModelStepPurposeStep }, Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("ok") }})
	model.Add(&looptest.ModelRule{Name: "overflow", When: func(v looptest.StepView) bool { return v.Purpose() == models.ModelStepPurposeSummary }, Err: failing})
	summary := kitStepRequest("step-sum")
	summary.Purpose = models.ModelStepPurposeSummary
	if _, err := model.ExecuteModelStep(context.Background(), summary); !errors.Is(err, failing) {
		t.Fatalf("summary err = %v, want the scripted failure", err)
	}
	if _, err := model.ExecuteModelStep(context.Background(), kitStepRequest("step-4")); err != nil {
		t.Fatal(err)
	}
	if got := len(model.Calls(models.ModelStepPurposeSummary)); got != 1 {
		t.Fatalf("summary calls = %d, want 1", got)
	}
	if got := len(model.Calls(models.ModelStepPurposeStep)); got != 1 {
		t.Fatalf("step calls = %d, want 1", got)
	}
}

func TestLoopTestkitBeforeHookAndCancellation(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	model := looptest.NewScriptedModel(t, &looptest.ModelRule{Name: "block",
		Before: func(ctx context.Context, v looptest.StepView) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return errors.New("released by cancel")
			}
		},
		Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("late") },
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
		models.SessionEntryView{ID: "e0", Sequence: 1, Type: models.SessionEntryCompaction, Compaction: &models.CompactionEntryView{Summary: "earlier work"}},
		kitUserView(2, "find issue 7", &models.EntryAuthor{Kind: models.EntryAuthorPerson, ID: "ana", Label: "Ana"}),
		models.SessionEntryView{ID: "e3", Sequence: 3, Type: models.SessionEntryAssistant, Assistant: &models.AssistantEntry{Provider: looptest.ModelProvider, Model: looptest.ModelName,
			ToolCalls: []models.ModelToolCall{{ID: "c1-1", Name: "lookup", Arguments: args}}, StopReason: models.ModelStopToolCalls}},
		models.SessionEntryView{ID: "e4", Sequence: 4, Type: models.SessionEntryTool, Tool: &models.ToolEntryView{CallID: "c1-1", Name: "lookup", Arguments: args,
			State: models.ToolEntryCompleted, Result: models.ToolResultView{Content: []models.ContentPart{{Type: "json", Value: []byte(`{"status":"open"}`)}}}}},
		models.SessionEntryView{ID: "e5", Sequence: 5, Type: models.SessionEntryNote, Note: &models.NoteEntryView{Kind: "repeat", Text: "you already looked"}},
		kitUserView(6, "and close it", nil),
	)
	v := looptest.StepView{Req: req}
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
	if d := v.Describe(); !strings.Contains(d, "compaction user assistant tool:lookup:completed note:repeat user") {
		t.Fatalf("describe = %s", d)
	}
}

func TestLoopTestkitResultBuildersPassValidation(t *testing.T) {
	cases := map[string]models.ModelStepResult{
		"say":       looptest.WithUsage(looptest.Say("done"), 10, 2),
		"act":       looptest.WithReasoning(looptest.Act("Looking it up.", looptest.Call("lookup", map[string]any{"id": "7"}), looptest.Call("list_items", nil)), "think-1"),
		"final":     looptest.FinalAnswer(map[string]any{"answer": "Closed."}),
		"emptyText": looptest.Act(""),
	}
	for name, built := range cases {
		model := looptest.NewScriptedModel(t, &looptest.ModelRule{Name: name, Reply: func(looptest.StepView) models.ModelStepResult { return built }})
		req := kitStepRequest("step-x", kitUserView(1, "go", nil))
		res, err := model.ExecuteModelStep(context.Background(), req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := modelstep.ValidateModelStepResult(req, res); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.Model.ContextTokens != looptest.DefaultContextWindow || res.Assistant.Model != "fake-model" {
			t.Fatalf("%s: model info = %#v", name, res.Model)
		}
	}

	model := looptest.NewScriptedModel(t, &looptest.ModelRule{Reply: func(looptest.StepView) models.ModelStepResult {
		return looptest.Act("two", looptest.Call("lookup", map[string]any{"id": "1"}), looptest.Call("lookup", map[string]any{"id": "2"}))
	}})
	model.SetContextWindow(4096)
	res, err := model.ExecuteModelStep(context.Background(), kitStepRequest("step-2", kitUserView(1, "go", nil),
		models.SessionEntryView{ID: "a", Sequence: 2, Type: models.SessionEntryAssistant, Assistant: &models.AssistantEntry{Provider: looptest.ModelProvider, Model: looptest.ModelName, StopReason: models.ModelStopEnd}}))
	if err != nil {
		t.Fatal(err)
	}
	if ids := res.Assistant.ToolCalls[0].ID + "," + res.Assistant.ToolCalls[1].ID; ids != "c2-1,c2-2" {
		t.Fatalf("call ids = %s", ids)
	}
	if res.Narration != "two" || res.Assistant.StopReason != models.ModelStopToolCalls || res.Model.ContextTokens != 4096 {
		t.Fatalf("result = %#v", res)
	}
	if res := looptest.FinalAnswer(nil); res.Assistant.ToolCalls[0].Name != string(models.ControlToolFinalAnswer) {
		t.Fatalf("finalAnswer calls %s", res.Assistant.ToolCalls[0].Name)
	}
}

func TestLoopTestkitFakeHostTools(t *testing.T) {
	rec := &recordingTB{TB: t}
	tools := looptest.NewFakeHostTools(rec)
	release := make(chan struct{})
	tools.Handle("read_file", func(ctx context.Context, req models.HostToolRequest) (models.HostToolResult, error) {
		<-release
		return looptest.HostText("contents of " + fmt.Sprint(req.Arguments["path"])), nil
	})
	host := looptest.FakeStepHost{ScriptedModel: looptest.NewScriptedModel(t), FakeHostTools: tools}

	var wg sync.WaitGroup
	results := make([]models.HostToolResult, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := host.ExecuteHostTool(context.Background(), models.HostToolRequest{InvocationID: fmt.Sprintf("entry-%d", i), Operation: "read_file", Arguments: map[string]interface{}{"path": "a.txt"}})
			if err != nil {
				t.Error(err)
			}
			results[i] = res
		}(i)
	}
	deadline := time.After(5 * time.Second)
	for tools.PeakInFlight() < 2 {
		select {
		case <-deadline:
			t.Fatal("host tool calls did not overlap")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(release)
	wg.Wait()
	if results[0].APIVersion != models.HostToolAPIVersion || results[1].InvocationID != "entry-1" || looptest.PartsText(results[0].Content) != "contents of a.txt" {
		t.Fatalf("results = %#v", results)
	}

	res, err := host.ExecuteHostTool(context.Background(), models.HostToolRequest{InvocationID: "entry-9", Operation: "rm_rf"})
	if err != nil || !res.IsError {
		t.Fatalf("unknown tool = %#v, %v", res, err)
	}
	if errs := rec.recorded(); len(errs) != 1 || !strings.Contains(errs[0], `"rm_rf"`) {
		t.Fatalf("recorded = %q", errs)
	}
	if got := len(tools.Requests("read_file")); got != 2 {
		t.Fatalf("read_file requests = %d", got)
	}
}

func TestLoopTestkitFakeClock(t *testing.T) {
	start := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	clock := looptest.NewFakeClock(start)
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
	looptest.EachStore(t, "kit_matrix", func(t *testing.T, store models.KernelStore) {
		if store == nil {
			t.Fatal("nil store")
		}
		if _, ok := store.(models.AgentSessionStore); !ok {
			t.Fatalf("%T is not an AgentSessionStore", store)
		}
		got = append(got, t.Name()[strings.LastIndex(t.Name(), "/")+1:])
	})
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("stores = %v, want %v", got, want)
	}
}

func TestLoopTestkitCrashStoreRefusesWritesAfterArm(t *testing.T) {
	looptest.EachStore(t, "kit_crash", func(t *testing.T, base models.KernelStore) {
		ctx := context.Background()
		scope := models.Scope{Kind: "tenant", ID: "kit-crash"}
		now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
		store := looptest.NewCrashStore(t, base)
		portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
		create := func() (*models.AgentRun, error) {
			return portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
				Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "finn"}, AssignedAgentID: "finn",
				Goal: "crash me", Source: models.RunSourceObjective,
			})
		}
		claim := models.AgentRunClaim{Scope: scope, WorkerID: "loop-worker-1", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute}

		run, err := create()
		if err != nil {
			t.Fatalf("create before arm: %v", err)
		}
		store.Arm()
		if _, err := create(); !errors.Is(err, looptest.ErrSimulatedCrash) {
			t.Fatalf("create after arm = %v, want errSimulatedCrash", err)
		}
		if _, err := store.ClaimNextAgentRun(ctx, claim); !errors.Is(err, looptest.ErrSimulatedCrash) {
			t.Fatalf("claim after arm = %v", err)
		}
		if _, err := store.CreateRunAgentSession(ctx, models.CreateRunAgentSessionRequest{}); !errors.Is(err, looptest.ErrSimulatedCrash) {
			t.Fatalf("create session after arm = %v", err)
		}
		if _, err := store.AppendAgentSessionEntries(ctx, models.AppendAgentSessionEntriesRequest{}); !errors.Is(err, looptest.ErrSimulatedCrash) {
			t.Fatalf("append after arm = %v", err)
		}
		if _, err := store.TransitionToolEntry(ctx, models.ToolEntryTransitionRequest{}); !errors.Is(err, looptest.ErrSimulatedCrash) {
			t.Fatalf("transition after arm = %v", err)
		}
		if err := store.RecordAgentTurnStepModel(ctx, &models.AgentTurn{}, 0, nil); !errors.Is(err, looptest.ErrSimulatedCrash) {
			t.Fatalf("record step after arm = %v", err)
		}
		if _, err := store.PersistActionExecution(ctx, models.ActionExecutionRecord{}); !errors.Is(err, looptest.ErrSimulatedCrash) {
			t.Fatalf("persist action after arm = %v", err)
		}
		if got, err := store.GetAgentRun(ctx, scope, run.ID); err != nil || got.ID != run.ID {
			t.Fatalf("read after arm = %v, %v", got, err)
		}
		if runs, err := store.ListAgentRuns(ctx, models.AgentRunFilter{Scope: scope}); err != nil || len(runs) != 1 {
			t.Fatalf("list after arm = %d, %v", len(runs), err)
		}
		if _, err := store.ListAgentSessionEntries(ctx, models.SessionEntryFilter{Scope: scope, SessionID: "none"}); errors.Is(err, looptest.ErrSimulatedCrash) {
			t.Fatalf("session read refused after arm")
		}

		store.Disarm()
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
	for _, pattern := range []string{"looptest/*.go", "loop_testkit_test.go", "loop_scenario_*_test.go", "runworker/loop_scenario_*_test.go"} {
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
	looptest.EachStore(t, "kit_fixture_objective", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "lookup", Times: 1,
				Reply: func(v looptest.StepView) models.ModelStepResult {
					return looptest.Act("Looking it up.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "7"}))
				}},
			&looptest.ModelRule{Name: "answer", Reply: func(v looptest.StepView) models.ModelStepResult {
				for _, e := range v.ToolResultsSinceUser() {
					if strings.Contains(looptest.PartsText(e.Tool.Result.Content), "Issue 7") {
						return looptest.FinalAnswer(map[string]any{"answer": "Issue 7 is open."})
					}
				}
				return looptest.Say("I could not find it.")
			}},
		))
		run := f.StartRun(models.RunKindAgentWork, "Check issue 7")
		done := f.DriveToRest(run.ID, 3)
		f.MustStatus(done, models.AgentRunStatusCompleted)
		if got, want := strings.Join(f.EntryTypes(run.ID), ","), "user,assistant,tool:completed,assistant,tool:completed"; got != want {
			t.Fatalf("log = %s, want %s", got, want)
		}
		if f.Dispatch.Count("lookup") != 1 {
			t.Fatalf("lookup dispatched %d times, want 1", f.Dispatch.Count("lookup"))
		}
		if calls := f.Calls(run.ID); len(calls) != 1 || calls[0].Status != models.ActionCallStatusSucceeded {
			t.Fatalf("action calls = %#v", calls)
		}
		if steps := f.Steps(run.ID); len(steps) != 2 {
			t.Fatalf("steps = %d, want 2", len(steps))
		}
		if len(f.Events(run.ID, "turn.narration")) != 1 {
			t.Fatalf("narration events = %d, want 1", len(f.Events(run.ID, "turn.narration")))
		}
		if users := f.Model.Calls(models.ModelStepPurposeStep)[0]; !strings.Contains(looptest.StepView{Req: users}.LastUserText(), "Check issue 7") {
			t.Fatalf("first step did not see the goal: %s", looptest.StepView{Req: users}.Describe())
		}
	})
}

func TestLoopTestkitFixtureChatPostsTheAnswer(t *testing.T) {
	looptest.EachStore(t, "kit_fixture_chat", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithRules(&looptest.ModelRule{Name: "hello",
			When:  func(v looptest.StepView) bool { return strings.Contains(v.LastUserText(), "hello") },
			Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("Hi Ana.") }}))
		conversation, run := f.StartChat("hello finn")
		done := f.DriveToRest(run.ID, 3)
		f.MustStatus(done, models.AgentRunStatusCompleted)
		messages := f.Messages(conversation.ID)
		if len(messages) != 2 || messages[1].Sender.ID != looptest.FixtureAgent || !strings.Contains(messages[1].Content, "Hi Ana.") {
			t.Fatalf("channel = %s", looptest.DescribeMessages(messages))
		}
		if first := (looptest.StepView{Req: f.Model.Calls(models.ModelStepPurposeStep)[0]}); !strings.Contains(first.LastUserText(), "hello finn") {
			t.Fatalf("first step did not see the trigger: %s", first.Describe())
		}
	})
}

func TestLoopTestkitFixtureApprovalParksAndResumes(t *testing.T) {
	looptest.EachStore(t, "kit_fixture_approval", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithApprovalForWrites(), looptest.WithRules(
			&looptest.ModelRule{Name: "update", Times: 1, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Closing it.", looptest.ReviewedCall(looptest.GovernedTool(v, "update_status"), map[string]any{"id": "7", "status": "closed"}, "Close issue 7"))
			}},
			&looptest.ModelRule{Name: "done", Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.FinalAnswer(map[string]any{"answer": "Closed issue 7."})
			}},
		))
		run := f.StartRun(models.RunKindAgentWork, "Close issue 7")
		parked := f.DriveToRest(run.ID, 3)
		f.MustStatus(parked, models.AgentRunStatusWaitingForApproval)
		if f.Dispatch.Count("update_status") != 0 {
			t.Fatal("the write ran before approval")
		}
		f.Decide(run.ID, models.ApprovalDecisionApprove, "go ahead")
		done := f.DriveToRest(run.ID, 3)
		f.MustStatus(done, models.AgentRunStatusCompleted)
		if f.Dispatch.Count("update_status") != 1 {
			t.Fatalf("update_status dispatched %d times, want 1", f.Dispatch.Count("update_status"))
		}
	})
}

func TestLoopTestkitFixtureCrashResumesOnSecondWorker(t *testing.T) {
	looptest.EachStore(t, "kit_fixture_crash", func(t *testing.T, store models.KernelStore) {
		entered := make(chan struct{})
		f := looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "lookup", Times: 1, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Looking it up.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "9"}))
			}},
			&looptest.ModelRule{Name: "answer", Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.FinalAnswer(map[string]any{"answer": "Issue 9 is open."})
			}},
		))
		release := f.Dispatch.Hold("lookup")
		f.Dispatch.On("lookup", func(in agentrun.ActionDispatchInput) (map[string]interface{}, error) {
			return map[string]interface{}{"id": "9", "status": "open", "title": "Issue 9"}, nil
		})
		go func() {
			for f.Dispatch.Count("lookup") == 0 {
				time.Sleep(time.Millisecond)
			}
			close(entered)
		}()
		run := f.StartRun(models.RunKindAgentWork, "Check issue 9")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			f.RunClaimContext(ctx, run.ID)
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("dispatch never started")
		}
		f.Crash(cancel, done)
		close(release)
		if f.WorkerID != looptest.FixtureWorker2 {
			t.Fatalf("worker after crash = %s", f.WorkerID)
		}
		final := f.DriveToRest(run.ID, 4)
		f.MustStatus(final, models.AgentRunStatusCompleted)
		if final.LeaseOwner != "" && final.LeaseOwner != looptest.FixtureWorker2 {
			t.Fatalf("lease owner = %s", final.LeaseOwner)
		}
	})
}
