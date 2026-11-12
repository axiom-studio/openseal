//go:build integration

package portfolio_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	runtimeportfolio "github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
)

func TestPostgresDependencyFanInIsConcurrentRecoverableAndIsolated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	primary, pgSchema := kerneltest.OpenPostgres(t, "openseal_dependencies_")
	schema := pgSchema.Name
	dsn := pgSchema.DSN
	replica, err := postgres.New(ctx, dsn, postgres.WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	if version, err := primary.PostgresSchemaVersion(ctx); err != nil || version != kerneltest.PostgresSchemaHead(t) {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}

	scope := models.Scope{Kind: "tenant", ID: "fan-in"}
	portfolio := runtimeportfolio.NewService(primary)
	source, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "research"}, AssignedAgentID: "lead",
		Goal: "Coordinate market research", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	create := models.CreateRunDependencyGroupRequest{
		Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision,
		Policy: models.RunDependencyPolicy{Mode: models.FanInModeQuorum, Quorum: 2, FailureMode: models.DependencyFailureFailFast},
		Dependencies: []models.RunDependencySpec{
			{ID: "reddit", Kind: models.RunDependencyKindAgentRequest, RequestID: "request-reddit"},
			{ID: "forums", Kind: models.RunDependencyKindAgentRequest, RequestID: "request-forums"},
			{ID: "reviews", Kind: models.RunDependencyKindAgentRequest, RequestID: "request-reviews"},
		},
		IdempotencyKey: "market-research-fan-in", Actor: models.ActivityActor{Type: "agent", ID: "lead"}, Visibility: models.ActivityVisibilityTeam,
	}
	stores := []*postgres.Store{primary, replica}
	created := make(chan *models.RunDependencyResult, 2)
	createErrors := make(chan error, 2)
	var createWait sync.WaitGroup
	for _, store := range stores {
		store := store
		createWait.Add(1)
		go func() {
			defer createWait.Done()
			result, createErr := runtimeportfolio.NewDependencyCoordinator(store).CreateRunDependencyGroup(ctx, create)
			if createErr != nil {
				createErrors <- createErr
				return
			}
			created <- result
		}()
	}
	createWait.Wait()
	close(created)
	close(createErrors)
	for createErr := range createErrors {
		t.Fatal(createErr)
	}
	var groupID string
	createCount := 0
	replayCount := 0
	for result := range created {
		createCount++
		if groupID == "" {
			groupID = result.Group.ID
		} else if result.Group.ID != groupID {
			t.Fatalf("concurrent create returned groups %s and %s", groupID, result.Group.ID)
		}
		if result.Replayed {
			replayCount++
		}
	}
	if createCount != 2 || replayCount != 1 {
		t.Fatalf("create count = %d, replay count = %d", createCount, replayCount)
	}

	var wakes atomic.Int32
	var failures atomic.Int32
	var resolveWait sync.WaitGroup
	for index, dependencyID := range []string{"reviews", "reddit", "forums"} {
		index, dependencyID := index, dependencyID
		resolveWait.Add(1)
		go func() {
			defer resolveWait.Done()
			result, resolveErr := runtimeportfolio.NewDependencyCoordinator(stores[index%len(stores)]).ResolveRunDependency(ctx, runtimeportfolio.ResolveRunDependencyRequest{
				Scope: scope, GroupID: groupID, DependencyID: dependencyID, ExpectedDependencyRevision: 1,
				State: models.RunDependencyStateSatisfied, Result: map[string]interface{}{"source": dependencyID},
				Actor: models.ActivityActor{Type: "agent", ID: dependencyID}, Visibility: models.ActivityVisibilityTeam,
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
	resolveWait.Wait()
	if failures.Load() != 0 || wakes.Load() != 1 {
		t.Fatalf("resolution failures = %d, wakes = %d", failures.Load(), wakes.Load())
	}

	restarted, err := postgres.New(ctx, dsn, postgres.WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	coordinator := runtimeportfolio.NewDependencyCoordinator(restarted)
	group, err := coordinator.GetRunDependencyGroup(ctx, scope, groupID)
	if err != nil || group.Status != models.RunDependencyGroupSatisfied || group.Revision != 4 || group.WakeSignalID == "" {
		t.Fatalf("restored group = %#v, err = %v", group, err)
	}
	edges, err := coordinator.ListRunDependencies(ctx, scope, groupID)
	if err != nil || len(edges) != 3 {
		t.Fatalf("restored edges = %#v, err = %v", edges, err)
	}
	for _, edge := range edges {
		if edge.State != models.RunDependencyStateSatisfied || edge.Revision != 2 {
			t.Fatalf("restored edge = %#v", edge)
		}
	}
	persistedSource, err := restarted.GetAgentRun(ctx, scope, source.ID)
	if err != nil || persistedSource.Status != models.AgentRunStatusQueued || persistedSource.WakeCondition != nil ||
		persistedSource.LastWakeSignalID != group.WakeSignalID || persistedSource.Revision != 5 {
		t.Fatalf("restored source = %#v, err = %v", persistedSource, err)
	}
	activity, err := restarted.ListActivity(ctx, models.ActivityFilter{Scope: scope, RunID: source.ID, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	groupWakeEvents := 0
	for _, event := range activity {
		if event.EventType == "dependency.group_satisfied" {
			groupWakeEvents++
		}
	}
	if groupWakeEvents != 1 || len(activity) != 5 {
		t.Fatalf("activity count = %d, group wakes = %d", len(activity), groupWakeEvents)
	}
	if foreign, err := coordinator.GetRunDependencyGroup(ctx, models.Scope{Kind: "tenant", ID: "other"}, groupID); err == nil || foreign != nil {
		t.Fatalf("cross-scope group = %#v, err = %v", foreign, err)
	}
	createReplay, err := coordinator.CreateRunDependencyGroup(ctx, create)
	if err != nil || !createReplay.Replayed || createReplay.Group.ID != groupID {
		t.Fatalf("restart create replay = %#v, err = %v", createReplay, err)
	}
}

func TestPostgresDependencyFanInPreservesIndependentMultiObjectiveProgress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	primary, pgSchema := kerneltest.OpenPostgres(t, "openseal_dependency_load_")
	schema := pgSchema.Name
	dsn := pgSchema.DSN
	replica, err := postgres.New(ctx, dsn, postgres.WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	stores := []*postgres.Store{primary, replica}
	scope := models.Scope{Kind: "tenant", ID: "portfolio-load"}
	const objectiveCount = 8
	groupIDs := make([]string, objectiveCount)
	for index := 0; index < objectiveCount; index++ {
		source, err := runtimeportfolio.NewService(primary).CreateAgentRun(ctx, models.CreateAgentRunRequest{
			Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: fmt.Sprintf("agent-%d", index)},
			AssignedAgentID: fmt.Sprintf("agent-%d", index), Goal: fmt.Sprintf("Objective %d", index), Source: models.RunSourceObjective,
		})
		if err != nil {
			t.Fatal(err)
		}
		result, err := runtimeportfolio.NewDependencyCoordinator(primary).CreateRunDependencyGroup(ctx, models.CreateRunDependencyGroupRequest{
			ID: fmt.Sprintf("group-%d", index), Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision,
			Policy: models.RunDependencyPolicy{Mode: models.FanInModeQuorum, Quorum: 2, FailureMode: models.DependencyFailureFailFast},
			Dependencies: []models.RunDependencySpec{
				{ID: "a", Kind: models.RunDependencyKindAgentRequest, RequestID: fmt.Sprintf("request-%d-a", index)},
				{ID: "b", Kind: models.RunDependencyKindAgentRequest, RequestID: fmt.Sprintf("request-%d-b", index)},
				{ID: "c", Kind: models.RunDependencyKindAgentRequest, RequestID: fmt.Sprintf("request-%d-c", index)},
			},
			Actor: models.ActivityActor{Type: "agent", ID: fmt.Sprintf("agent-%d", index)}, Visibility: models.ActivityVisibilityPrivate,
		})
		if err != nil {
			t.Fatal(err)
		}
		groupIDs[index] = result.Group.ID
	}
	var wakes atomic.Int32
	var failures atomic.Int32
	var wg sync.WaitGroup
	for groupIndex, groupID := range groupIDs {
		for edgeIndex, dependencyID := range []string{"a", "b", "c"} {
			groupIndex, groupID, edgeIndex, dependencyID := groupIndex, groupID, edgeIndex, dependencyID
			wg.Add(1)
			go func() {
				defer wg.Done()
				result, err := runtimeportfolio.NewDependencyCoordinator(stores[(groupIndex+edgeIndex)%len(stores)]).ResolveRunDependency(ctx, runtimeportfolio.ResolveRunDependencyRequest{
					Scope: scope, GroupID: groupID, DependencyID: dependencyID, ExpectedDependencyRevision: 1,
					State: models.RunDependencyStateSatisfied, Actor: models.ActivityActor{Type: "agent", ID: dependencyID}, Visibility: models.ActivityVisibilityPrivate,
				})
				if err != nil {
					failures.Add(1)
					return
				}
				if result.Evaluation.Wake {
					wakes.Add(1)
				}
			}()
		}
	}
	wg.Wait()
	if failures.Load() != 0 || wakes.Load() != objectiveCount {
		t.Fatalf("failures = %d, wakes = %d", failures.Load(), wakes.Load())
	}
	for _, groupID := range groupIDs {
		group, err := runtimeportfolio.NewDependencyCoordinator(replica).GetRunDependencyGroup(ctx, scope, groupID)
		if err != nil || group.Status != models.RunDependencyGroupSatisfied || group.Revision != 4 {
			t.Fatalf("group %s = %#v, err = %v", groupID, group, err)
		}
	}
}
