package openseal

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

const (
	loopActionsWorker = "loop-worker"
	loopActionsTool   = "release_status"
)

// loopActionsDispatcher records which action-worker spec's dispatcher ran.
type loopActionsDispatcher struct {
	name  string
	calls int
	input ActionDispatchInput
}

func (d *loopActionsDispatcher) dispatcher() ActionDispatcher {
	return ActionDispatcherFunc(func(_ context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
		d.calls++
		d.input = input
		return map[string]interface{}{"dispatchedBy": d.name}, nil
	})
}

func newLoopActionsEngine(t *testing.T, options ...Option) *Engine {
	t.Helper()
	allow := WithActionPolicy(ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
	}))
	engine, err := New(append([]Option{allow}, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	t.Cleanup(engine.Stop)
	return engine
}

// executeLoopStatusCall binds a read-only release status action, claims a
// Run for loopActionsWorker and executes one governed tool call of its step
// through engine.GovernedTools without starting any worker.
func executeLoopStatusCall(t *testing.T, engine *Engine, scope Scope) GovernedToolOutcome {
	t.Helper()
	ctx := context.Background()
	schema := map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{"environment": map[string]interface{}{"type": "string"}}, "required": []interface{}{"environment"},
	}
	definition := &SkillDefinition{
		ID: "release", Version: "1.0.0", Name: "Release", Transport: SkillTransportReference{Kind: "local"},
		Actions: map[string]SkillAction{"status": {
			Name: "status", Description: "Read the release status", Risk: SkillRiskRead, SideEffect: SkillSideEffectRead, Idempotency: SkillIdempotencyNone, InputSchema: schema,
		}},
	}
	if err := engine.RegisterSkill(ctx, definition); err != nil {
		t.Fatal(err)
	}
	binding := &SkillBinding{ID: "release-binding", Scope: SkillScope{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "release-agent", SkillID: "release", SkillVersion: "1.0.0", AllowedActions: []string{"status"}, MaximumRisk: SkillRiskRead, Revision: 1}
	if err := engine.BindSkill(ctx, binding); err != nil {
		t.Fatal(err)
	}
	run, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "release-agent"}, AssignedAgentID: "release-agent", Goal: "check the release", Source: RunSourceObjective})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := engine.ClaimNextAgentRun(ctx, AgentRunClaimRequest{Scope: scope, WorkerID: loopActionsWorker, AssignedAgentID: "release-agent", LeaseDuration: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	executor := engine.GovernedTools()
	if executor == nil {
		t.Fatal("engine with action workers has no governed tools")
	}
	outcome, err := executor.Execute(ctx, GovernedToolCall{
		Run: claimed, Step: &AgentTurn{ID: "step-1", Scope: scope, RunID: run.ID}, WorkerID: loopActionsWorker,
		Binding: &TurnRunnerBinding{DeploymentID: "release-agent", ModelActions: []capability.ModelAction{{
			Name: loopActionsTool, BindingID: binding.ID, BindingRevision: binding.Revision, SkillID: "release", Version: "1.0.0", Action: "status",
			SideEffect: capability.SideEffectRead, Risk: capability.RiskLevelRead, InputSchema: schema,
		}}},
		ToolCallID: "tool-1", Name: loopActionsTool, Arguments: json.RawMessage(`{"environment":"production"}`), Narration: "Checking the release.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.State != runtime.ToolEntryCompleted || outcome.ActionCallID == "" {
		t.Fatalf("outcome = %#v", outcome)
	}
	call, err := engine.GetActionCall(ctx, scope, outcome.ActionCallID)
	if err != nil {
		t.Fatal(err)
	}
	if call.Status != ActionCallStatusSucceeded || call.Origin != ActionOriginLoop || call.Execution != ActionExecutionLoop || call.TurnID != "step-1" {
		t.Fatalf("call = %#v", call)
	}
	return outcome
}

func TestEngineBuildsGovernedToolExecutorFromSupervisorSpec(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "loop-actions-supervisor"}
	supervisor := &loopActionsDispatcher{name: "supervisor"}
	pool := &loopActionsDispatcher{name: "pool"}
	sources := WorkerScopeSourceFunc(func(context.Context) ([]Scope, error) { return []Scope{scope}, nil })
	engine := newLoopActionsEngine(t,
		WithActionWorkers(ActionWorkerConfig{Scope: scope, Concurrency: 1, PollInterval: time.Second, LeaseDuration: time.Second}, nil, pool.dispatcher()),
		WithDynamicActionWorkers(DynamicActionWorkerConfig{Concurrency: 1, PollInterval: time.Second, LeaseDuration: time.Second}, sources, nil, supervisor.dispatcher()),
	)
	outcome := executeLoopStatusCall(t, engine, scope)
	if supervisor.calls != 1 || pool.calls != 0 || outcome.Output["dispatchedBy"] != "supervisor" {
		t.Fatalf("supervisor calls = %d, pool calls = %d, output = %#v", supervisor.calls, pool.calls, outcome.Output)
	}
	if supervisor.input.Call == nil || supervisor.input.Call.ID != outcome.ActionCallID {
		t.Fatalf("dispatch input = %#v", supervisor.input)
	}
}

func TestEngineBuildsGovernedToolExecutorFromPoolSpec(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "loop-actions-pool"}
	pool := &loopActionsDispatcher{name: "pool"}
	engine := newLoopActionsEngine(t,
		WithActionWorkers(ActionWorkerConfig{Scope: scope, Concurrency: 1, PollInterval: time.Second, LeaseDuration: time.Second}, nil, pool.dispatcher()),
	)
	outcome := executeLoopStatusCall(t, engine, scope)
	if pool.calls != 1 || outcome.Output["dispatchedBy"] != "pool" {
		t.Fatalf("pool calls = %d, output = %#v", pool.calls, outcome.Output)
	}
}

func TestEngineWithoutActionWorkersHasNoGovernedTools(t *testing.T) {
	engine := newLoopActionsEngine(t)
	if engine.GovernedTools() != nil {
		t.Fatal("engine without action workers built governed tools")
	}
	var unset *Engine
	if unset.GovernedTools() != nil {
		t.Fatal("nil engine returned governed tools")
	}
}
