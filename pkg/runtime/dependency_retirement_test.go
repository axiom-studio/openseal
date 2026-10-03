package runtime

import (
	"context"
	"errors"
	"os"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

type dependencyRetirementStore interface {
	KernelStore
	DependencyKernelStore
	RunDependencyReconciliationStore
}

type dependencyRetirementFixture struct {
	store    dependencyRetirementStore
	scope    Scope
	groupID  string
	sourceID string
	childIDs map[string]string
	now      time.Time
}

type dependencyRetirementSnapshot struct {
	group    *RunDependencyGroup
	edges    []*RunDependency
	source   *AgentRun
	children map[string]*AgentRun
	events   []*ActivityEvent
}

func TestDependencyRetirementTerminalSourceDoesNotReviveOrCancelChildren(t *testing.T) {
	for _, status := range []AgentRunStatus{AgentRunStatusCanceled, AgentRunStatusFailed, AgentRunStatusCompleted} {
		t.Run(string(status), func(t *testing.T) {
			forDependencyRetirementStores(t, func(t *testing.T, store dependencyRetirementStore) {
				fixture := newDependencyRetirementFixture(t, store)
				completed := fixture.finishChild(t, "alpha", AgentRunStatusCompleted)
				fixture.resolveChild(t, "alpha", completed)
				fixture.terminateSource(t, status)
				fixture.assertWaitingWork(t, 1, true)
				before := fixture.snapshot(t)
				request := fixture.request(t)
				result, err := fixture.coordinator().ReconcileRunDependencyGroup(t.Context(), request)
				if err != nil || result == nil || result.Replayed {
					t.Fatalf("retire = %#v, %v", result, err)
				}
				if result.Group.Status != RunDependencyGroupCanceled || result.Group.ResolvedAt == nil || result.Group.WakeSignalID != "" || result.Evaluation.Wake {
					t.Fatalf("terminal source retirement manufactured a wake: %#v", result)
				}
				if err := result.Group.Validate(); err != nil {
					t.Fatalf("retired group is invalid: %v", err)
				}
				after := fixture.snapshot(t)
				if !reflect.DeepEqual(before.source, after.source) || !reflect.DeepEqual(before.children, after.children) {
					t.Fatalf("retirement changed source or independently executing targets: before=%#v after=%#v", before, after)
				}
				if !reflect.DeepEqual(dependencyRetirementEdge(t, before.edges, "alpha"), dependencyRetirementEdge(t, after.edges, "alpha")) {
					t.Fatal("retirement replaced an already committed child outcome")
				}
				unresolved := dependencyRetirementEdge(t, after.edges, "beta")
				if unresolved.State != RunDependencyStateCanceled || unresolved.ResolvedAt == nil || len(unresolved.Result) != 0 || len(unresolved.Artifacts) != 0 {
					t.Fatalf("unresolved edge was not retired without inventing a child result: %#v", unresolved)
				}
				fixture.assertWaitingWork(t, 0)
				fixture.assertReplayUnchanged(t, request, after)
				fixture.store = reopenDependencyRetirementStore(t, store)
				fixture.assertReplayUnchanged(t, request, fixture.snapshot(t))
				fixture.assertWaitingWork(t, 0)
			})
		})
	}
}

func TestDependencyRetirementReadsOnlyExactDurableChildOutcome(t *testing.T) {
	for _, status := range []AgentRunStatus{AgentRunStatusCompleted, AgentRunStatusFailed, AgentRunStatusCanceled} {
		t.Run(string(status), func(t *testing.T) {
			forDependencyRetirementStores(t, func(t *testing.T, store dependencyRetirementStore) {
				fixture := newDependencyRetirementFixture(t, store)
				child := fixture.finishChild(t, "alpha", status)
				fixture.assertWaitingWork(t, 1, true)
				before := fixture.snapshot(t)
				result, err := fixture.coordinator().ReconcileRunDependencyGroup(t.Context(), fixture.request(t))
				if err != nil || result == nil || result.Replayed {
					t.Fatalf("reconcile child outcome = %#v, %v", result, err)
				}
				after := fixture.snapshot(t)
				edge := dependencyRetirementEdge(t, after.edges, "alpha")
				if edge.State != dependencyRetirementState(status) || !reflect.DeepEqual(edge.Result, dependencyRetirementChildResult(edge.ID, child)) || edge.Error != child.Error {
					t.Fatalf("edge does not match its authoritative child: edge=%#v child=%#v", edge, child)
				}
				if after.group.Status != RunDependencyGroupWaiting || after.source.Status != AgentRunStatusWaitingForDependency || result.Evaluation.Wake {
					t.Fatalf("partial all-branches join woke before the remaining target finished: %#v", result)
				}
				if !reflect.DeepEqual(before.children, after.children) || !reflect.DeepEqual(dependencyRetirementEdge(t, before.edges, "beta"), dependencyRetirementEdge(t, after.edges, "beta")) {
					t.Fatal("reading one terminal child changed another target, lease, or edge")
				}
				fixture.assertReplayUnchanged(t, fixture.request(t), after)
				fixture.assertWaitingWork(t, 1, false)
				fixture.finishChild(t, "beta", AgentRunStatusCompleted)
				request := fixture.request(t)
				joined, err := fixture.coordinator().ReconcileRunDependencyGroup(t.Context(), request)
				expected := RunDependencyGroupSatisfied
				if status != AgentRunStatusCompleted {
					expected = RunDependencyGroupFailed
				}
				if err != nil || joined == nil || joined.Group.Status != expected || joined.Source.Status != AgentRunStatusQueued || !joined.Evaluation.Wake {
					t.Fatalf("completed all-branches join = %#v, %v", joined, err)
				}
				fixture.assertReplayUnchanged(t, request, fixture.snapshot(t))
				fixture.assertWaitingWork(t, 0)
			})
		})
	}
}

func TestDependencyRetirementHonorsAnyQuorumAndFailFastWithoutChangingTargets(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		policy         RunDependencyPolicy
		firstStatus    AgentRunStatus
		expectedStatus RunDependencyGroupStatus
		needsSecond    bool
	}{
		{name: "any", policy: RunDependencyPolicy{Mode: FanInModeAny, FailureMode: DependencyFailureWait}, firstStatus: AgentRunStatusCompleted, expectedStatus: RunDependencyGroupSatisfied},
		{name: "quorum", policy: RunDependencyPolicy{Mode: FanInModeQuorum, Quorum: 2, FailureMode: DependencyFailureWait}, firstStatus: AgentRunStatusCompleted, expectedStatus: RunDependencyGroupSatisfied, needsSecond: true},
		{name: "fail_fast", policy: RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureFailFast}, firstStatus: AgentRunStatusFailed, expectedStatus: RunDependencyGroupFailed},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			forDependencyRetirementStores(t, func(t *testing.T, store dependencyRetirementStore) {
				fixture := newDependencyRetirementFixtureWithPolicy(t, store, Scope{Kind: "tenant", ID: uuid.NewString()}, testCase.policy)
				fixture.finishChild(t, "alpha", testCase.firstStatus)
				if testCase.needsSecond {
					partial, err := fixture.coordinator().ReconcileRunDependencyGroup(t.Context(), fixture.request(t))
					if err != nil || partial == nil || partial.Group.Status != RunDependencyGroupWaiting || partial.Source.Status != AgentRunStatusWaitingForDependency || partial.Evaluation.Wake {
						t.Fatalf("quorum woke before its threshold: %#v, %v", partial, err)
					}
					fixture.finishChild(t, "beta", AgentRunStatusCompleted)
				}
				before := fixture.snapshot(t)
				request := fixture.request(t)
				result, err := fixture.coordinator().ReconcileRunDependencyGroup(t.Context(), request)
				if err != nil || result == nil || result.Replayed || result.Group.Status != testCase.expectedStatus || result.Source.Status != AgentRunStatusQueued || !result.Evaluation.Wake || result.Source.LastWakeSignalID != result.Group.WakeSignalID {
					t.Fatalf("policy fan-in did not commit one exact wake: %#v, %v", result, err)
				}
				after := fixture.snapshot(t)
				if !reflect.DeepEqual(before.children, after.children) {
					t.Fatal("early fan-in changed independently progressing targets or their leases")
				}
				activeBranch := "beta"
				if testCase.needsSecond {
					activeBranch = "gamma"
				}
				if after.children[activeBranch].Status != AgentRunStatusRunning || !reflect.DeepEqual(dependencyRetirementEdge(t, before.edges, activeBranch), dependencyRetirementEdge(t, after.edges, activeBranch)) {
					t.Fatalf("policy reconciliation changed a still-running target's edge: %#v", after)
				}
				fixture.assertReplayUnchanged(t, request, after)
				fixture.assertWaitingWork(t, 0)
			})
		})
	}
}

func TestDependencyRetirementPausedSourceKeepsResultsUntilNormalResume(t *testing.T) {
	for _, direct := range []bool{false, true} {
		name := "reconcile"
		if direct {
			name = "direct_resolution"
		}
		t.Run(name, func(t *testing.T) {
			forDependencyRetirementStores(t, func(t *testing.T, store dependencyRetirementStore) {
				fixture := newDependencyRetirementFixture(t, store)
				fixture.command(t, AgentRunCommandPause)
				fixture.assertWaitingWork(t, 1, false)
				paused := fixture.snapshot(t).source
				for _, branch := range []string{"alpha", "beta"} {
					child := fixture.finishChild(t, branch, AgentRunStatusCompleted)
					fixture.assertWaitingWork(t, 1, true)
					var result *RunDependencyResult
					var err error
					if direct {
						result = fixture.resolveChild(t, branch, child)
					} else {
						result, err = fixture.coordinator().ReconcileRunDependencyGroup(t.Context(), fixture.request(t))
					}
					if err != nil || result == nil || result.Source.Status != AgentRunStatusPaused || result.Evaluation.Wake {
						t.Fatalf("paused result resumed work: %#v, %v", result, err)
					}
					if branch == "alpha" && (result.Source.PausedFrom != AgentRunStatusWaitingForDependency || result.Source.PausedWakeCondition == nil) {
						t.Fatalf("partial paused join lost its remaining dependency: %#v", result.Source)
					}
					if branch == "alpha" {
						fixture.assertWaitingWork(t, 1, false)
					}
				}
				joined := fixture.snapshot(t)
				if joined.group.Status != RunDependencyGroupSatisfied || joined.source.PausedFrom != AgentRunStatusQueued || joined.source.PausedWakeCondition != nil || joined.source.WakeCondition != nil {
					t.Fatalf("completed paused join is not ready for normal resume: %#v", joined)
				}
				if !reflect.DeepEqual(paused.Checkpoint, joined.source.Checkpoint) || !reflect.DeepEqual(paused.Budget, joined.source.Budget) || joined.source.BudgetUsage != paused.BudgetUsage {
					t.Fatal("dependency completion replaced unrelated paused state or spent the parent budget")
				}
				for _, edge := range joined.edges {
					if !reflect.DeepEqual(edge.Result, dependencyRetirementChildResult(edge.ID, joined.children[edge.ID])) {
						t.Fatalf("paused join lost child result: %#v", edge)
					}
				}
				request := fixture.request(t)
				fixture.command(t, AgentRunCommandResume)
				resumed := fixture.snapshot(t)
				if resumed.source.Status != AgentRunStatusQueued || resumed.source.WakeCondition != nil || resumed.source.PausedWakeCondition != nil || !reflect.DeepEqual(resumed.source.Output, joined.source.Output) {
					t.Fatalf("normal resume restored a completed wait or lost output: %#v", resumed.source)
				}
				fixture.assertReplayUnchanged(t, request, resumed)
			})
		})
	}
}

func TestDependencyRetirementFencesGroupSourceAndTenantBeforeWrites(t *testing.T) {
	forDependencyRetirementStores(t, func(t *testing.T, store dependencyRetirementStore) {
		fixture := newDependencyRetirementFixture(t, store)
		fixture.finishChild(t, "alpha", AgentRunStatusCompleted)
		before := fixture.snapshot(t)
		request := fixture.request(t)
		for _, testCase := range []struct {
			name     string
			mutate   func(*ReconcileRunDependencyGroupRequest)
			expected error
		}{
			{name: "group_revision", mutate: func(r *ReconcileRunDependencyGroupRequest) { r.ExpectedGroupRevision++ }, expected: ErrRevisionConflict},
			{name: "source_revision", mutate: func(r *ReconcileRunDependencyGroupRequest) { r.ExpectedSourceRevision++ }, expected: ErrRevisionConflict},
			{name: "foreign_scope", mutate: func(r *ReconcileRunDependencyGroupRequest) { r.Scope.ID = "foreign-tenant" }, expected: ErrDependencyGroupNotFound},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				invalid := request
				testCase.mutate(&invalid)
				if _, err := fixture.coordinator().ReconcileRunDependencyGroup(t.Context(), invalid); !errors.Is(err, testCase.expected) {
					t.Fatalf("reconcile error = %v, want %v", err, testCase.expected)
				}
				fixture.assertSnapshot(t, before)
			})
		}
		if result, err := fixture.coordinator().ReconcileRunDependencyGroup(t.Context(), request); err != nil || result == nil || result.Replayed {
			t.Fatalf("valid reconciliation after rejected writes = %#v, %v", result, err)
		}
	})
}

func TestDependencyRetirementIdleJoinDoesNotWrite(t *testing.T) {
	forDependencyRetirementStores(t, func(t *testing.T, store dependencyRetirementStore) {
		fixture := newDependencyRetirementFixture(t, store)
		before := fixture.snapshot(t)
		fixture.assertReplayUnchanged(t, fixture.request(t), before)
		fixture.assertWaitingWork(t, 1)
	})
}

func TestDependencyRetirementWaitingScanIsBoundedScopedAndUsesExactKeyset(t *testing.T) {
	forDependencyRetirementStores(t, func(t *testing.T, store dependencyRetirementStore) {
		scope := Scope{Kind: "tenant", ID: uuid.NewString()}
		fixtures := make([]*dependencyRetirementFixture, 0, 3)
		want := make([]RunDependencyGroupWork, 0, 3)
		for range 3 {
			fixture := newDependencyRetirementFixtureInScope(t, store, scope)
			fixtures = append(fixtures, fixture)
			snapshot := fixture.snapshot(t)
			want = append(want, RunDependencyGroupWork{Scope: scope, GroupID: fixture.groupID, GroupRevision: snapshot.group.Revision, SourceRunID: fixture.sourceID, SourceRevision: snapshot.source.Revision})
		}
		foreign := newDependencyRetirementFixture(t, store)
		foreignBefore := foreign.snapshot(t)
		sort.Slice(want, func(i, j int) bool { return want[i].GroupID < want[j].GroupID })
		coordinator := fixtures[0].coordinator()
		first, err := coordinator.ListWaitingRunDependencyGroups(t.Context(), scope, "", 2)
		if err != nil || !reflect.DeepEqual(first, want[:2]) {
			t.Fatalf("first keyset page = %#v, %v; want %#v", first, err, want[:2])
		}
		second, err := coordinator.ListWaitingRunDependencyGroups(t.Context(), scope, first[1].GroupID, 2)
		if err != nil || !reflect.DeepEqual(second, want[2:]) {
			t.Fatalf("next exclusive keyset page = %#v, %v; want %#v", second, err, want[2:])
		}
		last, err := coordinator.ListWaitingRunDependencyGroups(t.Context(), scope, second[0].GroupID, 2)
		if err != nil || len(last) != 0 {
			t.Fatalf("exhausted keyset page = %#v, %v", last, err)
		}
		for _, limit := range []int{0, -1, 101} {
			if _, err := coordinator.ListWaitingRunDependencyGroups(t.Context(), scope, "", limit); !errors.Is(err, ErrInvalidRunDependency) {
				t.Fatalf("unbounded/invalid page %d was accepted: %v", limit, err)
			}
			if _, err := store.ListWaitingRunDependencyGroups(t.Context(), scope, "", limit); !errors.Is(err, ErrInvalidRunDependency) {
				t.Fatalf("direct store unbounded/invalid page %d was accepted: %v", limit, err)
			}
		}
		retired := fixtures[1]
		retired.command(t, AgentRunCommandCancel)
		if _, err := retired.coordinator().ReconcileRunDependencyGroup(t.Context(), retired.request(t)); err != nil {
			t.Fatal(err)
		}
		remaining, err := coordinator.ListWaitingRunDependencyGroups(t.Context(), scope, "", 100)
		if err != nil || len(remaining) != 2 {
			t.Fatalf("retired group remained in indexed waiting scan = %#v, %v", remaining, err)
		}
		for _, work := range remaining {
			if work.GroupID == retired.groupID || work.Scope != scope {
				t.Fatalf("waiting scan returned retired or foreign work: %#v", work)
			}
		}
		foreign.assertWaitingWork(t, 1)
		foreign.assertSnapshot(t, foreignBefore)
	})
}

func TestDependencyRetirementConcurrentReconciliationCommitsOnce(t *testing.T) {
	for _, terminalSource := range []bool{false, true} {
		name := "children_completed"
		if terminalSource {
			name = "source_canceled"
		}
		t.Run(name, func(t *testing.T) {
			forDependencyRetirementStores(t, func(t *testing.T, store dependencyRetirementStore) {
				fixture := newDependencyRetirementFixture(t, store)
				if terminalSource {
					fixture.terminateSource(t, AgentRunStatusCanceled)
				} else {
					fixture.finishChild(t, "alpha", AgentRunStatusCompleted)
					fixture.finishChild(t, "beta", AgentRunStatusCompleted)
				}
				before := fixture.snapshot(t)
				request := fixture.request(t)
				start := make(chan struct{})
				results := make(chan *RunDependencyResult, 2)
				errs := make(chan error, 2)
				var workers sync.WaitGroup
				for range 2 {
					workers.Add(1)
					go func() {
						defer workers.Done()
						<-start
						result, err := fixture.coordinator().ReconcileRunDependencyGroup(t.Context(), request)
						results <- result
						errs <- err
					}()
				}
				close(start)
				workers.Wait()
				close(results)
				close(errs)
				writes := 0
				for err := range errs {
					if err != nil && !errors.Is(err, ErrRevisionConflict) {
						t.Fatalf("concurrent reconcile error: %v", err)
					}
				}
				for result := range results {
					if result != nil && !result.Replayed {
						writes++
					}
				}
				if writes != 1 {
					t.Fatalf("concurrent reconciliation committed %d times", writes)
				}
				after := fixture.snapshot(t)
				if !reflect.DeepEqual(before.children, after.children) {
					t.Fatal("concurrent reconciliation changed child execution records")
				}
				if terminalSource && !reflect.DeepEqual(before.source, after.source) {
					t.Fatal("concurrent retirement revived or rewrote terminal source")
				}
				if !terminalSource && (after.source.Status != AgentRunStatusQueued || after.source.LastWakeSignalID == "" || after.source.LastWakeSignalID != after.group.WakeSignalID) {
					t.Fatalf("fan-in lacks its one durable wake: %#v", after)
				}
				fixture.assertReplayUnchanged(t, request, after)
				fixture.assertWaitingWork(t, 0)
			})
		})
	}
}

type dependencyRetirementCancelBeforeStore struct {
	dependencyRetirementStore
	before func()
}

func (s *dependencyRetirementCancelBeforeStore) ReconcileRunDependencyGroup(ctx context.Context, record RunDependencyGroupReconciliationRecord) (*RunDependencyResult, error) {
	if s.before != nil {
		before := s.before
		s.before = nil
		before()
	}
	return s.dependencyRetirementStore.ReconcileRunDependencyGroup(ctx, record)
}

func TestDependencyRetirementSourceCancellationBetweenReadAndCommitIsFenced(t *testing.T) {
	forDependencyRetirementStores(t, func(t *testing.T, store dependencyRetirementStore) {
		fixture := newDependencyRetirementFixture(t, store)
		fixture.finishChild(t, "alpha", AgentRunStatusCompleted)
		request := fixture.request(t)
		var canceled *AgentRun
		var before dependencyRetirementSnapshot
		wrapped := &dependencyRetirementCancelBeforeStore{dependencyRetirementStore: store, before: func() {
			canceled = fixture.command(t, AgentRunCommandCancel)
			before = fixture.snapshot(t)
		}}
		coordinator := NewDependencyCoordinator(wrapped)
		coordinator.now = func() time.Time { return fixture.now.Add(10 * time.Second) }
		if _, err := coordinator.ReconcileRunDependencyGroup(t.Context(), request); !errors.Is(err, ErrRevisionConflict) {
			t.Fatalf("stale source accepted across cancellation: %v", err)
		}
		fixture.assertSnapshot(t, before)
		result, err := fixture.coordinator().ReconcileRunDependencyGroup(t.Context(), fixture.request(t))
		if err != nil || result == nil || result.Group.Status != RunDependencyGroupCanceled || result.Group.WakeSignalID != "" || !reflect.DeepEqual(canceled, result.Source) {
			t.Fatalf("retry did not retire the canceled source: %#v, %v", result, err)
		}
		for _, edge := range result.Dependencies {
			if edge.State != RunDependencyStateCanceled || len(edge.Result) != 0 {
				t.Fatalf("retirement forged a canceled branch outcome from a completed child: %#v", edge)
			}
		}
	})
}

func TestDependencyRetirementDirectResolutionCannotReviveCanceledSource(t *testing.T) {
	forDependencyRetirementStores(t, func(t *testing.T, store dependencyRetirementStore) {
		fixture := newDependencyRetirementFixture(t, store)
		child := fixture.finishChild(t, "alpha", AgentRunStatusCompleted)
		fixture.command(t, AgentRunCommandCancel)
		before := fixture.snapshot(t)
		result := fixture.resolveChild(t, "alpha", child)
		after := fixture.snapshot(t)
		if result.Group.Status != RunDependencyGroupCanceled || result.Group.WakeSignalID != "" || result.Evaluation.Wake || !reflect.DeepEqual(before.source, after.source) || !reflect.DeepEqual(before.children, after.children) {
			t.Fatalf("late direct resolution changed canceled work: %#v", result)
		}
		for _, edge := range after.edges {
			if edge.State != RunDependencyStateCanceled || len(edge.Result) != 0 || len(edge.Artifacts) != 0 {
				t.Fatalf("late direct resolution stored caller-supplied results while retiring canceled work: %#v", edge)
			}
		}
		fixture.assertWaitingWork(t, 0)
		fixture.assertReplayUnchanged(t, fixture.request(t), after)
	})
}

func TestDependencyRetirementHistoricalTypedSourceCleansUpAfterAuthorityChanges(t *testing.T) {
	for _, authority := range []string{"maintenance", "revoked_binding"} {
		t.Run(authority, func(t *testing.T) {
			forDependencyRetirementStores(t, func(t *testing.T, store dependencyRetirementStore) {
				catalog, scope, binding := runSkillDependencyFixture(t, store)
				now := time.Now().UTC().Round(0)
				portfolio := NewPortfolioService(store)
				portfolio.now = func() time.Time { return now }
				source, err := portfolio.CreateAgentRun(t.Context(), runSkillDependencyRequest(scope))
				if err != nil {
					t.Fatal(err)
				}
				child, err := portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: scope, ParentRunID: source.ID, Owner: source.Owner, AssignedAgentID: "helper", Goal: "Independent branch", Source: RunSourceRequest, Checkpoint: map[string]interface{}{"committed": "child work"}})
				if err != nil {
					t.Fatal(err)
				}
				coordinator := NewDependencyCoordinator(store)
				coordinator.now = func() time.Time { return now.Add(time.Second) }
				group, err := coordinator.CreateRunDependencyGroup(t.Context(), CreateRunDependencyGroupRequest{Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision, Policy: RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureWait}, Dependencies: []RunDependencySpec{{ID: "branch", TargetRunID: child.ID, Kind: RunDependencyKindRun}}, Actor: ActivityActor{Type: "worker", ID: "typed-worker"}})
				if err != nil {
					t.Fatal(err)
				}
				fixture := &dependencyRetirementFixture{store: store, scope: scope, groupID: group.Group.ID, sourceID: source.ID, childIDs: map[string]string{"branch": child.ID}, now: now}
				// The source ends before mutable executable authority is replaced;
				// this is cleanup of history, not admission of new execution.
				activity := NewRunActivityService(store, store)
				activity.now = func() time.Time { return now.Add(2 * time.Second) }
				if _, _, err := activity.TransitionRun(t.Context(), scope, source.ID, RunTransitionRequest{ExpectedRevision: group.Source.Revision, Status: AgentRunStatusCanceled, Actor: ActivityActor{Type: "user", ID: "operator"}}); err != nil {
					t.Fatal(err)
				}
				expectedAdmissionError := ErrRunSkillDependencyUnavailable
				if authority == "maintenance" {
					request := runSkillDependencyMaintenanceRequest(scope)
					request.Now = now.Add(3 * time.Second)
					gate, err := store.(SkillRuntimeMaintenanceStore).AcquireSkillRuntimeMaintenance(t.Context(), request)
					if err != nil || gate == nil || !gate.Active {
						t.Fatalf("maintenance after terminal source = %#v, %v", gate, err)
					}
					expectedAdmissionError = ErrSkillRuntimeMaintenance
				} else {
					revoked := cloneUpgradeBinding(binding)
					revoked.Disabled, revoked.Revision, revoked.UpdatedAt = true, binding.Revision+1, now.Add(3*time.Second)
					if err := catalog.Bind(t.Context(), revoked); err != nil {
						t.Fatal(err)
					}
				}
				// Verify the fixture's new execution authority is really denied.
				if run, err := portfolio.CreateAgentRun(t.Context(), runSkillDependencyRequest(scope)); run != nil || !errors.Is(err, expectedAdmissionError) {
					t.Fatalf("fresh typed work bypassed changed authority: %#v, %v", run, err)
				}
				before := fixture.snapshot(t)
				if before.source.Context[capabilityInvocationContextKey] == nil || before.children["branch"].Status != AgentRunStatusQueued {
					t.Fatal("fixture lost historical typed intent or active independent child")
				}
				edge := before.edges[0]
				result, err := coordinator.ResolveRunDependency(t.Context(), ResolveRunDependencyRequest{Scope: scope, GroupID: fixture.groupID, DependencyID: edge.ID, ExpectedDependencyRevision: edge.Revision, State: RunDependencyStateSatisfied, Result: map[string]interface{}{"callerClaim": "must not become child truth"}, Actor: ActivityActor{Type: "worker", ID: "dependency-reconciler"}})
				if err != nil || result == nil || result.Group.Status != RunDependencyGroupCanceled || result.Group.WakeSignalID != "" || result.Evaluation.Wake {
					t.Fatalf("historical relationship cleanup required fresh executable authority: %#v, %v", result, err)
				}
				after := fixture.snapshot(t)
				if !reflect.DeepEqual(before.source, after.source) || !reflect.DeepEqual(before.children, after.children) {
					t.Fatal("cleanup rewrote historical source identity or independently progressing child")
				}
				if after.edges[0].State != RunDependencyStateCanceled || len(after.edges[0].Result) != 0 || len(after.edges[0].Artifacts) != 0 {
					t.Fatalf("cleanup stored untrusted completion intent: %#v", after.edges[0])
				}
				fixture.assertReplayUnchanged(t, fixture.request(t), after)
				fixture.assertWaitingWork(t, 0)
			})
		})
	}
}

func forDependencyRetirementStores(t *testing.T, test func(*testing.T, dependencyRetirementStore)) {
	t.Helper()
	forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
		store, ok := kernel.(dependencyRetirementStore)
		if !ok {
			t.Fatalf("%T does not implement atomic dependency reconciliation", kernel)
		}
		test(t, store)
	})
}

func newDependencyRetirementFixture(t *testing.T, store dependencyRetirementStore) *dependencyRetirementFixture {
	t.Helper()
	return newDependencyRetirementFixtureInScope(t, store, Scope{Kind: "tenant", ID: uuid.NewString()})
}

func newDependencyRetirementFixtureInScope(t *testing.T, store dependencyRetirementStore, scope Scope) *dependencyRetirementFixture {
	t.Helper()
	return newDependencyRetirementFixtureWithPolicy(t, store, scope, RunDependencyPolicy{Mode: FanInModeAll, FailureMode: DependencyFailureWait})
}

func newDependencyRetirementFixtureWithPolicy(t *testing.T, store dependencyRetirementStore, scope Scope, policy RunDependencyPolicy) *dependencyRetirementFixture {
	t.Helper()
	fixture := &dependencyRetirementFixture{store: store, scope: scope, childIDs: make(map[string]string), now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return fixture.now }
	source, err := portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: fixture.scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "lead"}, AssignedAgentID: "lead", Goal: "Join two independently running branches", Source: RunSourceManual, Checkpoint: map[string]interface{}{"committed": "preserve"}, Budget: &BudgetPolicy{MaxTurns: 10}})
	if err != nil {
		t.Fatal(err)
	}
	fixture.sourceID = source.ID
	scheduler := NewAgentRunScheduler(store)
	scheduler.now = func() time.Time { return fixture.now.Add(time.Second) }
	claimed, err := scheduler.ClaimNext(t.Context(), AgentRunClaimRequest{Scope: fixture.scope, WorkerID: "source-worker", LeaseDuration: time.Minute})
	if err != nil || claimed == nil || claimed.ID != source.ID {
		t.Fatalf("claim source=%#v, %v", claimed, err)
	}
	forks := NewRunForkCoordinator(store)
	forks.now = func() time.Time { return fixture.now.Add(2 * time.Second) }
	branches := []RunForkBranch{
		{ID: "alpha", Goal: "Branch alpha", Checkpoint: map[string]interface{}{"branch": "alpha"}, Budget: &BudgetPolicy{MaxTurns: 2}},
		{ID: "beta", Goal: "Branch beta", Checkpoint: map[string]interface{}{"branch": "beta"}, Budget: &BudgetPolicy{MaxTurns: 2}},
	}
	if policy.Mode == FanInModeQuorum {
		branches = append(branches, RunForkBranch{ID: "gamma", Goal: "Branch gamma", Checkpoint: map[string]interface{}{"branch": "gamma"}, Budget: &BudgetPolicy{MaxTurns: 2}})
	}
	var group *RunDependencyResult
	if policy.Mode == FanInModeQuorum {
		// Forks expose all/any today. The general dependency coordinator owns
		// quorum groups and accepts normally created independent target Runs.
		dependencies := make([]RunDependencySpec, 0, len(branches))
		for _, branch := range branches {
			child, err := portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{
				Scope: fixture.scope, ParentRunID: source.ID, Owner: source.Owner, AssignedAgentID: source.AssignedAgentID,
				Goal: branch.Goal, Source: RunSourceRequest, Checkpoint: branch.Checkpoint, Budget: branch.Budget,
			})
			if err != nil {
				t.Fatal(err)
			}
			dependencies = append(dependencies, RunDependencySpec{ID: branch.ID, TargetRunID: child.ID, Kind: RunDependencyKindRun})
		}
		coordinator := fixture.coordinator()
		coordinator.now = func() time.Time { return fixture.now.Add(2 * time.Second) }
		group, err = coordinator.CreateRunDependencyGroup(t.Context(), CreateRunDependencyGroupRequest{
			Scope: fixture.scope, SourceRunID: source.ID, ExpectedSourceRevision: claimed.Revision,
			Policy: policy, Dependencies: dependencies, Actor: ActivityActor{Type: "worker", ID: "source-worker"},
		})
	} else {
		var created *RunForkResult
		created, err = forks.Create(t.Context(), CreateRunForkRequest{
			Scope: fixture.scope, SourceRunID: source.ID, ExpectedSourceRevision: claimed.Revision, WorkerID: "source-worker", ForkID: "retirement-wave",
			Policy: policy, Branches: branches, ContinuationCheckpoint: map[string]interface{}{"committed": "preserve", "continuation": "join"},
		})
		if err == nil {
			group = created.DependencyGroup
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	fixture.groupID = group.Group.ID
	for _, edge := range group.Dependencies {
		fixture.childIDs[edge.ID] = edge.TargetRunID
	}
	if len(fixture.childIDs) != len(branches) || fixture.childIDs["alpha"] == "" || fixture.childIDs["beta"] == "" {
		t.Fatalf("fork edges=%#v", group.Dependencies)
	}
	scheduler.now = func() time.Time { return fixture.now.Add(3 * time.Second) }
	for range len(branches) {
		child, err := scheduler.ClaimNext(t.Context(), AgentRunClaimRequest{Scope: fixture.scope, WorkerID: "child-worker", LeaseDuration: time.Minute})
		if err != nil || child == nil || child.ParentRunID != source.ID {
			t.Fatalf("claim child=%#v, %v", child, err)
		}
	}
	return fixture
}

func (f *dependencyRetirementFixture) coordinator() *DependencyCoordinator {
	coordinator := NewDependencyCoordinator(f.store)
	coordinator.now = func() time.Time { return f.now.Add(10 * time.Second) }
	return coordinator
}

func (f *dependencyRetirementFixture) request(t *testing.T) ReconcileRunDependencyGroupRequest {
	t.Helper()
	snapshot := f.snapshot(t)
	return ReconcileRunDependencyGroupRequest{Scope: f.scope, GroupID: f.groupID, ExpectedGroupRevision: snapshot.group.Revision, ExpectedSourceRevision: snapshot.source.Revision, Actor: ActivityActor{Type: "worker", ID: "dependency-reconciler"}, Visibility: ActivityVisibilityScope}
}

func (f *dependencyRetirementFixture) snapshot(t *testing.T) dependencyRetirementSnapshot {
	t.Helper()
	group, err := f.store.GetRunDependencyGroup(t.Context(), f.scope, f.groupID)
	if err != nil || group == nil {
		t.Fatalf("get group=%#v, %v", group, err)
	}
	edges, err := f.store.ListRunDependencies(t.Context(), f.scope, f.groupID)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].ID < edges[j].ID })
	source, err := f.store.GetAgentRun(t.Context(), f.scope, f.sourceID)
	if err != nil || source == nil {
		t.Fatalf("get source=%#v, %v", source, err)
	}
	children := make(map[string]*AgentRun, len(f.childIDs))
	for branch, id := range f.childIDs {
		child, err := f.store.GetAgentRun(t.Context(), f.scope, id)
		if err != nil || child == nil {
			t.Fatalf("get child=%#v, %v", child, err)
		}
		children[branch] = child
	}
	events, err := f.store.ListActivity(t.Context(), ActivityFilter{Scope: f.scope, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	return dependencyRetirementSnapshot{group: group, edges: edges, source: source, children: children, events: events}
}

func (f *dependencyRetirementFixture) finishChild(t *testing.T, branch string, status AgentRunStatus) *AgentRun {
	t.Helper()
	child, err := f.store.GetAgentRun(t.Context(), f.scope, f.childIDs[branch])
	if err != nil || child == nil {
		t.Fatalf("get child=%#v, %v", child, err)
	}
	service := NewRunActivityService(f.store, f.store)
	service.now = func() time.Time { return f.now.Add(4 * time.Second) }
	errorText := ""
	if status != AgentRunStatusCompleted {
		errorText = "durable " + string(status) + " child outcome"
	}
	_, _, err = service.TransitionRun(t.Context(), f.scope, child.ID, RunTransitionRequest{ExpectedRevision: child.Revision, Status: status, Actor: ActivityActor{Type: "worker", ID: "child-worker"}, LeaseOwner: "child-worker", Checkpoint: map[string]interface{}{"branch": branch, "committed": "terminal"}, Output: map[string]interface{}{"message": "actual " + branch + " result", "nested": map[string]interface{}{"verified": true}}, Error: errorText, BudgetUsageDelta: &BudgetUsage{Turns: 1}})
	if err != nil {
		t.Fatal(err)
	}
	child, err = f.store.GetAgentRun(t.Context(), f.scope, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	return child
}

func (f *dependencyRetirementFixture) resolveChild(t *testing.T, branch string, child *AgentRun) *RunDependencyResult {
	t.Helper()
	edge := dependencyRetirementEdge(t, f.snapshot(t).edges, branch)
	result, err := f.coordinator().ResolveRunDependency(t.Context(), ResolveRunDependencyRequest{Scope: f.scope, GroupID: f.groupID, DependencyID: edge.ID, ExpectedDependencyRevision: edge.Revision, State: dependencyRetirementState(child.Status), Result: dependencyRetirementChildResult(edge.ID, child), Error: child.Error, Actor: ActivityActor{Type: "worker", ID: "dependency-reconciler"}})
	if err != nil || result == nil {
		t.Fatalf("resolve child=%#v, %v", result, err)
	}
	return result
}

func (f *dependencyRetirementFixture) command(t *testing.T, kind AgentRunCommandKind) *AgentRun {
	t.Helper()
	source, err := f.store.GetAgentRun(t.Context(), f.scope, f.sourceID)
	if err != nil {
		t.Fatal(err)
	}
	commands := NewRunCommandService(f.store)
	commands.now = func() time.Time { return f.now.Add(5 * time.Second) }
	result, err := commands.CommandAgentRun(t.Context(), AgentRunCommandRequest{Scope: f.scope, RunID: f.sourceID, ExpectedRevision: source.Revision, Kind: kind, Actor: ActivityActor{Type: "user", ID: "operator"}})
	if err != nil {
		t.Fatal(err)
	}
	// Fetch the backend's canonical JSON representation for cross-store oracles.
	source, err = f.store.GetAgentRun(t.Context(), f.scope, result.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func (f *dependencyRetirementFixture) terminateSource(t *testing.T, status AgentRunStatus) {
	t.Helper()
	if status == AgentRunStatusCanceled {
		f.command(t, AgentRunCommandCancel)
		return
	}
	service := NewRunActivityService(f.store, f.store)
	service.now = func() time.Time { return f.now.Add(5 * time.Second) }
	source := f.snapshot(t).source
	if status == AgentRunStatusCompleted {
		var err error
		source, _, err = service.TransitionRun(t.Context(), f.scope, source.ID, RunTransitionRequest{ExpectedRevision: source.Revision, Status: AgentRunStatusRunning, Actor: ActivityActor{Type: "user", ID: "operator"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	errorText := ""
	if status == AgentRunStatusFailed {
		errorText = "parent execution failed"
	}
	if _, _, err := service.TransitionRun(t.Context(), f.scope, source.ID, RunTransitionRequest{ExpectedRevision: source.Revision, Status: status, Actor: ActivityActor{Type: "user", ID: "operator"}, Error: errorText}); err != nil {
		t.Fatal(err)
	}
}

func (f *dependencyRetirementFixture) assertSnapshot(t *testing.T, want dependencyRetirementSnapshot) {
	t.Helper()
	if got := f.snapshot(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("reconciliation mutated durable facts on rejection or replay:\nwant=%#v\ngot=%#v", want, got)
	}
}

func (f *dependencyRetirementFixture) assertReplayUnchanged(t *testing.T, request ReconcileRunDependencyGroupRequest, want dependencyRetirementSnapshot) {
	t.Helper()
	for range 3 {
		result, err := f.coordinator().ReconcileRunDependencyGroup(t.Context(), request)
		if err != nil || result == nil || !result.Replayed || len(result.Events) != 0 || result.Evaluation.Wake {
			t.Fatalf("replay made work or emitted a wake: %#v, %v", result, err)
		}
		f.assertSnapshot(t, want)
	}
}

func (f *dependencyRetirementFixture) assertWaitingWork(t *testing.T, want int, ready ...bool) {
	t.Helper()
	work, err := f.store.ListWaitingRunDependencyGroups(t.Context(), f.scope, "", 100)
	if err != nil || len(work) != want {
		t.Fatalf("waiting work=%#v, %v; want %d", work, err, want)
	}
	if want == 1 {
		snapshot := f.snapshot(t)
		wantReady := false
		if len(ready) != 0 {
			wantReady = ready[0]
		}
		if work[0].GroupID != f.groupID || work[0].SourceRunID != f.sourceID || work[0].GroupRevision != snapshot.group.Revision || work[0].SourceRevision != snapshot.source.Revision || work[0].Scope != f.scope || work[0].Ready != wantReady {
			t.Fatalf("waiting projection is not exact: %#v", work[0])
		}
	}
}

func dependencyRetirementEdge(t *testing.T, edges []*RunDependency, id string) *RunDependency {
	t.Helper()
	for _, edge := range edges {
		if edge.ID == id {
			return edge
		}
	}
	t.Fatalf("dependency %s not found in %#v", id, edges)
	return nil
}

func dependencyRetirementState(status AgentRunStatus) RunDependencyState {
	switch status {
	case AgentRunStatusCompleted:
		return RunDependencyStateSatisfied
	case AgentRunStatusFailed:
		return RunDependencyStateFailed
	default:
		return RunDependencyStateCanceled
	}
}

func dependencyRetirementChildResult(branch string, child *AgentRun) map[string]interface{} {
	return map[string]interface{}{"branchId": branch, "checkpoint": child.Checkpoint, "output": child.Output}
}

func reopenDependencyRetirementStore(t *testing.T, store dependencyRetirementStore) dependencyRetirementStore {
	t.Helper()
	switch original := store.(type) {
	case *MemoryStore:
		return original
	case *SQLiteStore:
		var sequence int
		var name, path string
		if err := original.db.QueryRowContext(t.Context(), `PRAGMA database_list`).Scan(&sequence, &name, &path); err != nil {
			t.Fatal(err)
		}
		if err := original.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := NewSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		return reopened
	case *PostgresStore:
		reopened, err := NewPostgresStore(t.Context(), os.Getenv("OPENSEAL_TEST_POSTGRES_DSN"), WithPostgresSchema(original.schema))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		return reopened
	default:
		t.Fatalf("unsupported dependency store %T", store)
		return nil
	}
}
