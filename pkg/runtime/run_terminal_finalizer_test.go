package runtime

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"go.uber.org/zap"
)

type terminalFinalizerStore struct {
	*MemoryStore
	activityFilters []ActivityFilter
}

func (s *terminalFinalizerStore) ListActivity(ctx context.Context, filter ActivityFilter) ([]*ActivityEvent, error) {
	s.activityFilters = append(s.activityFilters, filter)
	return s.MemoryStore.ListActivity(ctx, filter)
}

func TestActionRunTerminalFinalizerReleasesSucceededAcquisitionExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store := &terminalFinalizerStore{MemoryStore: NewMemoryStore()}
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
	if len(store.activityFilters) != 2 {
		t.Fatalf("activity filters = %d, want one per finalization attempt", len(store.activityFilters))
	}
	for _, filter := range store.activityFilters {
		if !filter.Descending || len(filter.EventTypes) != 2 || filter.EventTypes[0] != runResourcesReleasedEvent || filter.EventTypes[1] != runResourcesQuarantinedEvent {
			t.Fatalf("terminal finalizer activity filter = %#v", filter)
		}
	}
	events, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: run.ID, EventTypes: []string{runResourcesReleasedEvent}, Limit: 10})
	if err != nil || len(events) != 1 {
		t.Fatalf("cleanup events = %#v, %v", events, err)
	}
}

func TestActionRunTerminalFinalizerQuarantinesUnavailableHistoricalBindingOnce(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	catalog := skill.NewCatalog()
	scope := Scope{Kind: "tenant", ID: "one"}
	definition := &skill.Definition{
		ID: "session", Version: "1.0.0", Name: "Session", Transport: skill.TransportReference{Kind: "test"},
		Actions: map[string]skill.Action{
			"start": {
				Name: "start", Description: "Acquire session", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead,
				InputSchema:  map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{}},
				OutputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"sessionId": map[string]interface{}{"type": "string"}}, "required": []interface{}{"sessionId"}},
				Retry:        skill.ActionRetryPolicy{MaxAttempts: 1}, Idempotency: skill.IdempotencySupported, FinalizerAction: "release",
			},
			"release": {
				Name: "release", Description: "Release session", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectNone,
				InputSchema:  map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"sessionId": map[string]interface{}{"type": "string"}}, "required": []interface{}{"sessionId"}},
				OutputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"released": map[string]interface{}{"type": "boolean"}}, "required": []interface{}{"released"}},
				Retry:        skill.ActionRetryPolicy{MaxAttempts: 1}, Idempotency: skill.IdempotencySupported,
			},
		},
	}
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	binding := &skill.Binding{ID: "binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "agent", SkillID: definition.ID, SkillVersion: definition.Version, AllowedActions: []string{"start", "release"}, MaximumRisk: skill.RiskLevelRead, Revision: 1}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	updated := *binding
	updated.Revision = 2
	if err := catalog.Bind(ctx, &updated); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 31, 19, 0, 0, 0, time.UTC)
	run := &AgentRun{ID: "historical", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "cleanup", Source: RunSourceObjective, Status: AgentRunStatusCanceled, CreatedAt: now, UpdatedAt: now}
	store.agentRuns[portfolioKey(scope, run.ID)] = cloneAgentRun(run)
	store.actions[portfolioKey(scope, "start-call")] = &ActionCall{ID: "start-call", Scope: scope, RunID: run.ID, DeploymentID: "agent", BindingID: binding.ID, BindingRevision: 1, SkillID: definition.ID, SkillVersion: definition.Version, Action: "start", Status: ActionCallStatusSucceeded, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Output: map[string]interface{}{"sessionId": "old-session"}, MaxAttempts: 1, Attempt: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now}
	dispatches := 0
	finalizer := NewActionRunTerminalFinalizer(store, catalog, ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		dispatches++
		return map[string]interface{}{"released": true}, nil
	}))
	finalizer.now = func() time.Time { return now.Add(time.Second) }
	if err := finalizer.FinalizeRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := finalizer.FinalizeRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if dispatches != 0 {
		t.Fatalf("stale finalizer dispatched %d times", dispatches)
	}
	events, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: run.ID, EventTypes: []string{runResourcesQuarantinedEvent}, Limit: 10})
	if err != nil || len(events) != 1 {
		t.Fatalf("quarantine events = %#v, %v", events, err)
	}
	if fmt.Sprint(events[0].Payload["bindingRevision"]) != "1" {
		t.Fatalf("quarantine payload = %#v", events[0].Payload)
	}
}

func TestTerminalRunFinalizerReconciliationPagesNewestFirst(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "one"}
	start := time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC)
	for index := range 205 {
		run := &AgentRun{
			ID: fmt.Sprintf("run-%03d", index), Scope: scope,
			Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
			Goal: "terminal cleanup", Source: RunSourceObjective, Status: AgentRunStatusCompleted,
			CreatedAt: start.Add(time.Duration(index) * time.Second),
			UpdatedAt: start.Add(time.Duration(index) * time.Second),
		}
		store.agentRuns[portfolioKey(scope, run.ID)] = run
	}
	finalized := make([]string, 0, 206)
	seen := make(map[string]bool, 206)
	pool := &AgentRunWorkerPool{
		config:    AgentRunWorkerConfig{Scope: scope},
		portfolio: store,
		runFinalizer: RunTerminalFinalizerFunc(func(_ context.Context, run *AgentRun) error {
			if seen[run.ID] {
				return nil
			}
			seen[run.ID] = true
			finalized = append(finalized, run.ID)
			return nil
		}),
		logger: zap.NewNop().Sugar(),
	}
	pool.lastFinalizationScan = time.Time{}
	pool.reconcileTerminalRunFinalizers(ctx)
	newest := &AgentRun{
		ID: "run-205", Scope: scope,
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "newly terminal cleanup", Source: RunSourceObjective, Status: AgentRunStatusCanceled,
		CreatedAt: start.Add(205 * time.Second), UpdatedAt: start.Add(205 * time.Second),
	}
	store.agentRuns[portfolioKey(scope, newest.ID)] = newest
	for range 20 {
		pool.lastFinalizationScan = time.Time{}
		pool.reconcileTerminalRunFinalizers(ctx)
	}
	if len(finalized) != 206 {
		t.Fatalf("finalized %d Runs, want 206", len(finalized))
	}
	if finalized[0] != "run-204" || finalized[9] != "run-195" ||
		finalized[10] != "run-205" || finalized[205] != "run-000" {
		t.Fatalf("unexpected finalization order: first=%s newest=%s last=%s", finalized[0], finalized[10], finalized[205])
	}
	if pool.finalizationOffset != 0 {
		t.Fatalf("finalization offset = %d, want reset after final page", pool.finalizationOffset)
	}
}

func TestTerminalRunReconciliationResolvesCanceledDelegatedChild(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "canceled-child"}
	portfolio := NewPortfolioService(store)
	parent, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "scheduler"}, AssignedAgentID: "scheduler",
		Goal: "run scheduled delegated work", Source: RunSourceSchedule,
	})
	if err != nil {
		t.Fatal(err)
	}
	collaboration := NewCollaborationService(store)
	created, err := collaboration.CreateAgentRequest(ctx, CreateAgentRequestRequest{
		Scope: scope, Kind: AgentRequestKindRequest, SourceRunID: parent.ID,
		Requester: CollaborationParty{Type: OwnerTypeAgent, ID: "scheduler"},
		Recipient: CollaborationParty{Type: OwnerTypeAgent, ID: "worker"}, Goal: "perform delegated work",
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := collaboration.RespondAgentRequest(ctx, RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: AgentRequestDecisionAccept, Principal: CollaborationParty{Type: OwnerTypeAgent, ID: "worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := NewRunCommandService(store).CommandAgentRun(ctx, AgentRunCommandRequest{
		Scope: scope, RunID: accepted.Child.ID, ExpectedRevision: accepted.Child.Revision, Kind: AgentRunCommandCancel,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool := &AgentRunWorkerPool{
		config: AgentRunWorkerConfig{Scope: scope}, portfolio: store, collaboration: collaboration,
		logger: zap.NewNop().Sugar(),
	}
	pool.reconcileTerminalRunFinalizers(ctx)
	request, err := collaboration.GetAgentRequest(ctx, scope, created.Request.ID)
	if err != nil {
		t.Fatal(err)
	}
	resolvedParent, err := portfolio.GetAgentRun(ctx, scope, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if canceled.Run.Status != AgentRunStatusCanceled || request.Status != AgentRequestStatusCanceled ||
		resolvedParent.Status == AgentRunStatusWaitingForDependency {
		t.Fatalf("canceled=%s request=%s parent=%s", canceled.Run.Status, request.Status, resolvedParent.Status)
	}
	// Reconciliation is replay-safe after the request and parent are resolved.
	pool.lastFinalizationScan = time.Time{}
	pool.reconcileTerminalRunFinalizers(ctx)
}
