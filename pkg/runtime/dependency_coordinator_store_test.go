package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func createDependencySource(t *testing.T, store *sqlite.Store, scope models.Scope, id string) *models.AgentRun {
	t.Helper()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	run := &models.AgentRun{
		ID: id, Scope: scope, RootRunID: id, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "research"},
		AssignedAgentID: "lead", Goal: "Coordinate parallel research", Source: models.RunSourceManual,
		Status: models.AgentRunStatusQueued, Priority: 80, AvailableAt: now, QueueEnteredAt: now,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateAgentRun(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestDependencyCoordinatorAllFanInWakesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "one"}
	store := kerneltest.NewStore(t)
	createDependencySource(t, store, scope, "source-all")
	coordinatorClock := func() time.Time { return time.Date(2026, 7, 10, 12, 1, 0, 0, time.UTC) }
	coordinator := portfolio.NewDependencyCoordinator(store, models.WithClock(func() time.Time { return coordinatorClock() }))

	create := models.CreateRunDependencyGroupRequest{
		ID: "join-all", Scope: scope, SourceRunID: "source-all", ExpectedSourceRevision: 1,
		Policy: models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureFailFast},
		Dependencies: []models.RunDependencySpec{
			{ID: "research", Kind: models.RunDependencyKindAgentRequest, RequestID: "request-research"},
			{ID: "analysis", Kind: models.RunDependencyKindAgentRequest, RequestID: "request-analysis"},
		},
		IdempotencyKey: "join-all-once", Actor: models.ActivityActor{Type: "agent", ID: "lead"}, Visibility: models.ActivityVisibilityTeam,
	}
	created, err := coordinator.CreateRunDependencyGroup(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	if created.Group.Status != models.RunDependencyGroupWaiting || created.Source.Status != models.AgentRunStatusWaitingForDependency ||
		created.Source.WakeCondition == nil || created.Source.WakeCondition.Reference != "join-all" || len(created.Events) != 1 {
		t.Fatalf("created = %#v", created)
	}
	replayed, err := coordinator.CreateRunDependencyGroup(ctx, create)
	if err != nil || !replayed.Replayed || replayed.Source.Revision != 2 || len(replayed.Events) != 0 {
		t.Fatalf("create replay = %#v, err = %v", replayed, err)
	}
	conflict := create
	conflict.Policy.FailureMode = models.DependencyFailureWait
	if _, err := coordinator.CreateRunDependencyGroup(ctx, conflict); !errors.Is(err, models.ErrDependencyConflict) {
		t.Fatalf("conflicting replay error = %v", err)
	}

	coordinatorClock = func() time.Time { return time.Date(2026, 7, 10, 12, 2, 0, 0, time.UTC) }
	first, err := coordinator.ResolveRunDependency(ctx, portfolio.ResolveRunDependencyRequest{
		Scope: scope, GroupID: "join-all", DependencyID: "analysis", ExpectedDependencyRevision: 1,
		State: models.RunDependencyStateSatisfied, Result: map[string]interface{}{"finding": "conversion increased"},
		Actor: models.ActivityActor{Type: "agent", ID: "analyst"}, Visibility: models.ActivityVisibilityTeam,
	})
	if err != nil || first.Evaluation.Wake || first.Source.Status != models.AgentRunStatusWaitingForDependency || len(first.Events) != 1 {
		t.Fatalf("first completion = %#v, err = %v", first, err)
	}
	coordinatorClock = func() time.Time { return time.Date(2026, 7, 10, 12, 3, 0, 0, time.UTC) }
	secondRequest := portfolio.ResolveRunDependencyRequest{
		Scope: scope, GroupID: "join-all", DependencyID: "research", ExpectedDependencyRevision: 1,
		State: models.RunDependencyStateSatisfied, Result: map[string]interface{}{"finding": "users need durable retries"},
		Actor: models.ActivityActor{Type: "agent", ID: "researcher"}, Visibility: models.ActivityVisibilityTeam,
	}
	second, err := coordinator.ResolveRunDependency(ctx, secondRequest)
	if err != nil || !second.Evaluation.Wake || second.Group.Status != models.RunDependencyGroupSatisfied ||
		second.Source.Status != models.AgentRunStatusQueued || second.Source.WakeCondition != nil || len(second.Events) != 2 {
		t.Fatalf("second completion = %#v, err = %v", second, err)
	}
	replay, err := coordinator.ResolveRunDependency(ctx, secondRequest)
	if err != nil || !replay.Replayed || replay.Source.Revision != second.Source.Revision || len(replay.Events) != 0 {
		t.Fatalf("completion replay = %#v, err = %v", replay, err)
	}
	activity, err := store.ListActivity(ctx, models.ActivityFilter{Scope: scope, RunID: "source-all", Limit: 20})
	if err != nil || len(activity) != 4 {
		t.Fatalf("activity count = %d, err = %v", len(activity), err)
	}
	for index, event := range activity {
		if event.Sequence != int64(index+1) {
			t.Fatalf("activity sequence %d = %d", index, event.Sequence)
		}
	}
}

func TestDependencyCoordinatorConcurrentAnyFanInNeverDoubleWakes(t *testing.T) {
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "concurrent"}
	store := kerneltest.NewStore(t)
	createDependencySource(t, store, scope, "source-any")
	coordinator := portfolio.NewDependencyCoordinator(store, models.WithClock(func() time.Time { return time.Date(2026, 7, 10, 13, 0, 0, 0, time.UTC) }))
	_, err := coordinator.CreateRunDependencyGroup(ctx, models.CreateRunDependencyGroupRequest{
		ID: "join-any", Scope: scope, SourceRunID: "source-any", ExpectedSourceRevision: 1,
		Policy: models.RunDependencyPolicy{Mode: models.FanInModeAny, FailureMode: models.DependencyFailureFailFast},
		Dependencies: []models.RunDependencySpec{
			{ID: "one", Kind: models.RunDependencyKindAgentRequest, RequestID: "request-one"},
			{ID: "two", Kind: models.RunDependencyKindAgentRequest, RequestID: "request-two"},
			{ID: "three", Kind: models.RunDependencyKindAgentRequest, RequestID: "request-three"},
		},
		Actor: models.ActivityActor{Type: "agent", ID: "lead"}, Visibility: models.ActivityVisibilityTeam,
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
			result, resolveErr := coordinator.ResolveRunDependency(ctx, portfolio.ResolveRunDependencyRequest{
				Scope: scope, GroupID: "join-any", DependencyID: id, ExpectedDependencyRevision: 1,
				State: models.RunDependencyStateSatisfied, Result: map[string]interface{}{"winner": id},
				Actor: models.ActivityActor{Type: "agent", ID: id}, Visibility: models.ActivityVisibilityTeam,
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
	if err != nil || group.Status != models.RunDependencyGroupSatisfied || group.Revision != 4 || group.WakeSignalID == "" {
		t.Fatalf("group = %#v, err = %v", group, err)
	}
	source, err := store.GetAgentRun(ctx, scope, "source-any")
	if err != nil || source.Status != models.AgentRunStatusQueued || source.Revision != 5 || source.LastWakeSignalID != group.WakeSignalID {
		t.Fatalf("source = %#v, err = %v", source, err)
	}
	activity, err := store.ListActivity(ctx, models.ActivityFilter{Scope: scope, RunID: source.ID, Limit: 20})
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

func TestDependencyCoordinatorOptionalCompletionContinuesAfterWake(t *testing.T) {
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "optional"}
	store := kerneltest.NewStore(t)
	createDependencySource(t, store, scope, "source-optional")
	coordinator := portfolio.NewDependencyCoordinator(store)
	optional := false
	_, err := coordinator.CreateRunDependencyGroup(ctx, models.CreateRunDependencyGroupRequest{
		ID: "join-optional", Scope: scope, SourceRunID: "source-optional", ExpectedSourceRevision: 1,
		Policy: models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureFailFast},
		Dependencies: []models.RunDependencySpec{
			{ID: "required", Kind: models.RunDependencyKindRun, TargetRunID: "child-required"},
			{ID: "optional", Kind: models.RunDependencyKindRun, TargetRunID: "child-optional", Required: &optional},
		},
		Actor: models.ActivityActor{Type: "agent", ID: "lead"}, Visibility: models.ActivityVisibilityTeam,
	})
	if err != nil {
		t.Fatal(err)
	}
	required, err := coordinator.ResolveRunDependency(ctx, portfolio.ResolveRunDependencyRequest{
		Scope: scope, GroupID: "join-optional", DependencyID: "required", ExpectedDependencyRevision: 1,
		State: models.RunDependencyStateSatisfied, Actor: models.ActivityActor{Type: "agent", ID: "required"}, Visibility: models.ActivityVisibilityTeam,
	})
	if err != nil || !required.Evaluation.Wake {
		t.Fatalf("required completion = %#v, err = %v", required, err)
	}
	optionalResult, err := coordinator.ResolveRunDependency(ctx, portfolio.ResolveRunDependencyRequest{
		Scope: scope, GroupID: "join-optional", DependencyID: "optional", ExpectedDependencyRevision: 1,
		State: models.RunDependencyStateSatisfied, Actor: models.ActivityActor{Type: "agent", ID: "optional"}, Visibility: models.ActivityVisibilityTeam,
	})
	if err != nil || optionalResult.Evaluation.Wake || optionalResult.Source.Status != models.AgentRunStatusQueued || optionalResult.Group.WakeSignalID != required.Group.WakeSignalID {
		t.Fatalf("optional completion = %#v, err = %v", optionalResult, err)
	}
}
