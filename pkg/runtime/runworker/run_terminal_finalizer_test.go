package runworker

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimecollaboration "github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
	"go.uber.org/zap"
)

// terminalFinalizerStore is a kernel store whose action calls are the ones a
// test states (the finalizer only lists a Run's calls), and which records the
// activity filters the finalizer asks for.
type terminalFinalizerStore struct {
	*sqlite.Store
	actions         workerActionCalls
	activityFilters []models.ActivityFilter
}

func (s *terminalFinalizerStore) ListActionCalls(ctx context.Context, filter models.ActionFilter) ([]*models.ActionCall, error) {
	return s.actions.ListActionCalls(ctx, filter)
}

func (s *terminalFinalizerStore) ListActivity(ctx context.Context, filter models.ActivityFilter) ([]*models.ActivityEvent, error) {
	s.activityFilters = append(s.activityFilters, filter)
	return s.Store.ListActivity(ctx, filter)
}

func TestActionRunTerminalFinalizerReleasesSucceededAcquisitionExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store := &terminalFinalizerStore{Store: kerneltest.NewStore(t)}
	catalog := skill.NewCatalog()
	scope := models.Scope{Kind: "tenant", ID: "one"}
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
	portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	run, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "use session", Source: models.RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: "worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	store.actions.put(&models.ActionCall{
		ID: "start-call", Scope: scope, RunID: run.ID, DeploymentID: "agent", BindingID: "binding", BindingRevision: 1,
		SkillID: definition.ID, SkillVersion: definition.Version, Action: "start", Status: models.ActionCallStatusSucceeded,
		Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Arguments: map[string]interface{}{},
		Output: map[string]interface{}{"sessionId": "agent-session"}, MaxAttempts: 1, Attempt: 1, AvailableAt: now,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	})
	store.actions.put(&models.ActionCall{
		ID: "reuse-call", Scope: scope, RunID: run.ID, DeploymentID: "agent", BindingID: "binding", BindingRevision: 1,
		SkillID: definition.ID, SkillVersion: definition.Version, Action: "start", Status: models.ActionCallStatusSucceeded,
		Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Arguments: map[string]interface{}{},
		Output: map[string]interface{}{"sessionId": "agent-session"}, MaxAttempts: 1, Attempt: 1, AvailableAt: now,
		Revision: 1, CreatedAt: now.Add(time.Millisecond), UpdatedAt: now.Add(time.Millisecond),
	})
	activity := runtimeactivity.NewRunActivityService(store, store, models.WithClock(func() time.Time { return now.Add(time.Second) }))
	terminal, _, err := activity.TransitionRun(ctx, scope, claimed.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: claimed.Revision, Status: models.AgentRunStatusCompleted, LeaseOwner: "worker",
		Summary: "done", Actor: models.ActivityActor{Type: "worker", ID: "worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatches := 0
	finalizer := agentrun.NewActionRunTerminalFinalizer(store, catalog, action.ActionDispatcherFunc(func(_ context.Context, input agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		dispatches++
		if input.Bound.Action.Name != "release" || input.Arguments["sessionId"] != "agent-session" ||
			input.Run.ID != terminal.ID {
			t.Fatalf("finalizer dispatch = %#v", input)
		}
		return map[string]interface{}{"released": true}, nil
	}), models.WithClock(func() time.Time { return now.Add(2 * time.Second) }))
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
		if !filter.Descending || len(filter.EventTypes) != 2 || filter.EventTypes[0] != "run.resources_released" || filter.EventTypes[1] != "run.resources_release_quarantined" {
			t.Fatalf("terminal finalizer activity filter = %#v", filter)
		}
	}
	events, err := store.ListActivity(ctx, models.ActivityFilter{Scope: scope, RunID: run.ID, EventTypes: []string{"run.resources_released"}, Limit: 10})
	if err != nil || len(events) != 1 {
		t.Fatalf("cleanup events = %#v, %v", events, err)
	}
}

func TestActionRunTerminalFinalizerQuarantinesUnavailableHistoricalBindingOnce(t *testing.T) {
	ctx := context.Background()
	store := &terminalFinalizerStore{Store: kerneltest.NewStore(t)}
	catalog := skill.NewCatalog()
	scope := models.Scope{Kind: "tenant", ID: "one"}
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
	run := &models.AgentRun{ID: "historical", Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "cleanup", Source: models.RunSourceObjective, Status: models.AgentRunStatusCanceled, CreatedAt: now, UpdatedAt: now}
	if err := store.CreateAgentRun(ctx, models.CloneAgentRun(run)); err != nil {
		t.Fatal(err)
	}
	store.actions.put(&models.ActionCall{ID: "start-call", Scope: scope, RunID: run.ID, DeploymentID: "agent", BindingID: binding.ID, BindingRevision: 1, SkillID: definition.ID, SkillVersion: definition.Version, Action: "start", Status: models.ActionCallStatusSucceeded, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Output: map[string]interface{}{"sessionId": "old-session"}, MaxAttempts: 1, Attempt: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now})
	dispatches := 0
	finalizer := agentrun.NewActionRunTerminalFinalizer(store, catalog, action.ActionDispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		dispatches++
		return map[string]interface{}{"released": true}, nil
	}), models.WithClock(func() time.Time { return now.Add(time.Second) }))
	if err := finalizer.FinalizeRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := finalizer.FinalizeRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if dispatches != 0 {
		t.Fatalf("stale finalizer dispatched %d times", dispatches)
	}
	events, err := store.ListActivity(ctx, models.ActivityFilter{Scope: scope, RunID: run.ID, EventTypes: []string{"run.resources_release_quarantined"}, Limit: 10})
	if err != nil || len(events) != 1 {
		t.Fatalf("quarantine events = %#v, %v", events, err)
	}
	if fmt.Sprint(events[0].Payload["bindingRevision"]) != "1" {
		t.Fatalf("quarantine payload = %#v", events[0].Payload)
	}
}

func TestTerminalRunFinalizerReconciliationPagesNewestFirst(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "one"}
	start := time.Date(2026, 7, 31, 8, 0, 0, 0, time.UTC)
	for index := range 205 {
		run := &models.AgentRun{
			ID: fmt.Sprintf("run-%03d", index), Scope: scope,
			Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
			Goal: "terminal cleanup", Source: models.RunSourceObjective, Status: models.AgentRunStatusCompleted,
			CreatedAt: start.Add(time.Duration(index) * time.Second),
			UpdatedAt: start.Add(time.Duration(index) * time.Second),
		}
		if err := store.CreateAgentRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	finalized := make([]string, 0, 206)
	seen := make(map[string]bool, 206)
	pool := &AgentRunWorkerPool{
		config:    AgentRunWorkerConfig{Scope: scope},
		portfolio: store, now: time.Now,
		runFinalizer: agentrun.RunTerminalFinalizerFunc(func(_ context.Context, run *models.AgentRun) error {
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
	newest := &models.AgentRun{
		ID: "run-205", Scope: scope,
		Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "newly terminal cleanup", Source: models.RunSourceObjective, Status: models.AgentRunStatusCanceled,
		CreatedAt: start.Add(205 * time.Second), UpdatedAt: start.Add(205 * time.Second),
	}
	if err := store.CreateAgentRun(ctx, newest); err != nil {
		t.Fatal(err)
	}
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
	if offset, due := terminalRunSweeps.next(pool.finalizationKey(), time.Now()); offset != 0 || due {
		t.Fatalf("finalization sweep = offset %d due %t, want completed after final page", offset, due)
	}
}

func TestTerminalRunReconciliationResolvesCanceledDelegatedChild(t *testing.T) {
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "canceled-child"}
	portfolio := portfolio.NewPortfolioService(store)
	parent, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "scheduler"}, AssignedAgentID: "scheduler",
		Goal: "run scheduled delegated work", Source: models.RunSourceSchedule,
	})
	if err != nil {
		t.Fatal(err)
	}
	collaboration := runtimecollaboration.NewCollaborationService(store)
	created, err := collaboration.CreateAgentRequest(ctx, runtimecollaboration.CreateAgentRequestRequest{
		Scope: scope, Kind: models.AgentRequestKindRequest, SourceRunID: parent.ID,
		Requester: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "scheduler"},
		Recipient: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "worker"}, Goal: "perform delegated work",
	})
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := collaboration.RespondAgentRequest(ctx, runtimecollaboration.RespondAgentRequestRequest{
		Scope: scope, RequestID: created.Request.ID, ExpectedRevision: created.Request.Revision,
		Decision: runtimecollaboration.AgentRequestDecisionAccept, Principal: models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "worker"},
	})
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := agentrun.NewRunCommandService(store).CommandAgentRun(ctx, agentrun.AgentRunCommandRequest{
		Scope: scope, RunID: accepted.Child.ID, ExpectedRevision: accepted.Child.Revision, Kind: agentrun.AgentRunCommandCancel,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool := &AgentRunWorkerPool{
		config: AgentRunWorkerConfig{Scope: scope}, portfolio: store, collaboration: collaboration,
		logger: zap.NewNop().Sugar(), now: time.Now,
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
	if canceled.Run.Status != models.AgentRunStatusCanceled || request.Status != models.AgentRequestStatusCanceled ||
		resolvedParent.Status == models.AgentRunStatusWaitingForDependency {
		t.Fatalf("canceled=%s request=%s parent=%s", canceled.Run.Status, request.Status, resolvedParent.Status)
	}
	// Reconciliation is replay-safe after the request and parent are resolved.
	pool.lastFinalizationScan = time.Time{}
	pool.reconcileTerminalRunFinalizers(ctx)
}

func TestTerminalRunReconciliationCancelsOrphanedDescendantsAfterRestart(t *testing.T) {
	ctx := t.Context()
	store := kerneltest.NewStore(t)
	scope := models.Scope{Kind: "tenant", ID: "terminal-cascade"}
	service := agentrun.NewRunCommandService(store)
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}
	parentResult, err := service.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: owner, AssignedAgentID: owner.ID, Goal: "parent", Source: models.RunSourceSchedule,
	})
	if err != nil {
		t.Fatal(err)
	}
	childResult, err := service.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, ParentRunID: parentResult.Run.ID, Owner: owner, AssignedAgentID: owner.ID,
		Goal: "child", Source: models.RunSourceHandoff,
	})
	if err != nil {
		t.Fatal(err)
	}
	activity := runtimeactivity.NewRunActivityService(store, store)
	running, _, err := activity.TransitionRun(ctx, scope, parentResult.Run.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: parentResult.Run.Revision, Status: models.AgentRunStatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, _, err := activity.TransitionRun(ctx, scope, running.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: models.AgentRunStatusFailed, Error: "worker stopped",
	})
	if err != nil {
		t.Fatal(err)
	}

	// A restarted worker learns about the already-terminal parent from durable
	// state and must repair descendants without relying on the original worker.
	pool := &AgentRunWorkerPool{
		config: AgentRunWorkerConfig{Scope: scope}, portfolio: store,
		runCommands: agentrun.NewRunCommandService(store), logger: zap.NewNop().Sugar(), now: time.Now,
	}
	pool.finalizeTerminalRun(ctx, failed)
	child, err := store.GetAgentRun(ctx, scope, childResult.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if child.Status != models.AgentRunStatusCanceled || child.CompletedAt == nil {
		t.Fatalf("orphaned child was not canceled during reconciliation: %#v", child)
	}
	pool.finalizeTerminalRun(ctx, failed)
}
