package runtime

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

type atomicForkStore interface {
	KernelStore
	RunDependencyStore
}

func TestDependencyFanOutAtomicallyCreatesTargetRuns(t *testing.T) {
	t.Parallel()
	stores := []struct {
		name string
		open func(*testing.T) (atomicForkStore, func())
	}{
		{name: "memory", open: func(*testing.T) (atomicForkStore, func()) { return NewMemoryStore(20), func() {} }},
		{name: "sqlite", open: func(t *testing.T) (atomicForkStore, func()) {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "fork.db"))
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
				if err != nil || persisted == nil || persisted.ParentRunID != record.SourceRun.ID || persisted.Status != AgentRunStatusQueued {
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
	store := NewMemoryStore(20)
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

func atomicTargetRunFixture(t *testing.T, store atomicForkStore) RunDependencyGroupCreateRecord {
	t.Helper()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "7"}
	portfolio := NewPortfolioService(store)
	source, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "parent", Source: RunSourceManual})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	targets := make([]*AgentRun, 0, 2)
	for _, id := range []string{"child-a", "child-b"} {
		target, err := buildAgentRun(ctx, store, CreateAgentRunRequest{
			Scope: scope, ParentRunID: source.ID, Owner: source.Owner, AssignedAgentID: source.AssignedAgentID,
			Goal: "branch " + id, Source: RunSourceRequest, Checkpoint: map[string]interface{}{"branch": id},
		}, id, now)
		if err != nil {
			t.Fatal(err)
		}
		targets = append(targets, target)
	}
	sealedAt := now
	group := &RunDependencyGroup{
		ID: "fork", Scope: scope, SourceRunID: source.ID,
		Policy:        RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast},
		ExpectedCount: 2, Status: RunDependencyGroupWaiting, Revision: 1, CreatedAt: now, UpdatedAt: now, SealedAt: &sealedAt,
		IdempotencyKey: "fork-once",
	}
	edges := []*RunDependency{
		{ID: "a", Scope: scope, GroupID: group.ID, SourceRunID: source.ID, TargetRunID: targets[0].ID, Kind: RunDependencyKindRun, State: RunDependencyStatePending, Required: true, Revision: 1, CreatedAt: now, UpdatedAt: now},
		{ID: "b", Scope: scope, GroupID: group.ID, SourceRunID: source.ID, TargetRunID: targets[1].ID, Kind: RunDependencyKindRun, State: RunDependencyStatePending, Required: true, Revision: 1, CreatedAt: now, UpdatedAt: now},
	}
	updatedSource := cloneAgentRun(source)
	updatedSource.Status = AgentRunStatusWaitingForDependency
	updatedSource.WakeCondition = &WakeCondition{Type: "run_dependencies", Reference: group.ID}
	updatedSource.Revision++
	updatedSource.UpdatedAt = now
	event := &ActivityEvent{ID: "fork-event", Scope: scope, RunID: source.ID, EventType: "dependency.group_created", Summary: "Forked two branches", Actor: ActivityActor{Type: "worker", ID: "worker"}, Visibility: ActivityVisibilityScope, CorrelationID: group.ID, CreatedAt: now}
	return RunDependencyGroupCreateRecord{Group: group, Dependencies: edges, TargetRuns: targets, SourceRun: updatedSource, ExpectedSourceRevision: source.Revision, Event: event}
}
