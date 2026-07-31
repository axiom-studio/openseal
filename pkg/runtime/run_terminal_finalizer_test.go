package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestActionRunTerminalFinalizerReleasesSucceededAcquisitionExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	catalog := skill.NewCatalog()
	scope := Scope{Kind: "tenant", ID: "one"}
	definition := &skill.Definition{
		ID: "session", Version: "1.0.0", Name: "Session",
		Transport: skill.TransportReference{Kind: "test"},
		Actions: map[string]skill.Action{
			"start": {
				Name: "start", Description: "Acquire session", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead,
				InputSchema:  map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{}},
				OutputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"sessionId": map[string]interface{}{"type": "string"}}, "required": []interface{}{"sessionId"}},
				Retry:        skill.ActionRetryPolicy{MaxAttempts: 1}, Idempotency: skill.IdempotencySupported, FinalizerAction: "release",
			},
			"release": {
				Name: "release", Description: "Release session usage", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectNone,
				InputSchema:  map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"sessionId": map[string]interface{}{"type": "string"}}, "required": []interface{}{"sessionId"}},
				OutputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"released": map[string]interface{}{"type": "boolean"}}, "required": []interface{}{"released"}},
				Retry:        skill.ActionRetryPolicy{MaxAttempts: 1}, Idempotency: skill.IdempotencySupported,
			},
		},
	}
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "agent",
		SkillID: definition.ID, SkillVersion: definition.Version, AllowedActions: []string{"start", "release"},
		MaximumRisk: skill.RiskLevelRead, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC)
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "use session", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.actions[portfolioKey(scope, "start-call")] = &ActionCall{
		ID: "start-call", Scope: scope, RunID: run.ID, DeploymentID: "agent", BindingID: "binding", BindingRevision: 1,
		SkillID: definition.ID, SkillVersion: definition.Version, Action: "start", Status: ActionCallStatusSucceeded,
		Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Arguments: map[string]interface{}{},
		Output: map[string]interface{}{"sessionId": "agent-session"}, MaxAttempts: 1, Attempt: 1, AvailableAt: now,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	store.mu.Unlock()
	activity := NewRunActivityService(store, store)
	activity.now = func() time.Time { return now.Add(time.Second) }
	terminal, _, err := activity.TransitionRun(ctx, scope, claimed.ID, RunTransitionRequest{
		ExpectedRevision: claimed.Revision, Status: AgentRunStatusCompleted, LeaseOwner: "worker",
		Summary: "done", Actor: ActivityActor{Type: "worker", ID: "worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatches := 0
	finalizer := NewActionRunTerminalFinalizer(store, catalog, ActionDispatcherFunc(func(_ context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
		dispatches++
		if input.Bound.Action.Name != "release" || input.Arguments["sessionId"] != "agent-session" ||
			input.Run.ID != terminal.ID {
			t.Fatalf("finalizer dispatch = %#v", input)
		}
		return map[string]interface{}{"released": true}, nil
	}))
	finalizer.now = func() time.Time { return now.Add(2 * time.Second) }
	if err := finalizer.FinalizeRun(ctx, terminal); err != nil {
		t.Fatal(err)
	}
	if err := finalizer.FinalizeRun(ctx, terminal); err != nil {
		t.Fatal(err)
	}
	if dispatches != 1 {
		t.Fatalf("finalizer dispatched %d times", dispatches)
	}
	events, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: run.ID, EventTypes: []string{runResourcesReleasedEvent}, Limit: 10})
	if err != nil || len(events) != 1 {
		t.Fatalf("cleanup events = %#v, %v", events, err)
	}
}
