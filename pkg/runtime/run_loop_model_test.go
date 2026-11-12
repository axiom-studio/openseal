package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/modelstep/modelsteptest"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

type modelProducerTestHost struct {
	requests []ModelStepRequest
	result   func(req ModelStepRequest) ModelStepResult
	err      error
}

func (h *modelProducerTestHost) ExecuteModelStep(_ context.Context, req ModelStepRequest) (ModelStepResult, error) {
	h.requests = append(h.requests, req)
	if h.err != nil {
		return ModelStepResult{}, h.err
	}
	if h.result != nil {
		return h.result(req), nil
	}
	return modelProducerTestResult(req), nil
}

func (h *modelProducerTestHost) ExecuteHostTool(context.Context, HostToolRequest) (HostToolResult, error) {
	return HostToolResult{}, ErrHostToolUnsupported
}

func modelProducerTestResult(req ModelStepRequest) ModelStepResult {
	a := *modelStepTestAssistant()
	a.Text = "Done."
	a.Usage = ModelStepUsage{InputTokens: 1500, OutputTokens: 40, Cost: 0.01, DurationMS: 900, ProviderDurationMS: 800, Attempts: 1}
	return ModelStepResult{
		APIVersion: ModelStepAPIVersion, StepID: req.StepID, Assistant: a,
		Model:  ModelStepModelInfo{Provider: a.Provider, Model: a.Model, ContextTokens: 200000, InputLimitTokens: 180000},
		Render: ModelStepRenderInfo{EstimatedInputTokens: 1400},
	}
}

type modelProducerTestPreparer struct {
	calls int
	req   ModelStepRequest
}

func (p *modelProducerTestPreparer) PrepareModelStep(context.Context, agentrun.StepInput) (ModelStepRequest, error) {
	p.calls++
	return p.req, nil
}

type modelProducerTestStore struct {
	path  *AgentSessionPath
	loads int
}

func (s *modelProducerTestStore) LoadAgentSessionPath(_ context.Context, _ Scope, sessionID string) (*AgentSessionPath, error) {
	s.loads++
	if s.path == nil || s.path.Session == nil || s.path.Session.ID != sessionID {
		return nil, ErrAgentSessionNotFound
	}
	return s.path, nil
}

func (s *modelProducerTestStore) GetActionCall(context.Context, Scope, string) (*ActionCall, error) {
	return nil, ErrActionNotFound
}

func (s *modelProducerTestStore) AppendAgentSessionEntries(context.Context, AppendAgentSessionEntriesRequest) (*AppendAgentSessionEntriesResult, error) {
	return nil, errors.New("the test preparer writes no capabilities note")
}

type modelProducerFixture struct {
	host     *modelProducerTestHost
	preparer *modelProducerTestPreparer
	store    *modelProducerTestStore
	producer *modelstep.ModelStepProducer
	render   *modelsteptest.RenderFixture
	in       agentrun.StepInput
}

func newModelProducerFixture(t *testing.T) *modelProducerFixture {
	t.Helper()
	f := &modelProducerFixture{
		host:   &modelProducerTestHost{},
		render: modelsteptest.NewRenderFixture(t, "lane"),
	}
	prepared := validModelStepRequest()
	// The preparer's identity is overwritten; the producer owns it.
	prepared.APIVersion, prepared.Purpose, prepared.RunID, prepared.StepID, prepared.SessionID = "", "", "stale", "stale", "stale"
	prepared.Attempt = 7
	prepared.Entries = nil
	f.preparer = &modelProducerTestPreparer{req: prepared}

	f.render.User("run-1", "old request")
	f.render.Assistant("run-1", "step-0", "c0")
	f.render.Tool("run-1", "step-0", "c0", ToolEntryCompleted)
	kept := f.render.User("run-1", "recent request")
	f.render.Compaction("run-1", kept.ID)
	f.render.Note("run-1", "Run context")
	f.store = &modelProducerTestStore{path: f.render.Path()}

	producer, err := modelstep.NewModelStepProducer(f.host, f.preparer, f.store)
	if err != nil {
		t.Fatalf("NewModelStepProducer: %v", err)
	}
	f.producer = producer
	run := &AgentRun{ID: "run-1", Scope: testSessionScope, AssignedAgentID: "agent-1", SessionID: "lane", Status: AgentRunStatusRunning}
	f.in = agentrun.StepInput{
		Run: run, Step: &AgentTurn{ID: "step-1", Sequence: 1}, Session: f.store.path.Session,
		Path: f.store.path, Binding: &agentrun.TurnRunnerBinding{DefinitionID: "def-1", DefinitionVersion: "1"},
		Mode: agentrun.StepModeNormal, WorkerID: "worker-1",
	}
	return f
}

func (f *modelProducerFixture) produce(t *testing.T, ctx context.Context, in agentrun.StepInput) (ModelStepRequest, agentrun.StepOutput) {
	t.Helper()
	out, err := f.producer.Produce(ctx, in)
	if err != nil {
		t.Fatalf("Produce: %v", err)
	}
	if err := modelstep.ValidateStepOutput(StepProducerModel, out); err != nil {
		t.Fatalf("validateStepOutput: %v", err)
	}
	return f.host.requests[len(f.host.requests)-1], out
}

func TestModelStepProducerRequestShape(t *testing.T) {
	f := newModelProducerFixture(t)
	if f.producer.Kind() != StepProducerModel {
		t.Fatalf("Kind = %q", f.producer.Kind())
	}

	req, out := f.produce(t, context.Background(), f.in)
	if req.APIVersion != ModelStepAPIVersion || req.Purpose != ModelStepPurposeStep {
		t.Fatalf("apiVersion/purpose = %q/%q", req.APIVersion, req.Purpose)
	}
	if req.Scope != testSessionScope || req.RunID != "run-1" || req.StepID != "step-1" || req.Attempt != 0 || req.SessionID != "lane" {
		t.Fatalf("identity = %+v %q %q %d %q", req.Scope, req.RunID, req.StepID, req.Attempt, req.SessionID)
	}
	if req.AgentID != "agent-1" || req.DefinitionID != "def-1" {
		t.Fatalf("agent/definition = %q/%q", req.AgentID, req.DefinitionID)
	}
	// The prepared loaded tool has no load entry on the path, so the
	// rendered request declares only the harness and core tools.
	if len(req.Tools) != 2 || req.Stream.FinalAnswerTool != "turn_final_answer" {
		t.Fatalf("tools = %d, stream = %+v", len(req.Tools), req.Stream)
	}
	// Entries are the path rendered from the latest compaction.
	if len(req.Entries) != 3 || req.Entries[0].Type != SessionEntryCompaction ||
		req.Entries[1].Type != SessionEntryUser || req.Entries[2].Type != SessionEntryNote {
		t.Fatalf("entries = %v", modelsteptest.RenderIDs(req.Entries))
	}
	// Unbounded: the preparer's (catalog) cap stays, 0 here; no preview observer.
	if req.MaxOutputTokens != 0 || req.Stream.AnswerPreview {
		t.Fatalf("maxOutputTokens = %d, answerPreview = %v", req.MaxOutputTokens, req.Stream.AnswerPreview)
	}
	if out.ModelProvider != string(ModelStepProviderAnthropic) || out.Model != "claude-test" || out.Assistant.Text != "Done." {
		t.Fatalf("output = %+v", out)
	}
	if lim := out.Assistant.Limits; lim == nil || lim.ContextTokens != 200000 || lim.InputLimitTokens != 180000 || lim.PromptTokens != 1500 {
		t.Fatalf("limits = %+v", out.Assistant.Limits)
	}
	if out.Usage != (TurnUsage{InputTokens: 1500, OutputTokens: 40, Cost: 0.01, DurationMS: 900, ProviderDurationMS: 800}) {
		t.Fatalf("usage = %+v", out.Usage)
	}

	// The forced-compaction retry is Attempt 1; a preview observer turns the preview on.
	retry := f.in
	retry.Attempt = 1
	ctx := WithTurnAnswerPreview(context.Background(), func(context.Context, TurnAnswerPreview) error { return nil })
	req, _ = f.produce(t, ctx, retry)
	if req.Attempt != 1 || !req.Stream.AnswerPreview {
		t.Fatalf("retry attempt = %d, answerPreview = %v", req.Attempt, req.Stream.AnswerPreview)
	}

	// Bounded: PlanStepBudget prepares once, Produce reuses it and caps the
	// output at the step's reserved output tokens.
	f.preparer.calls = 0
	bounded := f.in
	run := *f.in.Run
	run.Budget = &BudgetPolicy{MaxTotalTokens: 400000}
	bounded.Run = &run
	planned, err := f.producer.PlanStepBudget(context.Background(), bounded)
	if err != nil {
		t.Fatalf("PlanStepBudget: %v", err)
	}
	if planned.InputTokens <= 0 || planned.OutputTokens <= 0 || planned.InputTokens+planned.OutputTokens != 400000 {
		t.Fatalf("planned = %+v", planned)
	}
	run.BudgetReservations = map[string]BudgetReservation{"step-1": {ID: "step-1", Usage: BudgetUsage{Turns: 1, InputTokens: planned.InputTokens, OutputTokens: 900}}}
	req, _ = f.produce(t, context.Background(), bounded)
	if f.preparer.calls != 1 {
		t.Fatalf("preparer calls = %d, want 1 (plan and produce share the request)", f.preparer.calls)
	}
	if req.MaxOutputTokens != 900 {
		t.Fatalf("bounded maxOutputTokens = %d, want 900", req.MaxOutputTokens)
	}
	// A smaller catalog cap wins over the reservation.
	f.preparer.req.MaxOutputTokens = 500
	req, _ = f.produce(t, context.Background(), bounded)
	if req.MaxOutputTokens != 500 {
		t.Fatalf("catalog-capped maxOutputTokens = %d, want 500", req.MaxOutputTokens)
	}

	// An unbounded Run plans nothing and prepares nothing.
	f.preparer.calls = 0
	if planned, err := f.producer.PlanStepBudget(context.Background(), f.in); err != nil || planned != (BudgetUsage{}) || f.preparer.calls != 0 {
		t.Fatalf("unbounded plan = %+v, %v, preparer calls %d", planned, err, f.preparer.calls)
	}

	// A context overflow returns the host failure unchanged for the loop.
	f.host.err = &modelstep.TurnHostFailure{Code: modelstep.TurnHostContextExceeded, Message: "too long"}
	var failure *modelstep.TurnHostFailure
	if _, err := f.producer.Produce(context.Background(), f.in); !errors.As(err, &failure) || failure.Code != modelstep.TurnHostContextExceeded {
		t.Fatalf("overflow err = %v", err)
	}
}

func TestModelStepProducerExplainPurposeHasNoTools(t *testing.T) {
	f := newModelProducerFixture(t)
	in := f.in
	in.Mode = agentrun.StepModeExplainFailure
	req, out := f.produce(t, context.Background(), in)
	if req.Purpose != ModelStepPurposeExplanation {
		t.Fatalf("purpose = %q", req.Purpose)
	}
	if len(req.Tools) != 0 || req.Stream.FinalAnswerTool != "" {
		t.Fatalf("explanation tools = %d, finalAnswerTool = %q", len(req.Tools), req.Stream.FinalAnswerTool)
	}
	if len(req.System.AgentInstructions) == 0 || len(req.Entries) == 0 {
		t.Fatalf("explanation keeps system and entries: %+v", req.System)
	}
	if out.Assistant.Text != "Done." || len(out.Assistant.ToolCalls) != 0 {
		t.Fatalf("explanation output = %+v", out.Assistant)
	}

	// A host that answers an explanation with tool calls is misconfigured.
	f.host.result = func(req ModelStepRequest) ModelStepResult {
		res := modelProducerTestResult(req)
		res.Assistant = *modelStepTestAssistant("c9")
		res.Model.Provider = res.Assistant.Provider
		return res
	}
	if _, err := f.producer.Produce(context.Background(), in); !errors.Is(err, modelstep.ErrTurnHostConfiguration) {
		t.Fatalf("explanation with tool calls err = %v, want ErrTurnHostConfiguration", err)
	}
}

func TestModelStepProducerStepIdentityMismatchIsConfigurationError(t *testing.T) {
	f := newModelProducerFixture(t)
	f.host.result = func(req ModelStepRequest) ModelStepResult {
		res := modelProducerTestResult(req)
		res.StepID = "step-other"
		return res
	}
	out, err := f.producer.Produce(context.Background(), f.in)
	if !errors.Is(err, modelstep.ErrTurnHostConfiguration) {
		t.Fatalf("mismatched step id err = %v, want ErrTurnHostConfiguration", err)
	}
	if out.Assistant != nil {
		t.Fatalf("mismatched result returned an assistant entry")
	}

	// A host that fills the kernel-only limits is misconfigured too.
	f.host.result = func(req ModelStepRequest) ModelStepResult {
		res := modelProducerTestResult(req)
		res.Assistant.Limits = &AssistantModelLimits{ContextTokens: 1}
		return res
	}
	if _, err := f.producer.Produce(context.Background(), f.in); !errors.Is(err, modelstep.ErrTurnHostConfiguration) {
		t.Fatalf("host-filled limits err = %v, want ErrTurnHostConfiguration", err)
	}

	// A step without a session never reaches the host.
	calls := len(f.host.requests)
	in := f.in
	in.Session = nil
	if _, err := f.producer.Produce(context.Background(), in); !errors.Is(err, modelstep.ErrTurnHostConfiguration) || len(f.host.requests) != calls {
		t.Fatalf("sessionless step err = %v, host calls %d -> %d", err, calls, len(f.host.requests))
	}
}
