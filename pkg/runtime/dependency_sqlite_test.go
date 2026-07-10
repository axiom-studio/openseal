package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func createSQLiteDependencySource(t *testing.T, store *SQLiteStore, scope Scope, id string) {
	t.Helper()
	now := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	err := store.CreateAgentRun(context.Background(), &AgentRun{
		ID: id, Scope: scope, RootRunID: id, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "platform"},
		AssignedAgentID: "lead", Goal: "Coordinate durable fan-in", Source: RunSourceManual,
		Status: AgentRunStatusQueued, Priority: 90, AvailableAt: now, QueueEnteredAt: now,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteDependencyFanInSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dependencies.db")
	scope := Scope{Kind: "tenant", ID: "restart"}
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	createSQLiteDependencySource(t, store, scope, "source-restart")
	coordinator := NewDependencyCoordinator(store)
	coordinator.now = func() time.Time { return time.Date(2026, 7, 10, 14, 1, 0, 0, time.UTC) }
	create := CreateRunDependencyGroupRequest{
		ID: "join-restart", Scope: scope, SourceRunID: "source-restart", ExpectedSourceRevision: 1,
		Policy: RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
		Dependencies: []RunDependencySpec{
			{ID: "build", Kind: RunDependencyKindAgentRequest, RequestID: "request-build"},
			{ID: "review", Kind: RunDependencyKindAgentRequest, RequestID: "request-review"},
		},
		IdempotencyKey: "restart-once", Actor: ActivityActor{Type: "agent", ID: "lead"}, Visibility: ActivityVisibilityTeam,
	}
	created, err := coordinator.CreateRunDependencyGroup(ctx, create)
	if err != nil || created.Source.Status != AgentRunStatusWaitingForDependency {
		t.Fatalf("created = %#v, err = %v", created, err)
	}
	first, err := coordinator.ResolveRunDependency(ctx, ResolveRunDependencyRequest{
		Scope: scope, GroupID: "join-restart", DependencyID: "review", ExpectedDependencyRevision: 1,
		State: RunDependencyStateSatisfied, Result: map[string]interface{}{"approved": true},
		Actor: ActivityActor{Type: "agent", ID: "reviewer"}, Visibility: ActivityVisibilityTeam,
	})
	if err != nil || first.Evaluation.Wake {
		t.Fatalf("first resolution = %#v, err = %v", first, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	coordinator = NewDependencyCoordinator(restarted)
	coordinator.now = func() time.Time { return time.Date(2026, 7, 10, 14, 2, 0, 0, time.UTC) }
	group, err := coordinator.GetRunDependencyGroup(ctx, scope, "join-restart")
	if err != nil || group.Status != RunDependencyGroupWaiting || group.Revision != 2 {
		t.Fatalf("restored group = %#v, err = %v", group, err)
	}
	edges, err := coordinator.ListRunDependencies(ctx, scope, group.ID)
	if err != nil || len(edges) != 2 || edges[1].ID != "review" || edges[1].State != RunDependencyStateSatisfied {
		t.Fatalf("restored edges = %#v, err = %v", edges, err)
	}
	secondRequest := ResolveRunDependencyRequest{
		Scope: scope, GroupID: "join-restart", DependencyID: "build", ExpectedDependencyRevision: 1,
		State: RunDependencyStateSatisfied, Result: map[string]interface{}{"commit": "abc123"},
		Actor: ActivityActor{Type: "agent", ID: "builder"}, Visibility: ActivityVisibilityTeam,
	}
	second, err := coordinator.ResolveRunDependency(ctx, secondRequest)
	if err != nil || !second.Evaluation.Wake || second.Source.Status != AgentRunStatusQueued || second.Group.Status != RunDependencyGroupSatisfied {
		t.Fatalf("second resolution = %#v, err = %v", second, err)
	}
	replay, err := coordinator.ResolveRunDependency(ctx, secondRequest)
	if err != nil || !replay.Replayed || len(replay.Events) != 0 {
		t.Fatalf("resolution replay = %#v, err = %v", replay, err)
	}
	createReplay, err := coordinator.CreateRunDependencyGroup(ctx, create)
	if err != nil || !createReplay.Replayed || createReplay.Group.Status != RunDependencyGroupSatisfied {
		t.Fatalf("create replay = %#v, err = %v", createReplay, err)
	}
	if _, err := coordinator.GetRunDependencyGroup(ctx, Scope{Kind: "tenant", ID: "other"}, group.ID); !errors.Is(err, ErrDependencyGroupNotFound) {
		t.Fatalf("cross-scope lookup error = %v", err)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}

	verified, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	persistedSource, err := verified.GetAgentRun(ctx, scope, "source-restart")
	if err != nil || persistedSource.Status != AgentRunStatusQueued || persistedSource.WakeCondition != nil || persistedSource.Revision != 4 {
		t.Fatalf("persisted source = %#v, err = %v", persistedSource, err)
	}
	activity, err := verified.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: persistedSource.ID, Limit: 20})
	if err != nil || len(activity) != 4 {
		t.Fatalf("persisted activity count = %d, err = %v", len(activity), err)
	}
}

func TestSQLiteDependencyConcurrentStoresEmitOneWake(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "concurrent-dependencies.db")
	scope := Scope{Kind: "tenant", ID: "sqlite-concurrent"}
	primary, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	replica, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	createSQLiteDependencySource(t, primary, scope, "source-concurrent")
	coordinator := NewDependencyCoordinator(primary)
	_, err = coordinator.CreateRunDependencyGroup(ctx, CreateRunDependencyGroupRequest{
		ID: "join-concurrent", Scope: scope, SourceRunID: "source-concurrent", ExpectedSourceRevision: 1,
		Policy: RunDependencyPolicy{Mode: FanInModeAny, FailureMode: DependencyFailureFailFast},
		Dependencies: []RunDependencySpec{
			{ID: "first", Kind: RunDependencyKindAgentRequest, RequestID: "request-first"},
			{ID: "second", Kind: RunDependencyKindAgentRequest, RequestID: "request-second"},
		},
		Actor: ActivityActor{Type: "agent", ID: "lead"}, Visibility: ActivityVisibilityTeam,
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinators := []*DependencyCoordinator{NewDependencyCoordinator(primary), NewDependencyCoordinator(replica)}
	ids := []string{"first", "second"}
	var wakes atomic.Int32
	var failures atomic.Int32
	var wg sync.WaitGroup
	for index := range coordinators {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, resolveErr := coordinators[index].ResolveRunDependency(ctx, ResolveRunDependencyRequest{
				Scope: scope, GroupID: "join-concurrent", DependencyID: ids[index], ExpectedDependencyRevision: 1,
				State: RunDependencyStateSatisfied, Result: map[string]interface{}{"source": ids[index]},
				Actor: ActivityActor{Type: "agent", ID: ids[index]}, Visibility: ActivityVisibilityTeam,
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
	group, err := coordinator.GetRunDependencyGroup(ctx, scope, "join-concurrent")
	if err != nil || group.Status != RunDependencyGroupSatisfied || group.Revision != 3 {
		t.Fatalf("group = %#v, err = %v", group, err)
	}
	activity, err := primary.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: "source-concurrent", Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	groupWakes := 0
	for _, event := range activity {
		if event.EventType == "dependency.group_satisfied" {
			groupWakes++
		}
	}
	if groupWakes != 1 {
		t.Fatalf("group wake events = %d", groupWakes)
	}
}
