package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func createDependencySource(t *testing.T, store *MemoryStore, scope Scope, id string) *AgentRun {
	t.Helper()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	run := &AgentRun{
		ID: id, Scope: scope, RootRunID: id, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research"},
		AssignedAgentID: "lead", Goal: "Coordinate parallel research", Source: RunSourceManual,
		Status: AgentRunStatusQueued, Priority: 80, AvailableAt: now, QueueEnteredAt: now,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateAgentRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestMemoryDependencyCoordinatorAllFanInWakesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "one"}
	store := NewMemoryStore()
	createDependencySource(t, store, scope, "source-all")
	coordinator := NewDependencyCoordinator(store)
	coordinator.now = func() time.Time { return time.Date(2026, 7, 10, 12, 1, 0, 0, time.UTC) }

	create := CreateRunDependencyGroupRequest{
		ID: "join-all", Scope: scope, SourceRunID: "source-all", ExpectedSourceRevision: 1,
		Policy: RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
		Dependencies: []RunDependencySpec{
			{ID: "research", Kind: RunDependencyKindAgentRequest, RequestID: "request-research"},
			{ID: "analysis", Kind: RunDependencyKindAgentRequest, RequestID: "request-analysis"},
		},
		IdempotencyKey: "join-all-once", Actor: ActivityActor{Type: "agent", ID: "lead"}, Visibility: ActivityVisibilityTeam,
	}
	created, err := coordinator.CreateRunDependencyGroup(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	if created.Group.Status != RunDependencyGroupWaiting || created.Source.Status != AgentRunStatusWaitingForDependency ||
		created.Source.WakeCondition == nil || created.Source.WakeCondition.Reference != "join-all" || len(created.Events) != 1 {
		t.Fatalf("created = %#v", created)
	}
	replayed, err := coordinator.CreateRunDependencyGroup(ctx, create)
	if err != nil || !replayed.Replayed || replayed.Source.Revision != 2 || len(replayed.Events) != 0 {
		t.Fatalf("create replay = %#v, err = %v", replayed, err)
	}
	conflict := create
	conflict.Policy.FailureMode = DependencyFailureWait
	if _, err := coordinator.CreateRunDependencyGroup(ctx, conflict); !errors.Is(err, ErrDependencyConflict) {
		t.Fatalf("conflicting replay error = %v", err)
	}

	coordinator.now = func() time.Time { return time.Date(2026, 7, 10, 12, 2, 0, 0, time.UTC) }
	first, err := coordinator.ResolveRunDependency(ctx, ResolveRunDependencyRequest{
		Scope: scope, GroupID: "join-all", DependencyID: "analysis", ExpectedDependencyRevision: 1,
		State: RunDependencyStateSatisfied, Result: map[string]interface{}{"finding": "conversion increased"},
		Actor: ActivityActor{Type: "agent", ID: "analyst"}, Visibility: ActivityVisibilityTeam,
	})
	if err != nil || first.Evaluation.Wake || first.Source.Status != AgentRunStatusWaitingForDependency || len(first.Events) != 1 {
		t.Fatalf("first completion = %#v, err = %v", first, err)
	}
	coordinator.now = func() time.Time { return time.Date(2026, 7, 10, 12, 3, 0, 0, time.UTC) }
	secondRequest := ResolveRunDependencyRequest{
		Scope: scope, GroupID: "join-all", DependencyID: "research", ExpectedDependencyRevision: 1,
		State: RunDependencyStateSatisfied, Result: map[string]interface{}{"finding": "users need durable retries"},
		Actor: ActivityActor{Type: "agent", ID: "researcher"}, Visibility: ActivityVisibilityTeam,
	}
	second, err := coordinator.ResolveRunDependency(ctx, secondRequest)
	if err != nil || !second.Evaluation.Wake || second.Group.Status != RunDependencyGroupSatisfied ||
		second.Source.Status != AgentRunStatusQueued || second.Source.WakeCondition != nil || len(second.Events) != 2 {
		t.Fatalf("second completion = %#v, err = %v", second, err)
	}
	replay, err := coordinator.ResolveRunDependency(ctx, secondRequest)
	if err != nil || !replay.Replayed || replay.Source.Revision != second.Source.Revision || len(replay.Events) != 0 {
		t.Fatalf("completion replay = %#v, err = %v", replay, err)
	}
	activity, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: "source-all", Limit: 20})
	if err != nil || len(activity) != 4 {
		t.Fatalf("activity count = %d, err = %v", len(activity), err)
	}
	for index, event := range activity {
		if event.Sequence != int64(index+1) {
			t.Fatalf("activity sequence %d = %d", index, event.Sequence)
		}
	}
}

func TestMemoryDependencyCoordinatorConcurrentAnyFanInNeverDoubleWakes(t *testing.T) {
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "concurrent"}
	store := NewMemoryStore()
	createDependencySource(t, store, scope, "source-any")
	coordinator := NewDependencyCoordinator(store)
	coordinator.now = func() time.Time { return time.Date(2026, 7, 10, 13, 0, 0, 0, time.UTC) }
	_, err := coordinator.CreateRunDependencyGroup(ctx, CreateRunDependencyGroupRequest{
		ID: "join-any", Scope: scope, SourceRunID: "source-any", ExpectedSourceRevision: 1,
		Policy: RunDependencyPolicy{Mode: FanInModeAny, FailureMode: DependencyFailureFailFast},
		Dependencies: []RunDependencySpec{
			{ID: "one", Kind: RunDependencyKindAgentRequest, RequestID: "request-one"},
			{ID: "two", Kind: RunDependencyKindAgentRequest, RequestID: "request-two"},
			{ID: "three", Kind: RunDependencyKindAgentRequest, RequestID: "request-three"},
		},
		Actor: ActivityActor{Type: "agent", ID: "lead"}, Visibility: ActivityVisibilityTeam,
	})
	if err != nil {
		t.Fatal(err)
	}
	var wakes atomic.Int32
	var failures atomic.Int32
	var wg sync.WaitGroup
	for _, id := range []string{"one", "two", "three"} {
		id := id
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, resolveErr := coordinator.ResolveRunDependency(ctx, ResolveRunDependencyRequest{
				Scope: scope, GroupID: "join-any", DependencyID: id, ExpectedDependencyRevision: 1,
				State: RunDependencyStateSatisfied, Result: map[string]interface{}{"winner": id},
				Actor: ActivityActor{Type: "agent", ID: id}, Visibility: ActivityVisibilityTeam,
			})
			if resolveErr != nil {
				failures.Add(1)
				return
			}
			if result.Evaluation.Wake {
				wakes.Add(1)
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 || wakes.Load() != 1 {
		t.Fatalf("failures = %d, wakes = %d", failures.Load(), wakes.Load())
	}
	group, err := coordinator.GetRunDependencyGroup(ctx, scope, "join-any")
	if err != nil || group.Status != RunDependencyGroupSatisfied || group.Revision != 4 || group.WakeSignalID == "" {
		t.Fatalf("group = %#v, err = %v", group, err)
	}
	source, err := store.GetAgentRun(ctx, scope, "source-any")
	if err != nil || source.Status != AgentRunStatusQueued || source.Revision != 5 || source.LastWakeSignalID != group.WakeSignalID {
		t.Fatalf("source = %#v, err = %v", source, err)
	}
	activity, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: source.ID, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	groupWakeEvents := 0
	for _, event := range activity {
		if event.EventType == "dependency.group_satisfied" {
			groupWakeEvents++
		}
	}
	if groupWakeEvents != 1 {
		t.Fatalf("group wake events = %d, activity = %#v", groupWakeEvents, activity)
	}
}

func TestMemoryDependencyCoordinatorOptionalCompletionContinuesAfterWake(t *testing.T) {
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "optional"}
	store := NewMemoryStore()
	createDependencySource(t, store, scope, "source-optional")
	coordinator := NewDependencyCoordinator(store)
	optional := false
	_, err := coordinator.CreateRunDependencyGroup(ctx, CreateRunDependencyGroupRequest{
		ID: "join-optional", Scope: scope, SourceRunID: "source-optional", ExpectedSourceRevision: 1,
		Policy: RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
		Dependencies: []RunDependencySpec{
			{ID: "required", Kind: RunDependencyKindRun, TargetRunID: "child-required"},
			{ID: "optional", Kind: RunDependencyKindRun, TargetRunID: "child-optional", Required: &optional},
		},
		Actor: ActivityActor{Type: "agent", ID: "lead"}, Visibility: ActivityVisibilityTeam,
	})
	if err != nil {
		t.Fatal(err)
	}
	required, err := coordinator.ResolveRunDependency(ctx, ResolveRunDependencyRequest{
		Scope: scope, GroupID: "join-optional", DependencyID: "required", ExpectedDependencyRevision: 1,
		State: RunDependencyStateSatisfied, Actor: ActivityActor{Type: "agent", ID: "required"}, Visibility: ActivityVisibilityTeam,
	})
	if err != nil || !required.Evaluation.Wake {
		t.Fatalf("required completion = %#v, err = %v", required, err)
	}
	optionalResult, err := coordinator.ResolveRunDependency(ctx, ResolveRunDependencyRequest{
		Scope: scope, GroupID: "join-optional", DependencyID: "optional", ExpectedDependencyRevision: 1,
		State: RunDependencyStateSatisfied, Actor: ActivityActor{Type: "agent", ID: "optional"}, Visibility: ActivityVisibilityTeam,
	})
	if err != nil || optionalResult.Evaluation.Wake || optionalResult.Source.Status != AgentRunStatusQueued || optionalResult.Group.WakeSignalID != required.Group.WakeSignalID {
		t.Fatalf("optional completion = %#v, err = %v", optionalResult, err)
	}
}
