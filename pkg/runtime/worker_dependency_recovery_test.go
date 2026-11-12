package runtime

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun/agentruntest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/google/uuid"
)

func TestAgentRunWorkerDependencyRepairPreservesTypedJoinAny(t *testing.T) {
	forDependencyRetirementStores(t, func(t *testing.T, store agentruntest.DependencyStore) {
		fixture := agentruntest.NewDependencyFixtureWithPolicy(t, store, models.Scope{Kind: "tenant", ID: uuid.NewString()}, models.RunDependencyPolicy{Mode: models.FanInModeAny, FailureMode: models.DependencyFailureWait})
		finishOwnedDependencyRepairChild(t, fixture, "alpha")
		pool := newDependencyRepairTestPool(t, store, fixture.Scope)
		pool.reconcileForkChildren(t.Context())
		after := fixture.Snapshot(t)
		if after.Group.Status != models.RunDependencyGroupSatisfied || after.Source.Status != models.AgentRunStatusQueued || after.Children["beta"].Status != models.AgentRunStatusCanceled {
			t.Fatalf("indexed repair lost typed join_any policy: group=%s source=%s beta=%s", after.Group.Status, after.Source.Status, after.Children["beta"].Status)
		}
		fixture.AssertWaitingWork(t, 0)
	})
}

func TestAgentRunWorkerFinalizerRecoversWinnerCommitBeforeLoserCancellation(t *testing.T) {
	forDependencyRetirementStores(t, func(t *testing.T, store agentruntest.DependencyStore) {
		fixture := agentruntest.NewDependencyFixtureWithPolicy(t, store, models.Scope{Kind: "tenant", ID: uuid.NewString()}, models.RunDependencyPolicy{Mode: models.FanInModeAny, FailureMode: models.DependencyFailureWait})
		winner := finishOwnedDependencyRepairChild(t, fixture, "alpha")
		// Persist only fan-in to represent the durable state after a process
		// exits between winner publication and owned-loser cancellation.
		published, err := fixture.Coordinator().ReconcileRunDependencyGroup(t.Context(), fixture.Request(t))
		if err != nil || published.Group.Status != models.RunDependencyGroupSatisfied || published.Source.Status != models.AgentRunStatusQueued {
			t.Fatalf("winner publication = %#v, %v", published, err)
		}
		before := fixture.Snapshot(t)
		if before.Children["beta"].Status == models.AgentRunStatusCanceled {
			t.Fatal("fixture did not preserve the crash window")
		}
		fixture.AssertWaitingWork(t, 0)
		restarted := newDependencyRepairTestPool(t, store, fixture.Scope)
		restarted.reconcileTerminalRunFinalizers(t.Context())
		after := fixture.Snapshot(t)
		if after.Children["beta"].Status != models.AgentRunStatusCanceled || after.Source.Status != before.Source.Status ||
			after.Source.LastWakeSignalID != before.Source.LastWakeSignalID || after.Source.AvailableAt != before.Source.AvailableAt ||
			after.Source.QueueEnteredAt != before.Source.QueueEnteredAt || !reflect.DeepEqual(before.Source.Checkpoint, after.Source.Checkpoint) {
			t.Fatalf("bounded terminal repair did not recover owned cleanup without rewaking the source: beta=%s source=%s", after.Children["beta"].Status, after.Source.Status)
		}
		if _, err := agentrun.NewRunForkCoordinator(store).CompleteChild(t.Context(), winner, models.ActivityActor{Type: "worker", ID: "restarted-worker"}); err != nil {
			t.Fatal(err)
		}
		fixture.AssertSnapshot(t, after)
	})
}

func TestAgentRunWorkerTerminalSourceFinalizerRecoversRemainingOwnedChild(t *testing.T) {
	forDependencyRetirementStores(t, func(t *testing.T, store agentruntest.DependencyStore) {
		fixture := agentruntest.NewDependencyFixtureWithPolicy(t, store, models.Scope{Kind: "tenant", ID: uuid.NewString()}, models.RunDependencyPolicy{Mode: models.FanInModeAny, FailureMode: models.DependencyFailureWait})
		winner := finishOwnedDependencyRepairChild(t, fixture, "alpha")
		if _, err := fixture.Coordinator().ReconcileRunDependencyGroup(t.Context(), fixture.Request(t)); err != nil {
			t.Fatal(err)
		}
		fixture.TerminateSource(t, models.AgentRunStatusCompleted)
		before := fixture.Snapshot(t)
		if before.Children["beta"].Status != models.AgentRunStatusRunning {
			t.Fatal("fixture did not preserve the unresolved cleanup window")
		}
		restarted := newDependencyRepairTestPool(t, store, fixture.Scope)
		restarted.reconcileTerminalRunFinalizers(t.Context())
		after := fixture.Snapshot(t)
		if after.Children["beta"].Status != models.AgentRunStatusCanceled || !reflect.DeepEqual(before.Source, after.Source) || !reflect.DeepEqual(winner, after.Children["alpha"]) {
			t.Fatalf("terminal source repair did not cascade owned cleanup: beta=%s", after.Children["beta"].Status)
		}
	})
}

func finishOwnedDependencyRepairChild(t *testing.T, fixture *agentruntest.DependencyFixture, branch string) *models.AgentRun {
	t.Helper()
	child, err := fixture.Store.GetAgentRun(t.Context(), fixture.Scope, fixture.ChildIDs[branch])
	if err != nil || child == nil {
		t.Fatalf("get child = %#v, %v", child, err)
	}
	checkpoint := maps.Clone(child.Checkpoint)
	checkpoint["branch"] = branch
	checkpoint["committed"] = "terminal"
	service := activity.NewRunActivityService(fixture.Store, fixture.Store, models.WithClock(func() time.Time { return fixture.Now.Add(4 * time.Second) }))
	if _, _, err := service.TransitionRun(t.Context(), fixture.Scope, child.ID, activity.RunTransitionRequest{
		ExpectedRevision: child.Revision, Status: models.AgentRunStatusCompleted,
		Actor: models.ActivityActor{Type: "worker", ID: "child-worker"}, LeaseOwner: "child-worker",
		Checkpoint: checkpoint, Output: map[string]interface{}{"message": "actual " + branch + " result"},
		BudgetUsageDelta: &models.BudgetUsage{Turns: 1},
	}); err != nil {
		t.Fatal(err)
	}
	child, err = fixture.Store.GetAgentRun(t.Context(), fixture.Scope, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	return child
}

func TestAgentRunWorkerDependencyRetirementLeavesIndependentTargetsUntouched(t *testing.T) {
	forDependencyRetirementStores(t, func(t *testing.T, store agentruntest.DependencyStore) {
		fixture := agentruntest.NewDependencyFixtureWithPolicy(t, store, models.Scope{Kind: "tenant", ID: uuid.NewString()}, models.RunDependencyPolicy{Mode: models.FanInModeAny, FailureMode: models.DependencyFailureWait})
		fixture.FinishChild(t, "alpha", models.AgentRunStatusCompleted)
		fixture.TerminateSource(t, models.AgentRunStatusCanceled)
		before := fixture.Snapshot(t)
		pool := newDependencyRepairTestPool(t, store, fixture.Scope)
		pool.reconcileForkChildren(t.Context())
		after := fixture.Snapshot(t)
		if after.Group.Status != models.RunDependencyGroupCanceled || !reflect.DeepEqual(before.Source, after.Source) || !reflect.DeepEqual(before.Children, after.Children) {
			t.Fatalf("retirement changed independently progressing targets: %#v", after)
		}
	})
}

type dependencyRepairReadCountingStore struct {
	*sqlite.Store
	runReads atomic.Int64
	runLists atomic.Int64
}

func (s *dependencyRepairReadCountingStore) GetAgentRun(ctx context.Context, scope models.Scope, id string) (*models.AgentRun, error) {
	s.runReads.Add(1)
	return s.Store.GetAgentRun(ctx, scope, id)
}

func (s *dependencyRepairReadCountingStore) ListAgentRuns(ctx context.Context, filter models.AgentRunFilter) ([]*models.AgentRun, error) {
	s.runLists.Add(1)
	return s.Store.ListAgentRuns(ctx, filter)
}

func TestAgentRunWorkerIdleDependencyMetadataDoesNotHydrateRunsOrHistory(t *testing.T) {
	store := &dependencyRepairReadCountingStore{Store: newTestKernelStore(t)}
	fixture := agentruntest.NewDependencyFixtureWithPolicy(t, store.Store, models.Scope{Kind: "tenant", ID: uuid.NewString()}, models.RunDependencyPolicy{Mode: models.FanInModeAny, FailureMode: models.DependencyFailureWait})
	pool := newDependencyRepairTestPool(t, store, fixture.Scope)
	for range 5 {
		pool.reconcileForkChildren(t.Context())
	}
	if store.runReads.Load() != 0 || store.runLists.Load() != 0 {
		t.Fatalf("unchanged waiting metadata hydrated Runs/history: reads=%d lists=%d", store.runReads.Load(), store.runLists.Load())
	}
	fixture.AssertWaitingWork(t, 1)
}

func newDependencyRepairTestPool(t *testing.T, store models.KernelStore, scope models.Scope) *AgentRunWorkerPool {
	t.Helper()
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *models.AgentRun) (*agentrun.TurnRunnerBinding, error) {
		return nil, errors.New("reconciliation must not resolve provider authority")
	}), nil, AgentRunWorkerConfig{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	return pool
}
