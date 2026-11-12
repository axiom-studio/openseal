package runtime

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/google/uuid"
)

type dependencyRetirementStore interface {
	models.KernelStore
	models.DependencyKernelStore
	models.RunDependencyReconciliationStore
}

type dependencyRetirementFixture struct {
	store    dependencyRetirementStore
	scope    models.Scope
	groupID  string
	sourceID string
	childIDs map[string]string
	now      time.Time
}

type dependencyRetirementSnapshot struct {
	group    *models.RunDependencyGroup
	edges    []*models.RunDependency
	source   *models.AgentRun
	children map[string]*models.AgentRun
	events   []*models.ActivityEvent
}

func TestDependencyRetirementTerminalSourceDoesNotReviveOrCancelChildren(t *testing.T) {
	for _, status := range []models.AgentRunStatus{models.AgentRunStatusCanceled, models.AgentRunStatusFailed, models.AgentRunStatusCompleted} {
		t.Run(string(status), func(t *testing.T) {
			forDependencyRetirementBackends(t, func(t *testing.T, store dependencyRetirementStore, backend actionLifecycleBackend) {
				fixture := newDependencyRetirementFixture(t, store)
				completed := fixture.finishChild(t, "alpha", models.AgentRunStatusCompleted)
				fixture.resolveChild(t, "alpha", completed)
				fixture.terminateSource(t, status)
				fixture.assertWaitingWork(t, 1, true)
				before := fixture.snapshot(t)
				request := fixture.request(t)
				result, err := fixture.coordinator().ReconcileRunDependencyGroup(t.Context(), request)
				if err != nil || result == nil || result.Replayed {
					t.Fatalf("retire = %#v, %v", result, err)
				}
				if result.Group.Status != models.RunDependencyGroupCanceled || result.Group.ResolvedAt == nil || result.Group.WakeSignalID != "" || result.Evaluation.Wake {
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
				if unresolved.State != models.RunDependencyStateCanceled || unresolved.ResolvedAt == nil || len(unresolved.Result) != 0 || len(unresolved.Artifacts) != 0 {
					t.Fatalf("unresolved edge was not retired without inventing a child result: %#v", unresolved)
				}
				fixture.assertWaitingWork(t, 0)
				fixture.assertReplayUnchanged(t, request, after)
				fixture.store = reopenDependencyRetirementStore(t, backend)
				fixture.assertReplayUnchanged(t, request, fixture.snapshot(t))
				fixture.assertWaitingWork(t, 0)
			})
		})
	}
}

func TestDependencyRetirementReadsOnlyExactDurableChildOutcome(t *testing.T) {
	for _, status := range []models.AgentRunStatus{models.AgentRunStatusCompleted, models.AgentRunStatusFailed, models.AgentRunStatusCanceled} {
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
				if after.group.Status != models.RunDependencyGroupWaiting || after.source.Status != models.AgentRunStatusWaitingForDependency || result.Evaluation.Wake {
					t.Fatalf("partial all-branches join woke before the remaining target finished: %#v", result)
				}
				if !reflect.DeepEqual(before.children, after.children) || !reflect.DeepEqual(dependencyRetirementEdge(t, before.edges, "beta"), dependencyRetirementEdge(t, after.edges, "beta")) {
					t.Fatal("reading one terminal child changed another target, lease, or edge")
				}
				fixture.assertReplayUnchanged(t, fixture.request(t), after)
				fixture.assertWaitingWork(t, 1, false)
				fixture.finishChild(t, "beta", models.AgentRunStatusCompleted)
				request := fixture.request(t)
				joined, err := fixture.coordinator().ReconcileRunDependencyGroup(t.Context(), request)
				expected := models.RunDependencyGroupSatisfied
				if status != models.AgentRunStatusCompleted {
					expected = models.RunDependencyGroupFailed
				}
				if err != nil || joined == nil || joined.Group.Status != expected || joined.Source.Status != models.AgentRunStatusQueued || !joined.Evaluation.Wake {
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
		policy         models.RunDependencyPolicy
		firstStatus    models.AgentRunStatus
		expectedStatus models.RunDependencyGroupStatus
		needsSecond    bool
	}{
		{name: "any", policy: models.RunDependencyPolicy{Mode: models.FanInModeAny, FailureMode: models.DependencyFailureWait}, firstStatus: models.AgentRunStatusCompleted, expectedStatus: models.RunDependencyGroupSatisfied},
		{name: "quorum", policy: models.RunDependencyPolicy{Mode: models.FanInModeQuorum, Quorum: 2, FailureMode: models.DependencyFailureWait}, firstStatus: models.AgentRunStatusCompleted, expectedStatus: models.RunDependencyGroupSatisfied, needsSecond: true},
		{name: "fail_fast", policy: models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureFailFast}, firstStatus: models.AgentRunStatusFailed, expectedStatus: models.RunDependencyGroupFailed},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			forDependencyRetirementStores(t, func(t *testing.T, store dependencyRetirementStore) {
				fixture := newDependencyRetirementFixtureWithPolicy(t, store, models.Scope{Kind: "tenant", ID: uuid.NewString()}, testCase.policy)
				fixture.finishChild(t, "alpha", testCase.firstStatus)
				if testCase.needsSecond {
					partial, err := fixture.coordinator().ReconcileRunDependencyGroup(t.Context(), fixture.request(t))
					if err != nil || partial == nil || partial.Group.Status != models.RunDependencyGroupWaiting || partial.Source.Status != models.AgentRunStatusWaitingForDependency || partial.Evaluation.Wake {
						t.Fatalf("quorum woke before its threshold: %#v, %v", partial, err)
					}
					fixture.finishChild(t, "beta", models.AgentRunStatusCompleted)
				}
				before := fixture.snapshot(t)
				request := fixture.request(t)
				result, err := fixture.coordinator().ReconcileRunDependencyGroup(t.Context(), request)
				if err != nil || result == nil || result.Replayed || result.Group.Status != testCase.expectedStatus || result.Source.Status != models.AgentRunStatusQueued || !result.Evaluation.Wake || result.Source.LastWakeSignalID != result.Group.WakeSignalID {
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
				if after.children[activeBranch].Status != models.AgentRunStatusRunning || !reflect.DeepEqual(dependencyRetirementEdge(t, before.edges, activeBranch), dependencyRetirementEdge(t, after.edges, activeBranch)) {
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
				fixture.command(t, agentrun.AgentRunCommandPause)
				fixture.assertWaitingWork(t, 1, false)
				paused := fixture.snapshot(t).source
				for _, branch := range []string{"alpha", "beta"} {
					child := fixture.finishChild(t, branch, models.AgentRunStatusCompleted)
					fixture.assertWaitingWork(t, 1, true)
					var result *models.RunDependencyResult
					var err error
					if direct {
						result = fixture.resolveChild(t, branch, child)
					} else {
						result, err = fixture.coordinator().ReconcileRunDependencyGroup(t.Context(), fixture.request(t))
					}
					if err != nil || result == nil || result.Source.Status != models.AgentRunStatusPaused || result.Evaluation.Wake {
						t.Fatalf("paused result resumed work: %#v, %v", result, err)
					}
					if branch == "alpha" && (result.Source.PausedFrom != models.AgentRunStatusWaitingForDependency || result.Source.PausedWakeCondition == nil) {
						t.Fatalf("partial paused join lost its remaining dependency: %#v", result.Source)
					}
					if branch == "alpha" {
						fixture.assertWaitingWork(t, 1, false)
					}
				}
				joined := fixture.snapshot(t)
				if joined.group.Status != models.RunDependencyGroupSatisfied || joined.source.PausedFrom != models.AgentRunStatusQueued || joined.source.PausedWakeCondition != nil || joined.source.WakeCondition != nil {
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
				fixture.command(t, agentrun.AgentRunCommandResume)
				resumed := fixture.snapshot(t)
				if resumed.source.Status != models.AgentRunStatusQueued || resumed.source.WakeCondition != nil || resumed.source.PausedWakeCondition != nil || !reflect.DeepEqual(resumed.source.Output, joined.source.Output) {
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
		fixture.finishChild(t, "alpha", models.AgentRunStatusCompleted)
		before := fixture.snapshot(t)
		request := fixture.request(t)
		for _, testCase := range []struct {
			name     string
			mutate   func(*portfolio.ReconcileRunDependencyGroupRequest)
			expected error
		}{
			{name: "group_revision", mutate: func(r *portfolio.ReconcileRunDependencyGroupRequest) { r.ExpectedGroupRevision++ }, expected: models.ErrRevisionConflict},
			{name: "source_revision", mutate: func(r *portfolio.ReconcileRunDependencyGroupRequest) { r.ExpectedSourceRevision++ }, expected: models.ErrRevisionConflict},
			{name: "foreign_scope", mutate: func(r *portfolio.ReconcileRunDependencyGroupRequest) { r.Scope.ID = "foreign-tenant" }, expected: models.ErrDependencyGroupNotFound},
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
		scope := models.Scope{Kind: "tenant", ID: uuid.NewString()}
		fixtures := make([]*dependencyRetirementFixture, 0, 3)
		want := make([]models.RunDependencyGroupWork, 0, 3)
		for range 3 {
			fixture := newDependencyRetirementFixtureInScope(t, store, scope)
			fixtures = append(fixtures, fixture)
			snapshot := fixture.snapshot(t)
			want = append(want, models.RunDependencyGroupWork{Scope: scope, GroupID: fixture.groupID, GroupRevision: snapshot.group.Revision, SourceRunID: fixture.sourceID, SourceRevision: snapshot.source.Revision})
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
			if _, err := coordinator.ListWaitingRunDependencyGroups(t.Context(), scope, "", limit); !errors.Is(err, models.ErrInvalidRunDependency) {
				t.Fatalf("unbounded/invalid page %d was accepted: %v", limit, err)
			}
			if _, err := store.ListWaitingRunDependencyGroups(t.Context(), scope, "", limit); !errors.Is(err, models.ErrInvalidRunDependency) {
				t.Fatalf("direct store unbounded/invalid page %d was accepted: %v", limit, err)
			}
		}
		retired := fixtures[1]
		retired.command(t, agentrun.AgentRunCommandCancel)
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
					fixture.terminateSource(t, models.AgentRunStatusCanceled)
				} else {
					fixture.finishChild(t, "alpha", models.AgentRunStatusCompleted)
					fixture.finishChild(t, "beta", models.AgentRunStatusCompleted)
				}
				before := fixture.snapshot(t)
				request := fixture.request(t)
				start := make(chan struct{})
				results := make(chan *models.RunDependencyResult, 2)
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
					if err != nil && !errors.Is(err, models.ErrRevisionConflict) {
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
				if !terminalSource && (after.source.Status != models.AgentRunStatusQueued || after.source.LastWakeSignalID == "" || after.source.LastWakeSignalID != after.group.WakeSignalID) {
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

func (s *dependencyRetirementCancelBeforeStore) ReconcileRunDependencyGroup(ctx context.Context, record models.RunDependencyGroupReconciliationRecord) (*models.RunDependencyResult, error) {
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
		fixture.finishChild(t, "alpha", models.AgentRunStatusCompleted)
		request := fixture.request(t)
		var canceled *models.AgentRun
		var before dependencyRetirementSnapshot
		wrapped := &dependencyRetirementCancelBeforeStore{dependencyRetirementStore: store, before: func() {
			canceled = fixture.command(t, agentrun.AgentRunCommandCancel)
			before = fixture.snapshot(t)
		}}
		coordinator := portfolio.NewDependencyCoordinator(wrapped, models.WithClock(func() time.Time { return fixture.now.Add(10 * time.Second) }))
		if _, err := coordinator.ReconcileRunDependencyGroup(t.Context(), request); !errors.Is(err, models.ErrRevisionConflict) {
			t.Fatalf("stale source accepted across cancellation: %v", err)
		}
		fixture.assertSnapshot(t, before)
		result, err := fixture.coordinator().ReconcileRunDependencyGroup(t.Context(), fixture.request(t))
		if err != nil || result == nil || result.Group.Status != models.RunDependencyGroupCanceled || result.Group.WakeSignalID != "" || !reflect.DeepEqual(canceled, result.Source) {
			t.Fatalf("retry did not retire the canceled source: %#v, %v", result, err)
		}
		for _, edge := range result.Dependencies {
			if edge.State != models.RunDependencyStateCanceled || len(edge.Result) != 0 {
				t.Fatalf("retirement forged a canceled branch outcome from a completed child: %#v", edge)
			}
		}
	})
}

func TestDependencyRetirementDirectResolutionCannotReviveCanceledSource(t *testing.T) {
	forDependencyRetirementStores(t, func(t *testing.T, store dependencyRetirementStore) {
		fixture := newDependencyRetirementFixture(t, store)
		child := fixture.finishChild(t, "alpha", models.AgentRunStatusCompleted)
		fixture.command(t, agentrun.AgentRunCommandCancel)
		before := fixture.snapshot(t)
		result := fixture.resolveChild(t, "alpha", child)
		after := fixture.snapshot(t)
		if result.Group.Status != models.RunDependencyGroupCanceled || result.Group.WakeSignalID != "" || result.Evaluation.Wake || !reflect.DeepEqual(before.source, after.source) || !reflect.DeepEqual(before.children, after.children) {
			t.Fatalf("late direct resolution changed canceled work: %#v", result)
		}
		for _, edge := range after.edges {
			if edge.State != models.RunDependencyStateCanceled || len(edge.Result) != 0 || len(edge.Artifacts) != 0 {
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
				portfolioService := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
				source, err := portfolioService.CreateAgentRun(t.Context(), runSkillDependencyRequest(scope))
				if err != nil {
					t.Fatal(err)
				}
				child, err := portfolioService.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{Scope: scope, ParentRunID: source.ID, Owner: source.Owner, AssignedAgentID: "helper", Goal: "Independent branch", Source: models.RunSourceRequest, Checkpoint: map[string]interface{}{"committed": "child work"}})
				if err != nil {
					t.Fatal(err)
				}
				coordinator := portfolio.NewDependencyCoordinator(store, models.WithClock(func() time.Time { return now.Add(time.Second) }))
				group, err := coordinator.CreateRunDependencyGroup(t.Context(), models.CreateRunDependencyGroupRequest{Scope: scope, SourceRunID: source.ID, ExpectedSourceRevision: source.Revision, Policy: models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureWait}, Dependencies: []models.RunDependencySpec{{ID: "branch", TargetRunID: child.ID, Kind: models.RunDependencyKindRun}}, Actor: models.ActivityActor{Type: "worker", ID: "typed-worker"}})
				if err != nil {
					t.Fatal(err)
				}
				fixture := &dependencyRetirementFixture{store: store, scope: scope, groupID: group.Group.ID, sourceID: source.ID, childIDs: map[string]string{"branch": child.ID}, now: now}
				// The source ends before mutable executable authority is replaced;
				// this is cleanup of history, not admission of new execution.
				runActivity := activity.NewRunActivityService(store, store, models.WithClock(func() time.Time { return now.Add(2 * time.Second) }))
				if _, _, err := runActivity.TransitionRun(t.Context(), scope, source.ID, activity.RunTransitionRequest{ExpectedRevision: group.Source.Revision, Status: models.AgentRunStatusCanceled, Actor: models.ActivityActor{Type: "user", ID: "operator"}}); err != nil {
					t.Fatal(err)
				}
				expectedAdmissionError := models.ErrRunSkillDependencyUnavailable
				if authority == "maintenance" {
					request := runSkillDependencyMaintenanceRequest(scope)
					request.Now = now.Add(3 * time.Second)
					gate, err := store.(models.SkillRuntimeMaintenanceStore).AcquireSkillRuntimeMaintenance(t.Context(), request)
					if err != nil || gate == nil || !gate.Active {
						t.Fatalf("maintenance after terminal source = %#v, %v", gate, err)
					}
					expectedAdmissionError = models.ErrSkillRuntimeMaintenance
				} else {
					revoked := cloneUpgradeBinding(binding)
					revoked.Disabled, revoked.Revision, revoked.UpdatedAt = true, binding.Revision+1, now.Add(3*time.Second)
					if err := catalog.Bind(t.Context(), revoked); err != nil {
						t.Fatal(err)
					}
				}
				// Verify the fixture's new execution authority is really denied.
				if run, err := portfolioService.CreateAgentRun(t.Context(), runSkillDependencyRequest(scope)); run != nil || !errors.Is(err, expectedAdmissionError) {
					t.Fatalf("fresh typed work bypassed changed authority: %#v, %v", run, err)
				}
				before := fixture.snapshot(t)
				if before.source.Context[models.CapabilityInvocationContextKey] == nil || before.children["branch"].Status != models.AgentRunStatusQueued {
					t.Fatal("fixture lost historical typed intent or active independent child")
				}
				edge := before.edges[0]
				result, err := coordinator.ResolveRunDependency(t.Context(), portfolio.ResolveRunDependencyRequest{Scope: scope, GroupID: fixture.groupID, DependencyID: edge.ID, ExpectedDependencyRevision: edge.Revision, State: models.RunDependencyStateSatisfied, Result: map[string]interface{}{"callerClaim": "must not become child truth"}, Actor: models.ActivityActor{Type: "worker", ID: "dependency-reconciler"}})
				if err != nil || result == nil || result.Group.Status != models.RunDependencyGroupCanceled || result.Group.WakeSignalID != "" || result.Evaluation.Wake {
					t.Fatalf("historical relationship cleanup required fresh executable authority: %#v, %v", result, err)
				}
				after := fixture.snapshot(t)
				if !reflect.DeepEqual(before.source, after.source) || !reflect.DeepEqual(before.children, after.children) {
					t.Fatal("cleanup rewrote historical source identity or independently progressing child")
				}
				if after.edges[0].State != models.RunDependencyStateCanceled || len(after.edges[0].Result) != 0 || len(after.edges[0].Artifacts) != 0 {
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
	forDependencyRetirementBackends(t, func(t *testing.T, store dependencyRetirementStore, _ actionLifecycleBackend) { test(t, store) })
}

func forDependencyRetirementBackends(t *testing.T, test func(*testing.T, dependencyRetirementStore, actionLifecycleBackend)) {
	t.Helper()
	forActionLifecycleBackends(t, func(t *testing.T, kernel models.KernelStore, backend actionLifecycleBackend) {
		store, ok := kernel.(dependencyRetirementStore)
		if !ok {
			t.Fatalf("%T does not implement atomic dependency reconciliation", kernel)
		}
		test(t, store, backend)
	})
}

func newDependencyRetirementFixture(t *testing.T, store dependencyRetirementStore) *dependencyRetirementFixture {
	t.Helper()
	return newDependencyRetirementFixtureInScope(t, store, models.Scope{Kind: "tenant", ID: uuid.NewString()})
}

func newDependencyRetirementFixtureInScope(t *testing.T, store dependencyRetirementStore, scope models.Scope) *dependencyRetirementFixture {
	t.Helper()
	return newDependencyRetirementFixtureWithPolicy(t, store, scope, models.RunDependencyPolicy{Mode: models.FanInModeAll, FailureMode: models.DependencyFailureWait})
}

func newDependencyRetirementFixtureWithPolicy(t *testing.T, store dependencyRetirementStore, scope models.Scope, policy models.RunDependencyPolicy) *dependencyRetirementFixture {
	t.Helper()
	fixture := &dependencyRetirementFixture{store: store, scope: scope, childIDs: make(map[string]string), now: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	portfolioService := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return fixture.now }))
	source, err := portfolioService.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{Scope: fixture.scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "lead"}, AssignedAgentID: "lead", Goal: "Join two independently running branches", Source: models.RunSourceManual, Checkpoint: map[string]interface{}{"committed": "preserve"}, Budget: &models.BudgetPolicy{MaxTurns: 10}})
	if err != nil {
		t.Fatal(err)
	}
	fixture.sourceID = source.ID
	schedulerClock := func() time.Time { return fixture.now.Add(time.Second) }
	scheduler := agentrun.NewAgentRunScheduler(store, models.WithClock(func() time.Time { return schedulerClock() }))
	claimed, err := scheduler.ClaimNext(t.Context(), agentrun.AgentRunClaimRequest{Scope: fixture.scope, WorkerID: "source-worker", LeaseDuration: time.Minute})
	if err != nil || claimed == nil || claimed.ID != source.ID {
		t.Fatalf("claim source=%#v, %v", claimed, err)
	}
	forks := agentrun.NewRunForkCoordinator(store, models.WithClock(func() time.Time { return fixture.now.Add(2 * time.Second) }))
	branches := []agentrun.RunForkBranch{
		{ID: "alpha", Goal: "Branch alpha", Checkpoint: map[string]interface{}{"branch": "alpha"}, Budget: &models.BudgetPolicy{MaxTurns: 2}},
		{ID: "beta", Goal: "Branch beta", Checkpoint: map[string]interface{}{"branch": "beta"}, Budget: &models.BudgetPolicy{MaxTurns: 2}},
	}
	if policy.Mode == models.FanInModeQuorum {
		branches = append(branches, agentrun.RunForkBranch{ID: "gamma", Goal: "Branch gamma", Checkpoint: map[string]interface{}{"branch": "gamma"}, Budget: &models.BudgetPolicy{MaxTurns: 2}})
	}
	var group *models.RunDependencyResult
	if policy.Mode == models.FanInModeQuorum {
		// Forks expose all/any today. The general dependency coordinator owns
		// quorum groups and accepts normally created independent target Runs.
		dependencies := make([]models.RunDependencySpec, 0, len(branches))
		for _, branch := range branches {
			child, err := portfolioService.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
				Scope: fixture.scope, ParentRunID: source.ID, Owner: source.Owner, AssignedAgentID: source.AssignedAgentID,
				Goal: branch.Goal, Source: models.RunSourceRequest, Checkpoint: branch.Checkpoint, Budget: branch.Budget,
			})
			if err != nil {
				t.Fatal(err)
			}
			dependencies = append(dependencies, models.RunDependencySpec{ID: branch.ID, TargetRunID: child.ID, Kind: models.RunDependencyKindRun})
		}
		coordinator := portfolio.NewDependencyCoordinator(store, models.WithClock(func() time.Time { return fixture.now.Add(2 * time.Second) }))
		group, err = coordinator.CreateRunDependencyGroup(t.Context(), models.CreateRunDependencyGroupRequest{
			Scope: fixture.scope, SourceRunID: source.ID, ExpectedSourceRevision: claimed.Revision,
			Policy: policy, Dependencies: dependencies, Actor: models.ActivityActor{Type: "worker", ID: "source-worker"},
		})
	} else {
		var created *agentrun.RunForkResult
		created, err = forks.Create(t.Context(), agentrun.CreateRunForkRequest{
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
	schedulerClock = func() time.Time { return fixture.now.Add(3 * time.Second) }
	for range len(branches) {
		child, err := scheduler.ClaimNext(t.Context(), agentrun.AgentRunClaimRequest{Scope: fixture.scope, WorkerID: "child-worker", LeaseDuration: time.Minute})
		if err != nil || child == nil || child.ParentRunID != source.ID {
			t.Fatalf("claim child=%#v, %v", child, err)
		}
	}
	return fixture
}

func (f *dependencyRetirementFixture) coordinator() *portfolio.DependencyCoordinator {
	coordinator := portfolio.NewDependencyCoordinator(f.store, models.WithClock(func() time.Time { return f.now.Add(10 * time.Second) }))
	return coordinator
}

func (f *dependencyRetirementFixture) request(t *testing.T) portfolio.ReconcileRunDependencyGroupRequest {
	t.Helper()
	snapshot := f.snapshot(t)
	return portfolio.ReconcileRunDependencyGroupRequest{Scope: f.scope, GroupID: f.groupID, ExpectedGroupRevision: snapshot.group.Revision, ExpectedSourceRevision: snapshot.source.Revision, Actor: models.ActivityActor{Type: "worker", ID: "dependency-reconciler"}, Visibility: models.ActivityVisibilityScope}
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
	children := make(map[string]*models.AgentRun, len(f.childIDs))
	for branch, id := range f.childIDs {
		child, err := f.store.GetAgentRun(t.Context(), f.scope, id)
		if err != nil || child == nil {
			t.Fatalf("get child=%#v, %v", child, err)
		}
		children[branch] = child
	}
	events, err := f.store.ListActivity(t.Context(), models.ActivityFilter{Scope: f.scope, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
	return dependencyRetirementSnapshot{group: group, edges: edges, source: source, children: children, events: events}
}

func (f *dependencyRetirementFixture) finishChild(t *testing.T, branch string, status models.AgentRunStatus) *models.AgentRun {
	t.Helper()
	child, err := f.store.GetAgentRun(t.Context(), f.scope, f.childIDs[branch])
	if err != nil || child == nil {
		t.Fatalf("get child=%#v, %v", child, err)
	}
	service := activity.NewRunActivityService(f.store, f.store, models.WithClock(func() time.Time { return f.now.Add(4 * time.Second) }))
	errorText := ""
	if status != models.AgentRunStatusCompleted {
		errorText = "durable " + string(status) + " child outcome"
	}
	_, _, err = service.TransitionRun(t.Context(), f.scope, child.ID, activity.RunTransitionRequest{ExpectedRevision: child.Revision, Status: status, Actor: models.ActivityActor{Type: "worker", ID: "child-worker"}, LeaseOwner: "child-worker", Checkpoint: map[string]interface{}{"branch": branch, "committed": "terminal"}, Output: map[string]interface{}{"message": "actual " + branch + " result", "nested": map[string]interface{}{"verified": true}}, Error: errorText, BudgetUsageDelta: &models.BudgetUsage{Turns: 1}})
	if err != nil {
		t.Fatal(err)
	}
	child, err = f.store.GetAgentRun(t.Context(), f.scope, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	return child
}

func (f *dependencyRetirementFixture) resolveChild(t *testing.T, branch string, child *models.AgentRun) *models.RunDependencyResult {
	t.Helper()
	edge := dependencyRetirementEdge(t, f.snapshot(t).edges, branch)
	result, err := f.coordinator().ResolveRunDependency(t.Context(), portfolio.ResolveRunDependencyRequest{Scope: f.scope, GroupID: f.groupID, DependencyID: edge.ID, ExpectedDependencyRevision: edge.Revision, State: dependencyRetirementState(child.Status), Result: dependencyRetirementChildResult(edge.ID, child), Error: child.Error, Actor: models.ActivityActor{Type: "worker", ID: "dependency-reconciler"}})
	if err != nil || result == nil {
		t.Fatalf("resolve child=%#v, %v", result, err)
	}
	return result
}

func (f *dependencyRetirementFixture) command(t *testing.T, kind agentrun.AgentRunCommandKind) *models.AgentRun {
	t.Helper()
	source, err := f.store.GetAgentRun(t.Context(), f.scope, f.sourceID)
	if err != nil {
		t.Fatal(err)
	}
	commands := agentrun.NewRunCommandService(f.store, models.WithClock(func() time.Time { return f.now.Add(5 * time.Second) }))
	result, err := commands.CommandAgentRun(t.Context(), agentrun.AgentRunCommandRequest{Scope: f.scope, RunID: f.sourceID, ExpectedRevision: source.Revision, Kind: kind, Actor: models.ActivityActor{Type: "user", ID: "operator"}})
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

func (f *dependencyRetirementFixture) terminateSource(t *testing.T, status models.AgentRunStatus) {
	t.Helper()
	if status == models.AgentRunStatusCanceled {
		f.command(t, agentrun.AgentRunCommandCancel)
		return
	}
	service := activity.NewRunActivityService(f.store, f.store, models.WithClock(func() time.Time { return f.now.Add(5 * time.Second) }))
	source := f.snapshot(t).source
	if status == models.AgentRunStatusCompleted {
		var err error
		source, _, err = service.TransitionRun(t.Context(), f.scope, source.ID, activity.RunTransitionRequest{ExpectedRevision: source.Revision, Status: models.AgentRunStatusRunning, Actor: models.ActivityActor{Type: "user", ID: "operator"}})
		if err != nil {
			t.Fatal(err)
		}
	}
	errorText := ""
	if status == models.AgentRunStatusFailed {
		errorText = "parent execution failed"
	}
	if _, _, err := service.TransitionRun(t.Context(), f.scope, source.ID, activity.RunTransitionRequest{ExpectedRevision: source.Revision, Status: status, Actor: models.ActivityActor{Type: "user", ID: "operator"}, Error: errorText}); err != nil {
		t.Fatal(err)
	}
}

func (f *dependencyRetirementFixture) assertSnapshot(t *testing.T, want dependencyRetirementSnapshot) {
	t.Helper()
	if got := f.snapshot(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("reconciliation mutated durable facts on rejection or replay:\nwant=%#v\ngot=%#v", want, got)
	}
}

func (f *dependencyRetirementFixture) assertReplayUnchanged(t *testing.T, request portfolio.ReconcileRunDependencyGroupRequest, want dependencyRetirementSnapshot) {
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

func dependencyRetirementEdge(t *testing.T, edges []*models.RunDependency, id string) *models.RunDependency {
	t.Helper()
	for _, edge := range edges {
		if edge.ID == id {
			return edge
		}
	}
	t.Fatalf("dependency %s not found in %#v", id, edges)
	return nil
}

func dependencyRetirementState(status models.AgentRunStatus) models.RunDependencyState {
	switch status {
	case models.AgentRunStatusCompleted:
		return models.RunDependencyStateSatisfied
	case models.AgentRunStatusFailed:
		return models.RunDependencyStateFailed
	default:
		return models.RunDependencyStateCanceled
	}
}

func dependencyRetirementChildResult(branch string, child *models.AgentRun) map[string]interface{} {
	return map[string]interface{}{"branchId": branch, "checkpoint": child.Checkpoint, "output": child.Output}
}

// reopenDependencyRetirementStore opens the store again on backend's data, as
// a restart would.
func reopenDependencyRetirementStore(t *testing.T, backend actionLifecycleBackend) dependencyRetirementStore {
	t.Helper()
	return backend.reopen(t).(dependencyRetirementStore)
}
