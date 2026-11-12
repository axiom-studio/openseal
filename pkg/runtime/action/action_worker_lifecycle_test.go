package action_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

func TestActionWorkerLifecyclePreservesCompletedDispatchAcrossRunChanges(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		status models.AgentRunStatus
		fail   bool
	}{
		{name: "paused_success", status: models.AgentRunStatusPaused},
		{name: "canceled_success", status: models.AgentRunStatusCanceled},
		{name: "paused_failure", status: models.AgentRunStatusPaused, fail: true},
		{name: "canceled_failure", status: models.AgentRunStatusCanceled, fail: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			actiontest.ForEachStore(t, func(t *testing.T, store models.KernelStore) {
				f := newLoopWorkerFixture(t, store, &models.BudgetPolicy{MaxActions: 10})
				call := f.propose("deploy-1", "deploy")
				var changed *models.AgentRun
				worker := f.worker(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
					f.setRunStatus(testCase.status)
					changed = f.run
					if testCase.fail {
						return nil, errors.New("provider rejected operation")
					}
					return map[string]interface{}{"providerResult": "delivered"}, nil
				})
				result, err := f.execute(worker, call.ID)
				if err != nil {
					t.Fatal(err)
				}
				expected := models.ActionCallStatusSucceeded
				if testCase.fail {
					expected = models.ActionCallStatusFailed
				}
				if result.Call.Status != expected || result.Call.CompletedAt == nil || result.Call.LeaseOwner != "" || f.dispatches != 1 {
					t.Fatalf("completed dispatch was not preserved: %#v dispatches=%d", result.Call, f.dispatches)
				}
				if testCase.fail && result.Call.FailurePhase != models.ActionFailureAfterDispatch || !testCase.fail && result.Call.Output["providerResult"] != "delivered" {
					t.Fatalf("completed dispatch lost its result: %#v", result.Call)
				}
				after := f.currentRun()
				if after.Status != testCase.status || !reflect.DeepEqual(after.Checkpoint, changed.Checkpoint) || after.WakeCondition != nil ||
					after.BudgetUsage.Actions != 1 || len(after.BudgetReservations) != 0 {
					t.Fatalf("completed dispatch changed its Run beyond the budget: before=%#v after=%#v", changed, after)
				}
				assertActionLifecycleTerminalEvents(t, store, call, expected, 1)
				actiontest.AssertUnclaimable(t, store, call, f.clock.Add(time.Hour))
			})
		})
	}
}

func TestActionWorkerLifecycleRetriesPersistenceWithoutRedispatch(t *testing.T) {
	for _, runMutation := range []string{"heartbeat", "cancel"} {
		t.Run(runMutation, func(t *testing.T) {
			actiontest.ForEachStore(t, func(t *testing.T, store models.KernelStore) {
				f := newLoopWorkerFixture(t, store, &models.BudgetPolicy{MaxActions: 10})
				call := f.propose("deploy-1", "deploy")
				wrapped := &actionLifecycleConflictStore{KernelStore: store}
				wrapped.beforeFirstPersist = func() {
					if runMutation == "cancel" {
						f.setRunStatus(models.AgentRunStatusCanceled)
						return
					}
					if _, err := store.RenewAgentRunLease(t.Context(), f.scope, f.run.ID, loopWorkerID, f.clock, time.Hour); err != nil {
						t.Fatal(err)
					}
				}
				f.store = wrapped
				result, err := f.execute(f.worker(succeedDispatch), call.ID)
				if err != nil || result == nil || result.Call.Status != models.ActionCallStatusSucceeded || result.Call.Output["providerResult"] != "delivered" {
					t.Fatalf("completion did not survive revision conflict: %#v, %v", result, err)
				}
				if f.dispatches != 1 || wrapped.persistCalls != 2 || !wrapped.sawRevisionConflict {
					t.Fatalf("persistence conflict redispatched or was not retried: dispatches=%d persists=%d conflict=%t", f.dispatches, wrapped.persistCalls, wrapped.sawRevisionConflict)
				}
				after := f.currentRun()
				expected := models.AgentRunStatusRunning
				if runMutation == "cancel" {
					expected = models.AgentRunStatusCanceled
				}
				if after.Status != expected || after.BudgetUsage.Actions != 1 || len(after.BudgetReservations) != 0 {
					t.Fatalf("completion after a conflict changed its Run: %#v", after)
				}
				assertActionLifecycleTerminalEvents(t, store, call, models.ActionCallStatusSucceeded, 1)
				actiontest.AssertUnclaimable(t, store, call, f.clock.Add(time.Hour))
			})
		})
	}
}

func TestActionWorkerLifecycleCallOnlyCompletionFencesParentRevision(t *testing.T) {
	actiontest.ForEachStore(t, func(t *testing.T, store models.KernelStore) {
		f := newLoopWorkerFixture(t, store, nil)
		proposed := f.propose("deploy-1", "deploy")
		call, err := store.ClaimNextAction(t.Context(), models.ActionClaim{Scope: f.scope, WorkerID: loopWorkerID, Now: f.clock, LeaseDuration: time.Minute, OwnActionID: proposed.ID})
		if err != nil || call == nil {
			t.Fatalf("claim = %#v, %v", call, err)
		}
		parent := f.currentRun()
		baselineEvents, err := store.ListActivity(t.Context(), models.ActivityFilter{Scope: call.Scope, RunID: call.RunID})
		if err != nil {
			t.Fatal(err)
		}
		completed := models.CloneActionCall(call)
		completed.Revision++
		completed.Status = models.ActionCallStatusSucceeded
		completed.Output = map[string]interface{}{"providerResult": "delivered"}
		completed.LeaseOwner = ""
		completed.LeaseExpiresAt = nil
		completionTime := f.clock.Add(time.Second)
		completed.UpdatedAt = completionTime
		completed.CompletedAt = &completionTime
		event := actiontest.CallEvent(completed, "call-only-completion", "action.succeeded", completionTime)
		record := models.ActionExecutionRecord{
			Call: completed, ExpectedCallRevision: call.Revision, ExpectedRunRevision: parent.Revision + 1,
			WorkerID: loopWorkerID, Now: completionTime, Event: event,
		}
		if _, err := store.PersistActionExecution(t.Context(), record); !errors.Is(err, models.ErrRevisionConflict) {
			t.Fatalf("call-only completion did not fence stale parent revision: %v", err)
		}
		record.ExpectedRunRevision = 0
		if _, err := store.PersistActionExecution(t.Context(), record); err == nil {
			t.Fatal("call-only completion accepted missing parent revision")
		}
		other, err := portfolio.NewPortfolioService(store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{
			Scope: call.Scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "other-agent"},
			AssignedAgentID: "other-agent", Goal: "Independent workstream", Source: models.RunSourceManual,
		})
		if err != nil {
			t.Fatal(err)
		}
		other, err = store.GetAgentRun(t.Context(), other.Scope, other.ID)
		if err != nil || other == nil {
			t.Fatalf("snapshot independent Run: %#v, %v", other, err)
		}
		forged := record
		forged.Call = models.CloneActionCall(completed)
		forged.Call.RunID = other.ID
		forged.Event = models.CloneActivityEvent(event)
		forged.Event.RunID = other.ID
		forged.ExpectedRunRevision = other.Revision
		if _, err := store.PersistActionExecution(t.Context(), forged); !errors.Is(err, models.ErrInvalidScope) {
			t.Fatalf("call-only completion accepted another Run's identity and revision: %v", err)
		}
		otherAfter, err := store.GetAgentRun(t.Context(), other.Scope, other.ID)
		if err != nil || !reflect.DeepEqual(other, otherAfter) {
			t.Fatalf("forged completion changed another Run: %#v, %v", otherAfter, err)
		}
		otherEvents, err := store.ListActivity(t.Context(), models.ActivityFilter{Scope: other.Scope, RunID: other.ID})
		if err != nil || len(otherEvents) != 0 {
			t.Fatalf("forged completion appended another Run's activity: %#v, %v", otherEvents, err)
		}
		persisted, err := store.GetActionCall(t.Context(), call.Scope, call.ID)
		if err != nil || persisted.Status != models.ActionCallStatusRunning || persisted.Revision != call.Revision {
			t.Fatalf("rejected completion changed the call: %#v, %v", persisted, err)
		}
		events, err := store.ListActivity(t.Context(), models.ActivityFilter{Scope: call.Scope, RunID: call.RunID})
		if err != nil || !reflect.DeepEqual(events, baselineEvents) {
			t.Fatalf("rejected completion appended activity: %#v, %v", events, err)
		}
		record.ExpectedRunRevision = parent.Revision
		result, err := store.PersistActionExecution(t.Context(), record)
		if err != nil || result == nil || result.Call.Status != models.ActionCallStatusSucceeded {
			t.Fatalf("fenced call-only completion failed: %#v, %v", result, err)
		}
		if after := f.currentRun(); !reflect.DeepEqual(parent, after) {
			t.Fatalf("call-only completion altered the parent: before=%#v after=%#v", parent, after)
		}
		events, err = store.ListActivity(t.Context(), models.ActivityFilter{Scope: call.Scope, RunID: call.RunID})
		if err != nil || len(events) != len(baselineEvents)+1 || events[len(events)-1].ID != event.ID {
			t.Fatalf("successful completion did not append exactly one activity: %#v, %v", events, err)
		}
	})
}

func TestActionWorkerLifecycleBoundsPersistenceConflicts(t *testing.T) {
	actiontest.ForEachStore(t, func(t *testing.T, store models.KernelStore) {
		f := newLoopWorkerFixture(t, store, nil)
		call := f.propose("deploy-1", "deploy")
		before := f.currentRun()
		wrapped := &actionLifecycleAlwaysConflictStore{KernelStore: store}
		f.store = wrapped
		result, err := f.execute(f.worker(succeedDispatch), call.ID)
		if result != nil || !errors.Is(err, models.ErrRevisionConflict) || f.dispatches != 1 || wrapped.persistCalls != 8 {
			t.Fatalf("persistence conflict retries were unbounded or repeated dispatch: %#v, %v, dispatches=%d persists=%d", result, err, f.dispatches, wrapped.persistCalls)
		}
		if after := f.currentRun(); !reflect.DeepEqual(before, after) {
			t.Fatalf("rejected completion changed the Run: before=%#v after=%#v", before, after)
		}
	})
}

func assertActionLifecycleTerminalEvents(t *testing.T, store models.KernelStore, call *models.ActionCall, status models.ActionCallStatus, actions int64) {
	t.Helper()
	events, err := store.ListActivity(t.Context(), models.ActivityFilter{Scope: call.Scope, RunID: call.RunID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	terminal := 0
	for _, event := range events {
		if event.Payload["actionCallId"] != call.ID || fmt.Sprint(event.Payload["status"]) != string(status) {
			continue
		}
		terminal++
		if actions > 0 && (event.UsageDelta == nil || event.UsageDelta.Actions != actions) || actions == 0 && event.UsageDelta != nil && event.UsageDelta.Actions != 0 {
			t.Fatalf("terminal activity charged incorrect usage: %#v", event)
		}
	}
	if terminal != 1 {
		t.Fatalf("expected one durable terminal activity, found %d: %#v", terminal, events)
	}
}

type actionLifecycleConflictStore struct {
	models.KernelStore
	beforeFirstPersist  func()
	persistCalls        int
	sawRevisionConflict bool
}

type actionLifecycleAlwaysConflictStore struct {
	models.KernelStore
	persistCalls int
}

func (s *actionLifecycleAlwaysConflictStore) PersistActionExecution(context.Context, models.ActionExecutionRecord) (*models.ActionExecutionResult, error) {
	s.persistCalls++
	return nil, models.ErrRevisionConflict
}

func (s *actionLifecycleConflictStore) PersistActionExecution(ctx context.Context, execution models.ActionExecutionRecord) (*models.ActionExecutionResult, error) {
	s.persistCalls++
	if s.persistCalls == 1 {
		s.beforeFirstPersist()
	}
	result, err := s.KernelStore.PersistActionExecution(ctx, execution)
	if errors.Is(err, models.ErrRevisionConflict) {
		s.sawRevisionConflict = true
	}
	return result, err
}
