package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestAgentRunWorkerRetriesAcceptedActionAfterPromotionDuringPolicy(t *testing.T) {
	store := NewMemoryStore()
	run, _, source := continuationLifecycleFixture(t, store, "action-preparation-continuation")
	catalog := skill.NewCatalog()
	if err := catalog.Register(t.Context(), &skill.Definition{
		ID: "review", Version: "1", Name: "Review", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://review.invalid"},
		Actions: map[string]skill.Action{"inspect": {Name: "inspect", Description: "Inspect the release", SideEffect: skill.SideEffectRead,
			Risk: skill.RiskLevelRead, Idempotency: skill.IdempotencySupported,
			InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"release": map[string]interface{}{"type": "string"}}, "required": []interface{}{"release"}}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(t.Context(), &skill.Binding{ID: "review-binding", Scope: skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID},
		DeploymentID: run.AssignedAgentID, SkillID: "review", SkillVersion: "1", AllowedActions: []string{"inspect"}, MaximumRisk: skill.RiskLevelRead, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	modelActions, err := catalog.ListModelActions(t.Context(), capability.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, run.AssignedAgentID)
	if err != nil {
		t.Fatal(err)
	}
	modelCalls := 0
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{DeploymentID: run.AssignedAgentID, ModelActions: modelActions,
			Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
				modelCalls++
				return &TurnOutcome{NextRunStatus: AgentRunStatusRunning, ContinuationCheckpoint: map[string]interface{}{"actionInputs": map[string]interface{}{"inspect": map[string]interface{}{"release": "canary"}}},
					ProposedActions: []TurnAction{{Type: "skill_action", Capability: "review.inspect", Summary: "Inspect the requested release", InputRef: "/actionInputs/inspect", IdempotencyKey: "accepted-review"}}}, nil
			})}, nil
	}), nil, AgentRunWorkerConfig{Scope: run.Scope, Kind: RunKindConversation, MaxTurnsPerClaim: 1})
	if err != nil {
		t.Fatal(err)
	}
	policyCalls := 0
	logicalNow := run.CreatedAt.Add(ConversationTaskForegroundTimeout + time.Second)
	actions := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(ctx context.Context, input ActionPolicyInput) (ActionPolicyDecision, error) {
		policyCalls++
		if policyCalls == 1 {
			result, err := store.PromoteConversationTask(ctx, ConversationTaskPromotionRequest{Scope: run.Scope, RunID: run.ID, Now: logicalNow})
			if err != nil || result == nil {
				t.Fatalf("promotion while policy held the prior Run revision: %#v %v", result, err)
			}
		}
		return ActionPolicyDecision{Disposition: ActionDispositionRequireApproval, Reason: "Review requested approval", EligibleApprovers: []ApprovalPrincipal{{Type: "user", ID: source.Sender.ID}}}, nil
	}))
	actions.now = func() time.Time { return logicalNow }
	pool.SetActionCoordinator(actions)
	claimed, err := store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: run.Scope, Kind: RunKindConversation, WorkerID: "action-worker", Now: time.Now(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil {
		t.Fatalf("claim action source: %#v %v", claimed, err)
	}
	pool.executeClaim(t.Context(), "action-worker", claimed)
	current, err := store.GetAgentRun(t.Context(), run.Scope, run.ID)
	if err != nil || current.Status != AgentRunStatusWaitingForApproval || current.LastAppliedTurn != 1 || modelCalls != 1 || policyCalls != 2 {
		t.Fatalf("accepted action did not survive promotion without another model Turn: %#v model=%d policy=%d err=%v", current, modelCalls, policyCalls, err)
	}
	task, err := store.FindConversationTaskByWorkRunID(t.Context(), run.Scope, run.ID)
	if err != nil || !ConversationTaskMatchesWorkRun(task, current) {
		t.Fatalf("approval lost adopted work identity: %#v %v", task, err)
	}
	calls, err := store.ListActionCalls(t.Context(), ActionFilter{Scope: run.Scope, RunID: run.ID})
	if err != nil || len(calls) != 1 || calls[0].Status != ActionCallStatusWaitingApproval || calls[0].IdempotencyKey == "" {
		t.Fatalf("action proposal duplicated: %#v %v", calls, err)
	}
	approvals, err := store.ListApprovals(t.Context(), ApprovalFilter{Scope: run.Scope, RunID: run.ID})
	if err != nil || len(approvals) != 1 || approvals[0].ActionCallID != calls[0].ID {
		t.Fatalf("approval proposal duplicated: %#v %v", approvals, err)
	}
	turns, err := store.ListAgentTurns(t.Context(), AgentTurnFilter{Scope: run.Scope, RunID: run.ID})
	if err != nil || len(turns) != 1 || turns[0].ID != calls[0].TurnID {
		t.Fatalf("durable Turn repeated during action preparation: %#v %v", turns, err)
	}
}
