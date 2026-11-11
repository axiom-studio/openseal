package openseal

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestEngineExposesGovernedSkillCatalog(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	ctx := context.Background()
	definition := &SkillDefinition{
		ID: "analytics", Version: "1.0.0", Name: "Analytics",
		Transport: SkillTransportReference{Kind: "local"},
		Actions: map[string]SkillAction{
			"query": {
				Name: "query", Description: "Query analytics", Risk: SkillRiskRead,
				SideEffect: SkillSideEffectRead, Idempotency: SkillIdempotencySupported,
				InputSchema: map[string]interface{}{
					"type": "object", "properties": map[string]interface{}{"metric": map[string]interface{}{"type": "string"}},
					"required": []interface{}{"metric"},
				},
			},
		},
	}
	if err := engine.RegisterSkill(ctx, definition); err != nil {
		t.Fatal(err)
	}
	scope := SkillScope{Kind: "local", ID: "test"}
	if err := engine.BindSkill(ctx, &SkillBinding{
		ID: "analytics-binding", Scope: scope, DeploymentID: "analyst", SkillID: "analytics", SkillVersion: "1.0.0",
		AllowedActions: []string{"query"}, MaximumRisk: SkillRiskRead, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	actions, err := engine.ListModelSkillActions(ctx, scope, "analyst")
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Name != "analytics.query" {
		t.Fatalf("unexpected actions: %#v", actions)
	}
}

func TestEngineExposesCanonicalSkillBindingManagement(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	ctx := context.Background()
	definition := &SkillDefinition{ID: "reader", Version: "1", Name: "Reader", Transport: SkillTransportReference{Kind: "local"}, Actions: map[string]SkillAction{
		"read": {Name: "read", Description: "Read", Risk: SkillRiskRead, SideEffect: SkillSideEffectRead, Idempotency: SkillIdempotencySupported, InputSchema: map[string]interface{}{"type": "object"}},
	}}
	if err := engine.RegisterSkill(ctx, definition); err != nil {
		t.Fatal(err)
	}
	scope := SkillScope{Kind: "tenant", ID: "one"}
	owner := ensureTestAgent(t, engine, scope, "agent")
	created, err := engine.UpsertOwnerSkillBinding(ctx, owner, UpsertSkillBindingRequest{
		Binding: &SkillBinding{ID: "reader", Scope: scope, DeploymentID: "agent", SkillID: "reader", SkillVersion: "1", AllowedActions: []string{"read"}, MaximumRisk: SkillRiskRead},
		Actor:   SkillBindingActor{Type: "user", ID: "admin"}, Reason: "assign reader",
	})
	if err != nil || created.Revision != 1 {
		t.Fatalf("created binding = %#v, %v", created, err)
	}
	values, err := engine.ListSkillBindings(ctx, scope, "agent")
	if err != nil || len(values) != 1 {
		t.Fatalf("binding list = %#v, %v", values, err)
	}
	if _, err := engine.UpsertOwnerSkillBinding(ctx, ObjectiveOwner{Type: OwnerTypeTeam, ID: "agent"}, UpsertSkillBindingRequest{
		Binding: &SkillBinding{ID: "other", Scope: scope, DeploymentID: "agent", SkillID: "reader", SkillVersion: "1", AllowedActions: []string{"read"}, MaximumRisk: SkillRiskRead},
		Actor:   SkillBindingActor{Type: "user", ID: "admin"}, Reason: "wrong owner kind",
	}); err == nil {
		t.Fatal("an Agent's binding was saved as a Team's")
	}
	if _, err := engine.UpsertOwnerSkillBinding(ctx, agentOwner("ghost"), UpsertSkillBindingRequest{
		Binding: &SkillBinding{ID: "other", Scope: scope, DeploymentID: "ghost", SkillID: "reader", SkillVersion: "1", AllowedActions: []string{"read"}, MaximumRisk: SkillRiskRead},
		Actor:   SkillBindingActor{Type: "user", ID: "admin"}, Reason: "unknown owner",
	}); err == nil {
		t.Fatal("a binding was saved to an unknown umbrella")
	}
	disabled, err := engine.DisableOwnerSkillBinding(ctx, owner, DisableSkillBindingRequest{Scope: scope, DeploymentID: "agent", BindingID: "reader", ExpectedRevision: 1, Actor: SkillBindingActor{Type: "user", ID: "admin"}, Reason: "retire reader"})
	if err != nil || !disabled.Disabled || disabled.Revision != 2 {
		t.Fatalf("disabled binding = %#v, %v", disabled, err)
	}
	deleted, err := engine.DeleteOwnerSkillBinding(ctx, owner, DeleteSkillBindingRequest{Scope: scope, DeploymentID: "agent", BindingID: "reader", ExpectedRevision: 2, Actor: SkillBindingActor{Type: "user", ID: "admin"}, Reason: "remove reader"})
	if err != nil || deleted.Lifecycle[len(deleted.Lifecycle)-1].Action != SkillBindingLifecycleDeleted {
		t.Fatalf("deleted binding = %#v, %v", deleted, err)
	}
	if values, err := engine.ListSkillBindings(ctx, scope, "agent"); err != nil || len(values) != 0 {
		t.Fatalf("bindings after delete = %#v, %v", values, err)
	}
	if _, err := engine.DeleteOwnerSkillBinding(ctx, ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, DeleteSkillBindingRequest{Scope: scope, DeploymentID: "agent", BindingID: "reader", ExpectedRevision: 1, Actor: SkillBindingActor{Type: "user", ID: "admin"}, Reason: "remove"}); err == nil {
		t.Fatal("Team delete accepted another owner's binding")
	}
}

func TestEngineExposesGovernedActionAndApprovalLifecycle(t *testing.T) {
	engine, err := New(WithActionWorkers(ActionWorkerConfig{Scope: Scope{Kind: "local", ID: "governance"}, Concurrency: 1, PollInterval: time.Second, LeaseDuration: time.Second}, nil,
		ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
			t.Fatal("a deploy waiting for approval was dispatched")
			return nil, nil
		})))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "governance"}
	definition := &SkillDefinition{
		ID: "release", Version: "1.0.0", Name: "Release", Description: "Release safely",
		Transport: SkillTransportReference{Kind: "local"}, Prompt: &SkillPromptModule{Instructions: "Prepare release evidence.", UserInvocable: true},
		Actions: map[string]SkillAction{"deploy": {
			Name: "deploy", Description: "Deploy a release", Risk: SkillRiskProduction, SideEffect: SkillSideEffectExternal,
			Idempotency: SkillIdempotencyRequired, Retry: SkillActionRetryPolicy{MaxAttempts: 1},
			InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"environment": map[string]interface{}{"type": "string"}}, "required": []interface{}{"environment"}},
		}},
	}
	if err := engine.RegisterSkill(ctx, definition); err != nil {
		t.Fatal(err)
	}
	binding := &SkillBinding{ID: "release", Scope: SkillScope{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "release-agent", SkillID: "release", SkillVersion: "1.0.0", AllowedActions: []string{"deploy"}, EnablePrompt: true, MaximumRisk: SkillRiskProduction, Revision: 1}
	if err := engine.BindSkill(ctx, binding); err != nil {
		t.Fatal(err)
	}
	prompts, err := engine.ListModelSkillPrompts(ctx, binding.Scope, "release-agent")
	if err != nil || len(prompts) != 1 || prompts[0].SkillID != "release" {
		t.Fatalf("prompt projection = %#v, %v", prompts, err)
	}
	run, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "release-agent"}, AssignedAgentID: "release-agent", Goal: "deploy", Source: RunSourceObjective})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := engine.ClaimNextAgentRun(ctx, AgentRunClaimRequest{Scope: scope, WorkerID: "worker", AssignedAgentID: "release-agent", LeaseDuration: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	deploy := definition.Actions["deploy"]
	outcome, err := engine.GovernedTools().Execute(ctx, GovernedToolCall{
		Run: claimed, Step: &AgentTurn{ID: "step-1", Scope: scope, RunID: run.ID}, WorkerID: "worker",
		Binding: &TurnRunnerBinding{DeploymentID: "release-agent", ModelActions: []capability.ModelAction{{
			Name: "release_deploy", BindingID: binding.ID, BindingRevision: binding.Revision, SkillID: "release", Version: "1.0.0", Action: "deploy",
			SideEffect: deploy.SideEffect, Risk: deploy.Risk, InputSchema: deploy.InputSchema,
		}}},
		ToolCallID: "tool-1", Name: "release_deploy",
		Arguments: json.RawMessage(`{"arguments":{"environment":"production"},"reviewContext":{"summary":"Deploy the release to production","content":"release 1.0.0"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.State != runtime.ToolEntryAwaitingApproval || outcome.Wait == nil || outcome.Wait.ApprovalID == "" {
		t.Fatalf("outcome = %#v", outcome)
	}
	approval, err := engine.GetApproval(ctx, scope, outcome.Wait.ApprovalID)
	if err != nil {
		t.Fatal(err)
	}
	resolution, err := engine.ResolveApproval(ctx, ResolveApprovalRequest{
		Scope: scope, ApprovalID: approval.ID, ExpectedRevision: approval.Revision,
		DecisionID: "operator-decision", Decision: ApprovalDecisionApprove, Principal: ApprovalPrincipal{Type: "role", ID: "operator"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Call.ID != outcome.ActionCallID || resolution.Call.Status != ActionCallStatusReady {
		t.Fatalf("resolution = %#v", resolution)
	}
	approvals, err := engine.ListApprovals(ctx, ApprovalFilter{Scope: scope, RunID: run.ID})
	if err != nil || len(approvals) != 1 || approvals[0].Status != ApprovalStatusApproved {
		t.Fatalf("approvals = %#v, %v", approvals, err)
	}
}

func TestPublicFacadeCompilesAndExportsOpenClawSkill(t *testing.T) {
	source := []byte("---\nname: facade-skill\ndescription: Test the public facade.\n---\nUse the portable instructions.\n")
	compilation, err := CompileOpenClawSkill(OpenClawSkillBundle{SkillMD: source})
	if err != nil {
		t.Fatal(err)
	}
	exported, err := ExportOpenClawSkill(compilation)
	if err != nil {
		t.Fatal(err)
	}
	if string(exported.SkillMD) != string(source) || compilation.Definition.ID != "facade-skill" {
		t.Fatalf("public OpenClaw conversion lost semantics: %#v", compilation)
	}
}
