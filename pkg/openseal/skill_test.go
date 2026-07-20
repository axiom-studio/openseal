package openseal

import (
	"context"
	"testing"
	"time"
)

func TestEngineExposesGovernedSkillCatalog(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
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
	ctx := context.Background()
	definition := &SkillDefinition{ID: "reader", Version: "1", Name: "Reader", Transport: SkillTransportReference{Kind: "local"}, Actions: map[string]SkillAction{
		"read": {Name: "read", Description: "Read", Risk: SkillRiskRead, SideEffect: SkillSideEffectRead, Idempotency: SkillIdempotencySupported, InputSchema: map[string]interface{}{"type": "object"}},
	}}
	if err := engine.RegisterSkill(ctx, definition); err != nil {
		t.Fatal(err)
	}
	scope := SkillScope{Kind: "tenant", ID: "one"}
	created, err := engine.UpsertSkillBinding(ctx, UpsertSkillBindingRequest{
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
	disabled, err := engine.DisableSkillBinding(ctx, DisableSkillBindingRequest{Scope: scope, DeploymentID: "agent", BindingID: "reader", ExpectedRevision: 1, Actor: SkillBindingActor{Type: "user", ID: "admin"}, Reason: "retire reader"})
	if err != nil || !disabled.Disabled || disabled.Revision != 2 {
		t.Fatalf("disabled binding = %#v, %v", disabled, err)
	}
}

func TestEngineExposesGovernedActionAndApprovalLifecycle(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
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
	proposal, err := engine.ProposeAction(ctx, ProposeActionRequest{
		Scope: scope, RunID: run.ID, WorkerID: "worker", DeploymentID: "release-agent", SkillID: "release", SkillVersion: "1.0.0", Action: "deploy",
		Arguments: map[string]interface{}{"environment": "production"}, IdempotencyKey: "release-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Approval == nil || proposal.Call.Status != ActionCallStatusWaitingApproval {
		t.Fatalf("proposal = %#v", proposal)
	}
	resolution, err := engine.ResolveApproval(ctx, ResolveApprovalRequest{
		Scope: scope, ApprovalID: proposal.Approval.ID, ExpectedRevision: proposal.Approval.Revision,
		DecisionID: "operator-decision", Approve: true, Principal: ApprovalPrincipal{Type: "role", ID: "operator"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Call.Status != ActionCallStatusReady || resolution.Run.Status != AgentRunStatusWaitingForDependency {
		t.Fatalf("resolution = %#v", resolution)
	}
	approvals, err := engine.ListApprovals(ctx, ApprovalFilter{Scope: scope, RunID: run.ID})
	if err != nil || len(approvals) != 1 || approvals[0].Status != ApprovalStatusApproved {
		t.Fatalf("approvals = %#v, %v", approvals, err)
	}
}

func TestEngineRunsDurableActionWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scope := Scope{Kind: "local", ID: "action-workers"}
	engine, err := New(
		WithActionPolicy(ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
			return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
		})),
		WithActionWorkers(ActionWorkerConfig{Scope: scope, Concurrency: 1, PollInterval: time.Second, LeaseDuration: time.Second}, nil,
			ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
				return map[string]interface{}{"result": "published"}, nil
			})),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Stop()
	definition := &SkillDefinition{
		ID: "publisher", Version: "1.0.0", Name: "Publisher", Transport: SkillTransportReference{Kind: "local"},
		Actions: map[string]SkillAction{"publish": {
			Name: "publish", Description: "Publish an artifact", Risk: SkillRiskExternal, SideEffect: SkillSideEffectExternal,
			Idempotency: SkillIdempotencyRequired, Retry: SkillActionRetryPolicy{MaxAttempts: 2},
			InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"artifact": map[string]interface{}{"type": "string"}}, "required": []interface{}{"artifact"}},
		}},
	}
	if err := engine.RegisterSkill(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := engine.BindSkill(ctx, &SkillBinding{ID: "publisher", Scope: SkillScope{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "marketing-agent", SkillID: "publisher", SkillVersion: "1.0.0", AllowedActions: []string{"publish"}, MaximumRisk: SkillRiskExternal, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	run, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "marketing-agent"}, AssignedAgentID: "marketing-agent", Goal: "publish", Source: RunSourceObjective})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := engine.ClaimNextAgentRun(ctx, AgentRunClaimRequest{Scope: scope, WorkerID: "agent-worker", AssignedAgentID: "marketing-agent", LeaseDuration: time.Minute})
	if err != nil || claimed == nil {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	proposal, err := engine.ProposeAction(ctx, ProposeActionRequest{Scope: scope, RunID: run.ID, WorkerID: "agent-worker", DeploymentID: "marketing-agent", SkillID: "publisher", SkillVersion: "1.0.0", Action: "publish", Arguments: map[string]interface{}{"artifact": "artifact://report"}, IdempotencyKey: "publish-report"})
	if err != nil {
		t.Fatal(err)
	}
	engine.Start(ctx)
	engine.WakeActionWorkers()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		call, err := engine.GetActionCall(ctx, scope, proposal.Call.ID)
		if err != nil {
			t.Fatal(err)
		}
		currentRun, err := engine.GetAgentRun(ctx, scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if call.Status == ActionCallStatusSucceeded && currentRun.Status == AgentRunStatusQueued {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("background action worker did not execute and resume the run")
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
