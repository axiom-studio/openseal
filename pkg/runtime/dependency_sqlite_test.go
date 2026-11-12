package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func createSQLiteDependencySource(t *testing.T, store *sqlite.Store, scope models.Scope, id string) {
	t.Helper()
	now := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	err := store.CreateAgentRun(context.Background(), &models.AgentRun{
		ID: id, Scope: scope, RootRunID: id, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "platform"},
		AssignedAgentID: "lead", Goal: "Coordinate durable fan-in", Source: models.RunSourceManual,
		Status: models.AgentRunStatusQueued, Priority: 90, AvailableAt: now, QueueEnteredAt: now,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteDependencyFanInSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "dependencies.db")
	scope := models.Scope{Kind: "tenant", ID: "restart"}
	store, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	createSQLiteDependencySource(t, store, scope, "source-restart")
	coordinator := portfolio.NewDependencyCoordinator(store, models.WithClock(func() time.Time { return time.Date(2026, 7, 10, 14, 1, 0, 0, time.UTC) }))
	create := models.CreateRunDependencyGroupRequest{
		ID: "join-restart", Scope: scope, SourceRunID: "source-restart", ExpectedSourceRevision: 1,
		Policy: models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureFailFast},
		Dependencies: []models.RunDependencySpec{
			{ID: "build", Kind: models.RunDependencyKindAgentRequest, RequestID: "request-build"},
			{ID: "review", Kind: models.RunDependencyKindAgentRequest, RequestID: "request-review"},
		},
		IdempotencyKey: "restart-once", Actor: models.ActivityActor{Type: "agent", ID: "lead"}, Visibility: models.ActivityVisibilityTeam,
	}
	created, err := coordinator.CreateRunDependencyGroup(ctx, create)
	if err != nil || created.Source.Status != models.AgentRunStatusWaitingForDependency {
		t.Fatalf("created = %#v, err = %v", created, err)
	}
	first, err := coordinator.ResolveRunDependency(ctx, portfolio.ResolveRunDependencyRequest{
		Scope: scope, GroupID: "join-restart", DependencyID: "review", ExpectedDependencyRevision: 1,
		State: models.RunDependencyStateSatisfied, Result: map[string]interface{}{"approved": true},
		Actor: models.ActivityActor{Type: "agent", ID: "reviewer"}, Visibility: models.ActivityVisibilityTeam,
	})
	if err != nil || first.Evaluation.Wake {
		t.Fatalf("first resolution = %#v, err = %v", first, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	coordinator = portfolio.NewDependencyCoordinator(restarted, models.WithClock(func() time.Time { return time.Date(2026, 7, 10, 14, 2, 0, 0, time.UTC) }))
	group, err := coordinator.GetRunDependencyGroup(ctx, scope, "join-restart")
	if err != nil || group.Status != models.RunDependencyGroupWaiting || group.Revision != 2 {
		t.Fatalf("restored group = %#v, err = %v", group, err)
	}
	edges, err := coordinator.ListRunDependencies(ctx, scope, group.ID)
	if err != nil || len(edges) != 2 || edges[1].ID != "review" || edges[1].State != models.RunDependencyStateSatisfied {
		t.Fatalf("restored edges = %#v, err = %v", edges, err)
	}
	secondRequest := portfolio.ResolveRunDependencyRequest{
		Scope: scope, GroupID: "join-restart", DependencyID: "build", ExpectedDependencyRevision: 1,
		State: models.RunDependencyStateSatisfied, Result: map[string]interface{}{"commit": "abc123"},
		Actor: models.ActivityActor{Type: "agent", ID: "builder"}, Visibility: models.ActivityVisibilityTeam,
	}
	second, err := coordinator.ResolveRunDependency(ctx, secondRequest)
	if err != nil || !second.Evaluation.Wake || second.Source.Status != models.AgentRunStatusQueued || second.Group.Status != models.RunDependencyGroupSatisfied {
		t.Fatalf("second resolution = %#v, err = %v", second, err)
	}
	replay, err := coordinator.ResolveRunDependency(ctx, secondRequest)
	if err != nil || !replay.Replayed || len(replay.Events) != 0 {
		t.Fatalf("resolution replay = %#v, err = %v", replay, err)
	}
	createReplay, err := coordinator.CreateRunDependencyGroup(ctx, create)
	if err != nil || !createReplay.Replayed || createReplay.Group.Status != models.RunDependencyGroupSatisfied {
		t.Fatalf("create replay = %#v, err = %v", createReplay, err)
	}
	if _, err := coordinator.GetRunDependencyGroup(ctx, models.Scope{Kind: "tenant", ID: "other"}, group.ID); !errors.Is(err, models.ErrDependencyGroupNotFound) {
		t.Fatalf("cross-scope lookup error = %v", err)
	}
	if err := restarted.Close(); err != nil {
		t.Fatal(err)
	}

	verified, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer verified.Close()
	persistedSource, err := verified.GetAgentRun(ctx, scope, "source-restart")
	if err != nil || persistedSource.Status != models.AgentRunStatusQueued || persistedSource.WakeCondition != nil || persistedSource.Revision != 4 {
		t.Fatalf("persisted source = %#v, err = %v", persistedSource, err)
	}
	activity, err := verified.ListActivity(ctx, models.ActivityFilter{Scope: scope, RunID: persistedSource.ID, Limit: 20})
	if err != nil || len(activity) != 4 {
		t.Fatalf("persisted activity count = %d, err = %v", len(activity), err)
	}
}

func TestSQLiteDependencyConcurrentStoresEmitOneWake(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "concurrent-dependencies.db")
	scope := models.Scope{Kind: "tenant", ID: "sqlite-concurrent"}
	primary, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	replica, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	createSQLiteDependencySource(t, primary, scope, "source-concurrent")
	coordinator := portfolio.NewDependencyCoordinator(primary)
	_, err = coordinator.CreateRunDependencyGroup(ctx, models.CreateRunDependencyGroupRequest{
		ID: "join-concurrent", Scope: scope, SourceRunID: "source-concurrent", ExpectedSourceRevision: 1,
		Policy: models.RunDependencyPolicy{Mode: models.FanInModeAny, FailureMode: models.DependencyFailureFailFast},
		Dependencies: []models.RunDependencySpec{
			{ID: "first", Kind: models.RunDependencyKindAgentRequest, RequestID: "request-first"},
			{ID: "second", Kind: models.RunDependencyKindAgentRequest, RequestID: "request-second"},
		},
		Actor: models.ActivityActor{Type: "agent", ID: "lead"}, Visibility: models.ActivityVisibilityTeam,
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinators := []*portfolio.DependencyCoordinator{portfolio.NewDependencyCoordinator(primary), portfolio.NewDependencyCoordinator(replica)}
	ids := []string{"first", "second"}
	var wakes atomic.Int32
	var failures atomic.Int32
	var wg sync.WaitGroup
	for index := range coordinators {
		index := index
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, resolveErr := coordinators[index].ResolveRunDependency(ctx, portfolio.ResolveRunDependencyRequest{
				Scope: scope, GroupID: "join-concurrent", DependencyID: ids[index], ExpectedDependencyRevision: 1,
				State: models.RunDependencyStateSatisfied, Result: map[string]interface{}{"source": ids[index]},
				Actor: models.ActivityActor{Type: "agent", ID: ids[index]}, Visibility: models.ActivityVisibilityTeam,
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
	if err != nil || group.Status != models.RunDependencyGroupSatisfied || group.Revision != 3 {
		t.Fatalf("group = %#v, err = %v", group, err)
	}
	activity, err := primary.ListActivity(ctx, models.ActivityFilter{Scope: scope, RunID: "source-concurrent", Limit: 20})
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
