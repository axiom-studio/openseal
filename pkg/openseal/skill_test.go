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
	run, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "release-team"}, AssignedAgentID: "release-agent", Goal: "deploy", Source: RunSourceObjective})
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
	if resolution.Call.Status != ActionCallStatusReady || resolution.Run.Status != AgentRunStatusQueued {
		t.Fatalf("resolution = %#v", resolution)
	}
	approvals, err := engine.ListApprovals(ctx, ApprovalFilter{Scope: scope, RunID: run.ID})
	if err != nil || len(approvals) != 1 || approvals[0].Status != ApprovalStatusApproved {
		t.Fatalf("approvals = %#v, %v", approvals, err)
	}
}
