package runtime

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestAgentRunWorkerDependencyRepairPreservesTypedJoinAny(t *testing.T) {
	forDependencyRetirementStores(t, func(t *testing.T, store dependencyRetirementStore) {
		fixture := newDependencyRetirementFixtureWithPolicy(t, store, Scope{Kind: "tenant", ID: uuid.NewString()}, RunDependencyPolicy{Mode: FanInModeAny, FailureMode: DependencyFailureWait})
		finishOwnedDependencyRepairChild(t, fixture, "alpha")
		pool := newDependencyRepairTestPool(t, store, fixture.scope)
		pool.reconcileForkChildren(t.Context())
		after := fixture.snapshot(t)
		if after.group.Status != RunDependencyGroupSatisfied || after.source.Status != AgentRunStatusQueued || after.children["beta"].Status != AgentRunStatusCanceled {
			t.Fatalf("indexed repair lost typed join_any policy: group=%s source=%s beta=%s", after.group.Status, after.source.Status, after.children["beta"].Status)
		}
		fixture.assertWaitingWork(t, 0)
	})
}

func TestAgentRunWorkerFinalizerRecoversWinnerCommitBeforeLoserCancellation(t *testing.T) {
	forDependencyRetirementStores(t, func(t *testing.T, store dependencyRetirementStore) {
		fixture := newDependencyRetirementFixtureWithPolicy(t, store, Scope{Kind: "tenant", ID: uuid.NewString()}, RunDependencyPolicy{Mode: FanInModeAny, FailureMode: DependencyFailureWait})
		winner := finishOwnedDependencyRepairChild(t, fixture, "alpha")
		// Persist only fan-in to represent the durable state after a process
		// exits between winner publication and owned-loser cancellation.
		published, err := fixture.coordinator().ReconcileRunDependencyGroup(t.Context(), fixture.request(t))
		if err != nil || published.Group.Status != RunDependencyGroupSatisfied || published.Source.Status != AgentRunStatusQueued {
			t.Fatalf("winner publication = %#v, %v", published, err)
		}
		before := fixture.snapshot(t)
		if before.children["beta"].Status == AgentRunStatusCanceled {
			t.Fatal("fixture did not preserve the crash window")
		}
		fixture.assertWaitingWork(t, 0)
		restarted := newDependencyRepairTestPool(t, store, fixture.scope)
		restarted.reconcileTerminalRunFinalizers(t.Context())
		after := fixture.snapshot(t)
		if after.children["beta"].Status != AgentRunStatusCanceled || after.source.Status != before.source.Status ||
			after.source.LastWakeSignalID != before.source.LastWakeSignalID || after.source.AvailableAt != before.source.AvailableAt ||
			after.source.QueueEnteredAt != before.source.QueueEnteredAt || !reflect.DeepEqual(before.source.Checkpoint, after.source.Checkpoint) {
			t.Fatalf("bounded terminal repair did not recover owned cleanup without rewaking the source: beta=%s source=%s", after.children["beta"].Status, after.source.Status)
		}
		if _, err := NewRunForkCoordinator(store).CompleteChild(t.Context(), winner, ActivityActor{Type: "worker", ID: "restarted-worker"}); err != nil {
			t.Fatal(err)
		}
		fixture.assertSnapshot(t, after)
	})
}

func TestAgentRunWorkerTerminalSourceFinalizerRecoversRemainingOwnedChild(t *testing.T) {
	forDependencyRetirementStores(t, func(t *testing.T, store dependencyRetirementStore) {
		fixture := newDependencyRetirementFixtureWithPolicy(t, store, Scope{Kind: "tenant", ID: uuid.NewString()}, RunDependencyPolicy{Mode: FanInModeAny, FailureMode: DependencyFailureWait})
		winner := finishOwnedDependencyRepairChild(t, fixture, "alpha")
		if _, err := fixture.coordinator().ReconcileRunDependencyGroup(t.Context(), fixture.request(t)); err != nil {
			t.Fatal(err)
		}
		fixture.terminateSource(t, AgentRunStatusCompleted)
		before := fixture.snapshot(t)
		if before.children["beta"].Status != AgentRunStatusRunning {
			t.Fatal("fixture did not preserve the unresolved cleanup window")
		}
		restarted := newDependencyRepairTestPool(t, store, fixture.scope)
		restarted.reconcileTerminalRunFinalizers(t.Context())
		after := fixture.snapshot(t)
		if after.children["beta"].Status != AgentRunStatusCanceled || !reflect.DeepEqual(before.source, after.source) || !reflect.DeepEqual(winner, after.children["alpha"]) {
			t.Fatalf("terminal source repair did not cascade owned cleanup: beta=%s", after.children["beta"].Status)
		}
	})
}

func finishOwnedDependencyRepairChild(t *testing.T, fixture *dependencyRetirementFixture, branch string) *AgentRun {
	t.Helper()
	child, err := fixture.store.GetAgentRun(t.Context(), fixture.scope, fixture.childIDs[branch])
	if err != nil || child == nil {
		t.Fatalf("get child = %#v, %v", child, err)
	}
	checkpoint := cloneMap(child.Checkpoint)
	checkpoint["branch"] = branch
	checkpoint["committed"] = "terminal"
	service := NewRunActivityService(fixture.store, fixture.store)
	service.now = func() time.Time { return fixture.now.Add(4 * time.Second) }
	if _, _, err := service.TransitionRun(t.Context(), fixture.scope, child.ID, RunTransitionRequest{
		ExpectedRevision: child.Revision, Status: AgentRunStatusCompleted,
		Actor: ActivityActor{Type: "worker", ID: "child-worker"}, LeaseOwner: "child-worker",
		Checkpoint: checkpoint, Output: map[string]interface{}{"message": "actual " + branch + " result"},
		BudgetUsageDelta: &BudgetUsage{Turns: 1},
	}); err != nil {
		t.Fatal(err)
	}
	child, err = fixture.store.GetAgentRun(t.Context(), fixture.scope, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	return child
}

func TestAgentRunWorkerDependencyRetirementLeavesIndependentTargetsUntouched(t *testing.T) {
	forDependencyRetirementStores(t, func(t *testing.T, store dependencyRetirementStore) {
		fixture := newDependencyRetirementFixtureWithPolicy(t, store, Scope{Kind: "tenant", ID: uuid.NewString()}, RunDependencyPolicy{Mode: FanInModeAny, FailureMode: DependencyFailureWait})
		fixture.finishChild(t, "alpha", AgentRunStatusCompleted)
		fixture.terminateSource(t, AgentRunStatusCanceled)
		before := fixture.snapshot(t)
		pool := newDependencyRepairTestPool(t, store, fixture.scope)
		pool.reconcileForkChildren(t.Context())
		after := fixture.snapshot(t)
		if after.group.Status != RunDependencyGroupCanceled || !reflect.DeepEqual(before.source, after.source) || !reflect.DeepEqual(before.children, after.children) {
			t.Fatalf("retirement changed independently progressing targets: %#v", after)
		}
	})
}

type dependencyRepairReadCountingStore struct {
	*MemoryStore
	runReads atomic.Int64
	runLists atomic.Int64
}

func (s *dependencyRepairReadCountingStore) GetAgentRun(ctx context.Context, scope Scope, id string) (*AgentRun, error) {
	s.runReads.Add(1)
	return s.MemoryStore.GetAgentRun(ctx, scope, id)
}

func (s *dependencyRepairReadCountingStore) ListAgentRuns(ctx context.Context, filter AgentRunFilter) ([]*AgentRun, error) {
	s.runLists.Add(1)
	return s.MemoryStore.ListAgentRuns(ctx, filter)
}

func TestAgentRunWorkerIdleDependencyMetadataDoesNotHydrateRunsOrHistory(t *testing.T) {
	store := &dependencyRepairReadCountingStore{MemoryStore: NewMemoryStore()}
	fixture := newDependencyRetirementFixtureWithPolicy(t, store.MemoryStore, Scope{Kind: "tenant", ID: uuid.NewString()}, RunDependencyPolicy{Mode: FanInModeAny, FailureMode: DependencyFailureWait})
	pool := newDependencyRepairTestPool(t, store, fixture.scope)
	for range 5 {
		pool.reconcileForkChildren(t.Context())
	}
	if store.runReads.Load() != 0 || store.runLists.Load() != 0 {
		t.Fatalf("unchanged waiting metadata hydrated Runs/history: reads=%d lists=%d", store.runReads.Load(), store.runLists.Load())
	}
	fixture.assertWaitingWork(t, 1)
}

func newDependencyRepairTestPool(t *testing.T, store KernelStore, scope Scope) *AgentRunWorkerPool {
	t.Helper()
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return nil, errors.New("reconciliation must not resolve provider authority")
	}), nil, AgentRunWorkerConfig{Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	return pool
}
