package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestTurnActionIdempotencyRefreshesReadsAcrossTurns(t *testing.T) {
	const modelKey = "same-model-key"
	first := turnActionIdempotencyKey(modelKey, capability.SideEffectRead, "turn-before-navigation")
	retry := turnActionIdempotencyKey(modelKey, capability.SideEffectRead, "turn-before-navigation")
	refresh := turnActionIdempotencyKey(modelKey, capability.SideEffectRead, "turn-after-navigation")
	if first != retry || first == refresh {
		t.Fatalf("read key must replay within one Turn and refresh in a later Turn: %q %q %q", first, retry, refresh)
	}
	if turnActionIdempotencyKey(modelKey, capability.SideEffectNone, "turn-after-navigation") != refresh {
		t.Fatal("side-effect-free observations must follow read idempotency")
	}
	if turnActionIdempotencyKey(modelKey, capability.SideEffectWrite, "turn-after-navigation") != modelKey ||
		turnActionIdempotencyKey(modelKey, capability.SideEffectExternal, "turn-after-navigation") != modelKey {
		t.Fatal("mutating actions lost their stable semantic idempotency key")
	}
}

func TestAgentRunWorkerMaterializesFreshReadAfterEarlierTurn(t *testing.T) {
	ctx := t.Context()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "read-refresh"}
	catalog := skill.NewCatalog()
	if err := catalog.Register(ctx, &skill.Definition{
		ID: "browser", Version: "1", Name: "Browser", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://browser.invalid"},
		Actions: map[string]skill.Action{"snapshot": {
			Name: "snapshot", Description: "Read the current page", SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead, Idempotency: skill.IdempotencySupported,
			InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"sessionId": map[string]interface{}{"type": "string"}}, "required": []interface{}{"sessionId"}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "browser-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "agent",
		SkillID: "browser", SkillVersion: "1", AllowedActions: []string{"snapshot"}, MaximumRisk: skill.RiskLevelRead, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	modelActions, err := catalog.ListModelActions(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, "agent")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(time.Second)
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "Read the page twice after navigation", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	actions := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionDeny, Reason: "stop before transport execution"}, nil
	}))
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return nil, nil
	}), nil, AgentRunWorkerConfig{Scope: scope, AssignedAgentID: "agent", Concurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	pool.SetActionCoordinator(actions)
	for _, turnID := range []string{"before-navigation", "after-navigation"} {
		claimed, claimErr := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
		if claimErr != nil || claimed == nil || claimed.ID != run.ID {
			t.Fatalf("claim %s: run=%#v err=%v", turnID, claimed, claimErr)
		}
		turn := &AgentTurn{ID: turnID, RequestedActions: []TurnAction{{
			Type: "skill_action", Capability: "browser.snapshot", Summary: "Read page", IdempotencyKey: "same-model-key", InputRef: "/actionInputs/read",
		}}, ContinuationCheckpoint: map[string]interface{}{"actionInputs": map[string]interface{}{"read": map[string]interface{}{"sessionId": "session"}}}}
		if _, err := pool.materializeTurnAction(ctx, "worker", claimed, turn, &TurnRunnerBinding{DeploymentID: "agent", ModelActions: modelActions}); err != nil {
			t.Fatalf("materialize %s: %v", turnID, err)
		}
	}
	calls, err := store.ListActionCalls(ctx, ActionFilter{Scope: scope, RunID: run.ID})
	if err != nil || len(calls) != 2 || calls[0].ID == calls[1].ID || calls[0].IdempotencyKey == calls[1].IdempotencyKey {
		t.Fatalf("later read replayed stale call: calls=%#v err=%v", calls, err)
	}
}
