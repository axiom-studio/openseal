package portfolio_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	runtimeportfolio "github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

type atomicForkStore interface {
	models.KernelStore
	models.RunDependencyStore
}

func TestDependencyFanOutAtomicallyCreatesTargetRuns(t *testing.T) {
	t.Parallel()
	stores := []struct {
		name string
		open func(*testing.T) (atomicForkStore, func())
	}{
		{name: "sqlite", open: func(t *testing.T) (atomicForkStore, func()) {
			store, err := sqlite.New(filepath.Join(t.TempDir(), "fork.db"))
			if err != nil {
				t.Fatal(err)
			}
			return store, func() { _ = store.Close() }
		}},
	}
	for _, tc := range stores {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			store, closeStore := tc.open(t)
			defer closeStore()
			record := atomicTargetRunFixture(t, store)
			result, err := store.CreateRunDependencyGroup(t.Context(), record)
			if err != nil || result.Replayed || len(result.Dependencies) != 2 {
				t.Fatalf("create=%#v error=%v", result, err)
			}
			for _, target := range record.TargetRuns {
				persisted, err := store.GetAgentRun(t.Context(), target.Scope, target.ID)
				if err != nil || persisted == nil || persisted.ParentRunID != record.SourceRun.ID || persisted.Status != models.AgentRunStatusQueued {
					t.Fatalf("target=%#v error=%v", persisted, err)
				}
			}
			replayed, err := store.CreateRunDependencyGroup(t.Context(), record)
			if err != nil || !replayed.Replayed {
				t.Fatalf("replay=%#v error=%v", replayed, err)
			}
		})
	}
}

func TestDependencyFanOutRejectsUnsealedTargetWithoutMutation(t *testing.T) {
	store := kerneltest.NewStore(t)
	record := atomicTargetRunFixture(t, store)
	record.Dependencies[0].TargetRunID = "missing-child"
	if _, err := store.CreateRunDependencyGroup(t.Context(), record); err == nil {
		t.Fatal("unmatched target run was accepted")
	}
	for _, target := range record.TargetRuns {
		if persisted, _ := store.GetAgentRun(t.Context(), target.Scope, target.ID); persisted != nil {
			t.Fatalf("target %s leaked from rejected transaction", target.ID)
		}
	}
}

func atomicTargetRunFixture(t *testing.T, store atomicForkStore) models.RunDependencyGroupCreateRecord {
	t.Helper()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "7"}
	portfolio := runtimeportfolio.NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "parent", Source: models.RunSourceManual})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	targets := make([]*models.AgentRun, 0, 2)
	for _, id := range []string{"child-a", "child-b"} {
		target, err := runtimeportfolio.BuildAgentRun(ctx, store, models.CreateAgentRunRequest{
			Scope: scope, ParentRunID: source.ID, Owner: source.Owner, AssignedAgentID: source.AssignedAgentID,
			Goal: "branch " + id, Source: models.RunSourceRequest, Checkpoint: map[string]interface{}{"branch": id},
		}, id, now)
		if err != nil {
			t.Fatal(err)
		}
		targets = append(targets, target)
	}
	sealedAt := now
	group := &models.RunDependencyGroup{
		ID: "fork", Scope: scope, SourceRunID: source.ID,
		Policy:        models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureFailFast},
		ExpectedCount: 2, Status: models.RunDependencyGroupWaiting, Revision: 1, CreatedAt: now, UpdatedAt: now, SealedAt: &sealedAt,
		IdempotencyKey: "fork-once",
	}
	edges := []*models.RunDependency{
		{ID: "a", Scope: scope, GroupID: group.ID, SourceRunID: source.ID, TargetRunID: targets[0].ID, Kind: models.RunDependencyKindRun, State: models.RunDependencyStatePending, Required: true, Revision: 1, CreatedAt: now, UpdatedAt: now},
		{ID: "b", Scope: scope, GroupID: group.ID, SourceRunID: source.ID, TargetRunID: targets[1].ID, Kind: models.RunDependencyKindRun, State: models.RunDependencyStatePending, Required: true, Revision: 1, CreatedAt: now, UpdatedAt: now},
	}
	updatedSource := models.CloneAgentRun(source)
	updatedSource.Status = models.AgentRunStatusWaitingForDependency
	updatedSource.WakeCondition = &models.WakeCondition{Type: "run_dependencies", Reference: group.ID}
	updatedSource.Revision++
	updatedSource.UpdatedAt = now
	event := &models.ActivityEvent{ID: "fork-event", Scope: scope, RunID: source.ID, EventType: "dependency.group_created", Summary: "Forked two branches", Actor: models.ActivityActor{Type: "worker", ID: "worker"}, Visibility: models.ActivityVisibilityScope, CorrelationID: group.ID, CreatedAt: now}
	return models.RunDependencyGroupCreateRecord{Group: group, Dependencies: edges, TargetRuns: targets, SourceRun: updatedSource, ExpectedSourceRevision: source.Revision, Event: event}
}
