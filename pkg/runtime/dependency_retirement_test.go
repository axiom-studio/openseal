package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun/agentruntest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

func TestDependencyRetirementTerminalSourceDoesNotReviveOrCancelChildren(t *testing.T) {
	for _, status := range []models.AgentRunStatus{models.AgentRunStatusCanceled, models.AgentRunStatusFailed, models.AgentRunStatusCompleted} {
		t.Run(string(status), func(t *testing.T) {
			forDependencyRetirementBackends(t, func(t *testing.T, store agentruntest.DependencyStore, backend actiontest.Backend) {
				fixture := agentruntest.NewDependencyFixture(t, store)
				completed := fixture.FinishChild(t, "alpha", models.AgentRunStatusCompleted)
				fixture.ResolveChild(t, "alpha", completed)
				fixture.TerminateSource(t, status)
				fixture.AssertWaitingWork(t, 1, true)
				before := fixture.Snapshot(t)
				request := fixture.Request(t)
				result, err := fixture.Coordinator().ReconcileRunDependencyGroup(t.Context(), request)
				if err != nil || result == nil || result.Replayed {
					t.Fatalf("retire = %#v, %v", result, err)
				}
				if result.Group.Status != models.RunDependencyGroupCanceled || result.Group.ResolvedAt == nil || result.Group.WakeSignalID != "" || result.Evaluation.Wake {
					t.Fatalf("terminal source retirement manufactured a wake: %#v", result)
				}
				if err := result.Group.Validate(); err != nil {
					t.Fatalf("retired group is invalid: %v", err)
				}
				after := fixture.Snapshot(t)
				if !reflect.DeepEqual(before.Source, after.Source) || !reflect.DeepEqual(before.Children, after.Children) {
					t.Fatalf("retirement changed source or independently executing targets: before=%#v after=%#v", before, after)
				}
				if !reflect.DeepEqual(agentruntest.DependencyEdge(t, before.Edges, "alpha"), agentruntest.DependencyEdge(t, after.Edges, "alpha")) {
					t.Fatal("retirement replaced an already committed child outcome")
				}
				unresolved := agentruntest.DependencyEdge(t, after.Edges, "beta")
				if unresolved.State != models.RunDependencyStateCanceled || unresolved.ResolvedAt == nil || len(unresolved.Result) != 0 || len(unresolved.Artifacts) != 0 {
					t.Fatalf("unresolved edge was not retired without inventing a child result: %#v", unresolved)
				}
				fixture.AssertWaitingWork(t, 0)
				fixture.AssertReplayUnchanged(t, request, after)
				fixture.Store = reopenDependencyRetirementStore(t, backend)
				fixture.AssertReplayUnchanged(t, request, fixture.Snapshot(t))
				fixture.AssertWaitingWork(t, 0)
			})
		})
	}
}

func TestDependencyRetirementReadsOnlyExactDurableChildOutcome(t *testing.T) {
	for _, status := range []models.AgentRunStatus{models.AgentRunStatusCompleted, models.AgentRunStatusFailed, models.AgentRunStatusCanceled} {
		t.Run(string(status), func(t *testing.T) {
			forDependencyRetirementStores(t, func(t *testing.T, store agentruntest.DependencyStore) {
				fixture := agentruntest.NewDependencyFixture(t, store)
				child := fixture.FinishChild(t, "alpha", status)
				fixture.AssertWaitingWork(t, 1, true)
				before := fixture.Snapshot(t)
				result, err := fixture.Coordinator().ReconcileRunDependencyGroup(t.Context(), fixture.Request(t))
				if err != nil || result == nil || result.Replayed {
					t.Fatalf("reconcile child outcome = %#v, %v", result, err)
				}
				after := fixture.Snapshot(t)
				edge := agentruntest.DependencyEdge(t, after.Edges, "alpha")
				if edge.State != agentruntest.DependencyState(status) || !reflect.DeepEqual(edge.Result, agentruntest.DependencyChildResult(edge.ID, child)) || edge.Error != child.Error {
					t.Fatalf("edge does not match its authoritative child: edge=%#v child=%#v", edge, child)
				}
				if after.Group.Status != models.RunDependencyGroupWaiting || after.Source.Status != models.AgentRunStatusWaitingForDependency || result.Evaluation.Wake {
					t.Fatalf("partial all-branches join woke before the remaining target finished: %#v", result)
				}
				if !reflect.DeepEqual(before.Children, after.Children) || !reflect.DeepEqual(agentruntest.DependencyEdge(t, before.Edges, "beta"), agentruntest.DependencyEdge(t, after.Edges, "beta")) {
					t.Fatal("reading one terminal child changed another target, lease, or edge")
				}
				fixture.AssertReplayUnchanged(t, fixture.Request(t), after)
				fixture.AssertWaitingWork(t, 1, false)
				fixture.FinishChild(t, "beta", models.AgentRunStatusCompleted)
				request := fixture.Request(t)
				joined, err := fixture.Coordinator().ReconcileRunDependencyGroup(t.Context(), request)
				expected := models.RunDependencyGroupSatisfied
				if status != models.AgentRunStatusCompleted {
					expected = models.RunDependencyGroupFailed
				}
				if err != nil || joined == nil || joined.Group.Status != expected || joined.Source.Status != models.AgentRunStatusQueued || !joined.Evaluation.Wake {
					t.Fatalf("completed all-branches join = %#v, %v", joined, err)
				}
				fixture.AssertReplayUnchanged(t, request, fixture.Snapshot(t))
				fixture.AssertWaitingWork(t, 0)
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
			forDependencyRetirementStores(t, func(t *testing.T, store agentruntest.DependencyStore) {
				fixture := agentruntest.NewDependencyFixtureWithPolicy(t, store, models.Scope{Kind: "tenant", ID: uuid.NewString()}, testCase.policy)
				fixture.FinishChild(t, "alpha", testCase.firstStatus)
				if testCase.needsSecond {
					partial, err := fixture.Coordinator().ReconcileRunDependencyGroup(t.Context(), fixture.Request(t))
					if err != nil || partial == nil || partial.Group.Status != models.RunDependencyGroupWaiting || partial.Source.Status != models.AgentRunStatusWaitingForDependency || partial.Evaluation.Wake {
						t.Fatalf("quorum woke before its threshold: %#v, %v", partial, err)
					}
					fixture.FinishChild(t, "beta", models.AgentRunStatusCompleted)
				}
				before := fixture.Snapshot(t)
				request := fixture.Request(t)
				result, err := fixture.Coordinator().ReconcileRunDependencyGroup(t.Context(), request)
				if err != nil || result == nil || result.Replayed || result.Group.Status != testCase.expectedStatus || result.Source.Status != models.AgentRunStatusQueued || !result.Evaluation.Wake || result.Source.LastWakeSignalID != result.Group.WakeSignalID {
					t.Fatalf("policy fan-in did not commit one exact wake: %#v, %v", result, err)
				}
				after := fixture.Snapshot(t)
				if !reflect.DeepEqual(before.Children, after.Children) {
					t.Fatal("early fan-in changed independently progressing targets or their leases")
				}
				activeBranch := "beta"
				if testCase.needsSecond {
					activeBranch = "gamma"
				}
				if after.Children[activeBranch].Status != models.AgentRunStatusRunning || !reflect.DeepEqual(agentruntest.DependencyEdge(t, before.Edges, activeBranch), agentruntest.DependencyEdge(t, after.Edges, activeBranch)) {
					t.Fatalf("policy reconciliation changed a still-running target's edge: %#v", after)
				}
				fixture.AssertReplayUnchanged(t, request, after)
				fixture.AssertWaitingWork(t, 0)
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
			forDependencyRetirementStores(t, func(t *testing.T, store agentruntest.DependencyStore) {
				fixture := agentruntest.NewDependencyFixture(t, store)
				fixture.Command(t, agentrun.AgentRunCommandPause)
				fixture.AssertWaitingWork(t, 1, false)
				paused := fixture.Snapshot(t).Source
				for _, branch := range []string{"alpha", "beta"} {
					child := fixture.FinishChild(t, branch, models.AgentRunStatusCompleted)
					fixture.AssertWaitingWork(t, 1, true)
					var result *models.RunDependencyResult
					var err error
					if direct {
						result = fixture.ResolveChild(t, branch, child)
					} else {
						result, err = fixture.Coordinator().ReconcileRunDependencyGroup(t.Context(), fixture.Request(t))
					}
					if err != nil || result == nil || result.Source.Status != models.AgentRunStatusPaused || result.Evaluation.Wake {
						t.Fatalf("paused result resumed work: %#v, %v", result, err)
					}
					if branch == "alpha" && (result.Source.PausedFrom != models.AgentRunStatusWaitingForDependency || result.Source.PausedWakeCondition == nil) {
						t.Fatalf("partial paused join lost its remaining dependency: %#v", result.Source)
					}
					if branch == "alpha" {
						fixture.AssertWaitingWork(t, 1, false)
					}
				}
				joined := fixture.Snapshot(t)
				if joined.Group.Status != models.RunDependencyGroupSatisfied || joined.Source.PausedFrom != models.AgentRunStatusQueued || joined.Source.PausedWakeCondition != nil || joined.Source.WakeCondition != nil {
					t.Fatalf("completed paused join is not ready for normal resume: %#v", joined)
				}
				if !reflect.DeepEqual(paused.Checkpoint, joined.Source.Checkpoint) || !reflect.DeepEqual(paused.Budget, joined.Source.Budget) || joined.Source.BudgetUsage != paused.BudgetUsage {
					t.Fatal("dependency completion replaced unrelated paused state or spent the parent budget")
				}
				for _, edge := range joined.Edges {
					if !reflect.DeepEqual(edge.Result, agentruntest.DependencyChildResult(edge.ID, joined.Children[edge.ID])) {
						t.Fatalf("paused join lost child result: %#v", edge)
					}
				}
				request := fixture.Request(t)
				fixture.Command(t, agentrun.AgentRunCommandResume)
				resumed := fixture.Snapshot(t)
				if resumed.Source.Status != models.AgentRunStatusQueued || resumed.Source.WakeCondition != nil || resumed.Source.PausedWakeCondition != nil || !reflect.DeepEqual(resumed.Source.Output, joined.Source.Output) {
					t.Fatalf("normal resume restored a completed wait or lost output: %#v", resumed.Source)
				}
				fixture.AssertReplayUnchanged(t, request, resumed)
			})
		})
	}
}

func TestDependencyRetirementFencesGroupSourceAndTenantBeforeWrites(t *testing.T) {
	forDependencyRetirementStores(t, func(t *testing.T, store agentruntest.DependencyStore) {
		fixture := agentruntest.NewDependencyFixture(t, store)
		fixture.FinishChild(t, "alpha", models.AgentRunStatusCompleted)
		before := fixture.Snapshot(t)
		request := fixture.Request(t)
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
				if _, err := fixture.Coordinator().ReconcileRunDependencyGroup(t.Context(), invalid); !errors.Is(err, testCase.expected) {
					t.Fatalf("reconcile error = %v, want %v", err, testCase.expected)
				}
				fixture.AssertSnapshot(t, before)
			})
		}
		if result, err := fixture.Coordinator().ReconcileRunDependencyGroup(t.Context(), request); err != nil || result == nil || result.Replayed {
			t.Fatalf("valid reconciliation after rejected writes = %#v, %v", result, err)
		}
	})
}

func TestDependencyRetirementIdleJoinDoesNotWrite(t *testing.T) {
	forDependencyRetirementStores(t, func(t *testing.T, store agentruntest.DependencyStore) {
		fixture := agentruntest.NewDependencyFixture(t, store)
		before := fixture.Snapshot(t)
		fixture.AssertReplayUnchanged(t, fixture.Request(t), before)
		fixture.AssertWaitingWork(t, 1)
	})
}

func TestDependencyRetirementWaitingScanIsBoundedScopedAndUsesExactKeyset(t *testing.T) {
	forDependencyRetirementStores(t, func(t *testing.T, store agentruntest.DependencyStore) {
		scope := models.Scope{Kind: "tenant", ID: uuid.NewString()}
		fixtures := make([]*agentruntest.DependencyFixture, 0, 3)
		want := make([]models.RunDependencyGroupWork, 0, 3)
		for range 3 {
			fixture := agentruntest.NewDependencyFixtureInScope(t, store, scope)
			fixtures = append(fixtures, fixture)
			snapshot := fixture.Snapshot(t)
			want = append(want, models.RunDependencyGroupWork{Scope: scope, GroupID: fixture.GroupID, GroupRevision: snapshot.Group.Revision, SourceRunID: fixture.SourceID, SourceRevision: snapshot.Source.Revision})
		}
		foreign := agentruntest.NewDependencyFixture(t, store)
		foreignBefore := foreign.Snapshot(t)
		sort.Slice(want, func(i, j int) bool { return want[i].GroupID < want[j].GroupID })
		coordinator := fixtures[0].Coordinator()
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
		retired.Command(t, agentrun.AgentRunCommandCancel)
		if _, err := retired.Coordinator().ReconcileRunDependencyGroup(t.Context(), retired.Request(t)); err != nil {
			t.Fatal(err)
		}
		remaining, err := coordinator.ListWaitingRunDependencyGroups(t.Context(), scope, "", 100)
		if err != nil || len(remaining) != 2 {
			t.Fatalf("retired group remained in indexed waiting scan = %#v, %v", remaining, err)
		}
		for _, work := range remaining {
			if work.GroupID == retired.GroupID || work.Scope != scope {
				t.Fatalf("waiting scan returned retired or foreign work: %#v", work)
			}
		}
		foreign.AssertWaitingWork(t, 1)
		foreign.AssertSnapshot(t, foreignBefore)
	})
}

func TestDependencyRetirementConcurrentReconciliationCommitsOnce(t *testing.T) {
	for _, terminalSource := range []bool{false, true} {
		name := "children_completed"
		if terminalSource {
			name = "source_canceled"
		}
		t.Run(name, func(t *testing.T) {
			forDependencyRetirementStores(t, func(t *testing.T, store agentruntest.DependencyStore) {
				fixture := agentruntest.NewDependencyFixture(t, store)
				if terminalSource {
					fixture.TerminateSource(t, models.AgentRunStatusCanceled)
				} else {
					fixture.FinishChild(t, "alpha", models.AgentRunStatusCompleted)
					fixture.FinishChild(t, "beta", models.AgentRunStatusCompleted)
				}
				before := fixture.Snapshot(t)
				request := fixture.Request(t)
				start := make(chan struct{})
				results := make(chan *models.RunDependencyResult, 2)
				errs := make(chan error, 2)
				var workers sync.WaitGroup
				for range 2 {
					workers.Add(1)
					go func() {
						defer workers.Done()
						<-start
						result, err := fixture.Coordinator().ReconcileRunDependencyGroup(t.Context(), request)
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
				after := fixture.Snapshot(t)
				if !reflect.DeepEqual(before.Children, after.Children) {
					t.Fatal("concurrent reconciliation changed child execution records")
				}
				if terminalSource && !reflect.DeepEqual(before.Source, after.Source) {
					t.Fatal("concurrent retirement revived or rewrote terminal source")
				}
				if !terminalSource && (after.Source.Status != models.AgentRunStatusQueued || after.Source.LastWakeSignalID == "" || after.Source.LastWakeSignalID != after.Group.WakeSignalID) {
					t.Fatalf("fan-in lacks its one durable wake: %#v", after)
				}
				fixture.AssertReplayUnchanged(t, request, after)
				fixture.AssertWaitingWork(t, 0)
			})
		})
	}
}

type dependencyRetirementCancelBeforeStore struct {
	agentruntest.DependencyStore
	before func()
}

func (s *dependencyRetirementCancelBeforeStore) ReconcileRunDependencyGroup(ctx context.Context, record models.RunDependencyGroupReconciliationRecord) (*models.RunDependencyResult, error) {
	if s.before != nil {
		before := s.before
		s.before = nil
		before()
	}
	return s.DependencyStore.ReconcileRunDependencyGroup(ctx, record)
}

func TestDependencyRetirementSourceCancellationBetweenReadAndCommitIsFenced(t *testing.T) {
	forDependencyRetirementStores(t, func(t *testing.T, store agentruntest.DependencyStore) {
		fixture := agentruntest.NewDependencyFixture(t, store)
		fixture.FinishChild(t, "alpha", models.AgentRunStatusCompleted)
		request := fixture.Request(t)
		var canceled *models.AgentRun
		var before agentruntest.DependencySnapshot
		wrapped := &dependencyRetirementCancelBeforeStore{DependencyStore: store, before: func() {
			canceled = fixture.Command(t, agentrun.AgentRunCommandCancel)
			before = fixture.Snapshot(t)
		}}
		coordinator := portfolio.NewDependencyCoordinator(wrapped, models.WithClock(func() time.Time { return fixture.Now.Add(10 * time.Second) }))
		if _, err := coordinator.ReconcileRunDependencyGroup(t.Context(), request); !errors.Is(err, models.ErrRevisionConflict) {
			t.Fatalf("stale source accepted across cancellation: %v", err)
		}
		fixture.AssertSnapshot(t, before)
		result, err := fixture.Coordinator().ReconcileRunDependencyGroup(t.Context(), fixture.Request(t))
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
	forDependencyRetirementStores(t, func(t *testing.T, store agentruntest.DependencyStore) {
		fixture := agentruntest.NewDependencyFixture(t, store)
		child := fixture.FinishChild(t, "alpha", models.AgentRunStatusCompleted)
		fixture.Command(t, agentrun.AgentRunCommandCancel)
		before := fixture.Snapshot(t)
		result := fixture.ResolveChild(t, "alpha", child)
		after := fixture.Snapshot(t)
		if result.Group.Status != models.RunDependencyGroupCanceled || result.Group.WakeSignalID != "" || result.Evaluation.Wake || !reflect.DeepEqual(before.Source, after.Source) || !reflect.DeepEqual(before.Children, after.Children) {
			t.Fatalf("late direct resolution changed canceled work: %#v", result)
		}
		for _, edge := range after.Edges {
			if edge.State != models.RunDependencyStateCanceled || len(edge.Result) != 0 || len(edge.Artifacts) != 0 {
				t.Fatalf("late direct resolution stored caller-supplied results while retiring canceled work: %#v", edge)
			}
		}
		fixture.AssertWaitingWork(t, 0)
		fixture.AssertReplayUnchanged(t, fixture.Request(t), after)
	})
}

func TestDependencyRetirementHistoricalTypedSourceCleansUpAfterAuthorityChanges(t *testing.T) {
	for _, authority := range []string{"maintenance", "revoked_binding"} {
		t.Run(authority, func(t *testing.T) {
			forDependencyRetirementStores(t, func(t *testing.T, store agentruntest.DependencyStore) {
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
				fixture := &agentruntest.DependencyFixture{Store: store, Scope: scope, GroupID: group.Group.ID, SourceID: source.ID, ChildIDs: map[string]string{"branch": child.ID}, Now: now}
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
					revoked := cloneRetirementBinding(binding)
					revoked.Disabled, revoked.Revision, revoked.UpdatedAt = true, binding.Revision+1, now.Add(3*time.Second)
					if err := catalog.Bind(t.Context(), revoked); err != nil {
						t.Fatal(err)
					}
				}
				// Verify the fixture's new execution authority is really denied.
				if run, err := portfolioService.CreateAgentRun(t.Context(), runSkillDependencyRequest(scope)); run != nil || !errors.Is(err, expectedAdmissionError) {
					t.Fatalf("fresh typed work bypassed changed authority: %#v, %v", run, err)
				}
				before := fixture.Snapshot(t)
				if before.Source.Context[models.CapabilityInvocationContextKey] == nil || before.Children["branch"].Status != models.AgentRunStatusQueued {
					t.Fatal("fixture lost historical typed intent or active independent child")
				}
				edge := before.Edges[0]
				result, err := coordinator.ResolveRunDependency(t.Context(), portfolio.ResolveRunDependencyRequest{Scope: scope, GroupID: fixture.GroupID, DependencyID: edge.ID, ExpectedDependencyRevision: edge.Revision, State: models.RunDependencyStateSatisfied, Result: map[string]interface{}{"callerClaim": "must not become child truth"}, Actor: models.ActivityActor{Type: "worker", ID: "dependency-reconciler"}})
				if err != nil || result == nil || result.Group.Status != models.RunDependencyGroupCanceled || result.Group.WakeSignalID != "" || result.Evaluation.Wake {
					t.Fatalf("historical relationship cleanup required fresh executable authority: %#v, %v", result, err)
				}
				after := fixture.Snapshot(t)
				if !reflect.DeepEqual(before.Source, after.Source) || !reflect.DeepEqual(before.Children, after.Children) {
					t.Fatal("cleanup rewrote historical source identity or independently progressing child")
				}
				if after.Edges[0].State != models.RunDependencyStateCanceled || len(after.Edges[0].Result) != 0 || len(after.Edges[0].Artifacts) != 0 {
					t.Fatalf("cleanup stored untrusted completion intent: %#v", after.Edges[0])
				}
				fixture.AssertReplayUnchanged(t, fixture.Request(t), after)
				fixture.AssertWaitingWork(t, 0)
			})
		})
	}
}

func forDependencyRetirementStores(t *testing.T, test func(*testing.T, agentruntest.DependencyStore)) {
	t.Helper()
	forDependencyRetirementBackends(t, func(t *testing.T, store agentruntest.DependencyStore, _ actiontest.Backend) { test(t, store) })
}

func forDependencyRetirementBackends(t *testing.T, test func(*testing.T, agentruntest.DependencyStore, actiontest.Backend)) {
	t.Helper()
	actiontest.ForEachBackend(t, func(t *testing.T, kernel models.KernelStore, backend actiontest.Backend) {
		store, ok := kernel.(agentruntest.DependencyStore)
		if !ok {
			t.Fatalf("%T does not implement atomic dependency reconciliation", kernel)
		}
		test(t, store, backend)
	})
}

// reopenDependencyRetirementStore opens the store again on backend's data, as
// a restart would.
func reopenDependencyRetirementStore(t *testing.T, backend actiontest.Backend) agentruntest.DependencyStore {
	t.Helper()
	return backend.Reopen(t).(agentruntest.DependencyStore)
}

// cloneRetirementBinding deep-copies a Skill binding through JSON, as the
// skill reference upgrade does before it changes one.
func cloneRetirementBinding(value *skill.Binding) *skill.Binding {
	payload, _ := json.Marshal(value)
	var copy skill.Binding
	_ = json.Unmarshal(payload, &copy)
	return &copy
}
