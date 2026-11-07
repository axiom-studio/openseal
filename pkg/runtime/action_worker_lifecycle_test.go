package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

func TestActionWorkerLifecyclePreservesCompletedDispatchAcrossRunChanges(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		status AgentRunStatus
		fail   bool
	}{
		{name: "paused_success", status: AgentRunStatusPaused},
		{name: "canceled_success", status: AgentRunStatusCanceled},
		{name: "paused_failure", status: AgentRunStatusPaused, fail: true},
		{name: "canceled_failure", status: AgentRunStatusCanceled, fail: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
				f := newLoopWorkerFixture(t, store, &BudgetPolicy{MaxActions: 10})
				call := f.propose("deploy-1", "deploy")
				var changed *AgentRun
				worker := f.worker(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
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
				expected := ActionCallStatusSucceeded
				if testCase.fail {
					expected = ActionCallStatusFailed
				}
				if result.Call.Status != expected || result.Call.CompletedAt == nil || result.Call.LeaseOwner != "" || f.dispatches != 1 {
					t.Fatalf("completed dispatch was not preserved: %#v dispatches=%d", result.Call, f.dispatches)
				}
				if testCase.fail && result.Call.FailurePhase != ActionFailureAfterDispatch || !testCase.fail && result.Call.Output["providerResult"] != "delivered" {
					t.Fatalf("completed dispatch lost its result: %#v", result.Call)
				}
				after := f.currentRun()
				if after.Status != testCase.status || !reflect.DeepEqual(after.Checkpoint, changed.Checkpoint) || after.WakeCondition != nil ||
					after.BudgetUsage.Actions != 1 || len(after.BudgetReservations) != 0 {
					t.Fatalf("completed dispatch changed its Run beyond the budget: before=%#v after=%#v", changed, after)
				}
				assertActionLifecycleTerminalEvents(t, store, call, expected, 1)
				assertActionLifecycleUnclaimable(t, store, call, f.clock.Add(time.Hour))
			})
		})
	}
}

func TestActionWorkerLifecycleRetriesPersistenceWithoutRedispatch(t *testing.T) {
	for _, runMutation := range []string{"heartbeat", "cancel"} {
		t.Run(runMutation, func(t *testing.T) {
			forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
				f := newLoopWorkerFixture(t, store, &BudgetPolicy{MaxActions: 10})
				call := f.propose("deploy-1", "deploy")
				wrapped := &actionLifecycleConflictStore{KernelStore: store}
				wrapped.beforeFirstPersist = func() {
					if runMutation == "cancel" {
						f.setRunStatus(AgentRunStatusCanceled)
						return
					}
					if _, err := store.RenewAgentRunLease(t.Context(), f.scope, f.run.ID, loopWorkerID, f.clock, time.Hour); err != nil {
						t.Fatal(err)
					}
				}
				f.store = wrapped
				result, err := f.execute(f.worker(succeedDispatch), call.ID)
				if err != nil || result == nil || result.Call.Status != ActionCallStatusSucceeded || result.Call.Output["providerResult"] != "delivered" {
					t.Fatalf("completion did not survive revision conflict: %#v, %v", result, err)
				}
				if f.dispatches != 1 || wrapped.persistCalls != 2 || !wrapped.sawRevisionConflict {
					t.Fatalf("persistence conflict redispatched or was not retried: dispatches=%d persists=%d conflict=%t", f.dispatches, wrapped.persistCalls, wrapped.sawRevisionConflict)
				}
				after := f.currentRun()
				expected := AgentRunStatusRunning
				if runMutation == "cancel" {
					expected = AgentRunStatusCanceled
				}
				if after.Status != expected || after.BudgetUsage.Actions != 1 || len(after.BudgetReservations) != 0 {
					t.Fatalf("completion after a conflict changed its Run: %#v", after)
				}
				assertActionLifecycleTerminalEvents(t, store, call, ActionCallStatusSucceeded, 1)
				assertActionLifecycleUnclaimable(t, store, call, f.clock.Add(time.Hour))
			})
		})
	}
}

func TestActionWorkerLifecycleCallOnlyCompletionFencesParentRevision(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		f := newLoopWorkerFixture(t, store, nil)
		proposed := f.propose("deploy-1", "deploy")
		call, err := store.ClaimNextAction(t.Context(), ActionClaim{Scope: f.scope, WorkerID: loopWorkerID, Now: f.clock, LeaseDuration: time.Minute, OwnActionID: proposed.ID})
		if err != nil || call == nil {
			t.Fatalf("claim = %#v, %v", call, err)
		}
		parent := f.currentRun()
		baselineEvents, err := store.ListActivity(t.Context(), ActivityFilter{Scope: call.Scope, RunID: call.RunID})
		if err != nil {
			t.Fatal(err)
		}
		completed := cloneActionCall(call)
		completed.Revision++
		completed.Status = ActionCallStatusSucceeded
		completed.Output = map[string]interface{}{"providerResult": "delivered"}
		completed.LeaseOwner = ""
		completed.LeaseExpiresAt = nil
		completionTime := f.clock.Add(time.Second)
		completed.UpdatedAt = completionTime
		completed.CompletedAt = &completionTime
		event := actionLifecycleEvent(completed, "call-only-completion", "action.succeeded", completionTime)
		record := ActionExecutionRecord{
			Call: completed, ExpectedCallRevision: call.Revision, ExpectedRunRevision: parent.Revision + 1,
			WorkerID: loopWorkerID, Now: completionTime, Event: event,
		}
		if _, err := store.PersistActionExecution(t.Context(), record); !errors.Is(err, ErrRevisionConflict) {
			t.Fatalf("call-only completion did not fence stale parent revision: %v", err)
		}
		record.ExpectedRunRevision = 0
		if _, err := store.PersistActionExecution(t.Context(), record); err == nil {
			t.Fatal("call-only completion accepted missing parent revision")
		}
		other, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
			Scope: call.Scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "other-agent"},
			AssignedAgentID: "other-agent", Goal: "Independent workstream", Source: RunSourceManual,
		})
		if err != nil {
			t.Fatal(err)
		}
		other, err = store.GetAgentRun(t.Context(), other.Scope, other.ID)
		if err != nil || other == nil {
			t.Fatalf("snapshot independent Run: %#v, %v", other, err)
		}
		forged := record
		forged.Call = cloneActionCall(completed)
		forged.Call.RunID = other.ID
		forged.Event = cloneActivityEvent(event)
		forged.Event.RunID = other.ID
		forged.ExpectedRunRevision = other.Revision
		if _, err := store.PersistActionExecution(t.Context(), forged); !errors.Is(err, ErrInvalidScope) {
			t.Fatalf("call-only completion accepted another Run's identity and revision: %v", err)
		}
		otherAfter, err := store.GetAgentRun(t.Context(), other.Scope, other.ID)
		if err != nil || !reflect.DeepEqual(other, otherAfter) {
			t.Fatalf("forged completion changed another Run: %#v, %v", otherAfter, err)
		}
		otherEvents, err := store.ListActivity(t.Context(), ActivityFilter{Scope: other.Scope, RunID: other.ID})
		if err != nil || len(otherEvents) != 0 {
			t.Fatalf("forged completion appended another Run's activity: %#v, %v", otherEvents, err)
		}
		persisted, err := store.GetActionCall(t.Context(), call.Scope, call.ID)
		if err != nil || persisted.Status != ActionCallStatusRunning || persisted.Revision != call.Revision {
			t.Fatalf("rejected completion changed the call: %#v, %v", persisted, err)
		}
		events, err := store.ListActivity(t.Context(), ActivityFilter{Scope: call.Scope, RunID: call.RunID})
		if err != nil || !reflect.DeepEqual(events, baselineEvents) {
			t.Fatalf("rejected completion appended activity: %#v, %v", events, err)
		}
		record.ExpectedRunRevision = parent.Revision
		result, err := store.PersistActionExecution(t.Context(), record)
		if err != nil || result == nil || result.Call.Status != ActionCallStatusSucceeded {
			t.Fatalf("fenced call-only completion failed: %#v, %v", result, err)
		}
		if after := f.currentRun(); !reflect.DeepEqual(parent, after) {
			t.Fatalf("call-only completion altered the parent: before=%#v after=%#v", parent, after)
		}
		events, err = store.ListActivity(t.Context(), ActivityFilter{Scope: call.Scope, RunID: call.RunID})
		if err != nil || len(events) != len(baselineEvents)+1 || events[len(events)-1].ID != event.ID {
			t.Fatalf("successful completion did not append exactly one activity: %#v, %v", events, err)
		}
	})
}

func TestActionWorkerLifecycleBoundsPersistenceConflicts(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		f := newLoopWorkerFixture(t, store, nil)
		call := f.propose("deploy-1", "deploy")
		before := f.currentRun()
		wrapped := &actionLifecycleAlwaysConflictStore{KernelStore: store}
		f.store = wrapped
		result, err := f.execute(f.worker(succeedDispatch), call.ID)
		if result != nil || !errors.Is(err, ErrRevisionConflict) || f.dispatches != 1 || wrapped.persistCalls != 8 {
			t.Fatalf("persistence conflict retries were unbounded or repeated dispatch: %#v, %v, dispatches=%d persists=%d", result, err, f.dispatches, wrapped.persistCalls)
		}
		if after := f.currentRun(); !reflect.DeepEqual(before, after) {
			t.Fatalf("rejected completion changed the Run: before=%#v after=%#v", before, after)
		}
	})
}

func forActionLifecycleStores(t *testing.T, test func(*testing.T, KernelStore)) {
	t.Helper()
	for _, testCase := range []struct {
		name string
		open func(*testing.T) KernelStore
	}{
		{name: "memory", open: func(*testing.T) KernelStore { return NewMemoryStore() }},
		{name: "sqlite", open: func(t *testing.T) KernelStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "action-lifecycle.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
		{name: "postgres", open: func(t *testing.T) KernelStore {
			dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
			if dsn == "" {
				t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL lifecycle regressions")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema("openseal_action_lifecycle_"+uuid.NewString()[:8]))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_, _ = store.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+store.quotedSchema()+` CASCADE`)
				_ = store.Close()
			})
			return store
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) { test(t, testCase.open(t)) })
	}
}

func getActionLifecycleRun(t *testing.T, store KernelStore, call *ActionCall) *AgentRun {
	t.Helper()
	run, err := store.GetAgentRun(t.Context(), call.Scope, call.RunID)
	if err != nil || run == nil {
		t.Fatalf("get parent Run = %#v, %v", run, err)
	}
	return run
}

func transitionActionLifecycleRun(t *testing.T, store KernelStore, call *ActionCall, status AgentRunStatus, wake *WakeCondition, now time.Time) *AgentRun {
	t.Helper()
	run := getActionLifecycleRun(t, store, call)
	service := NewRunActivityService(store, store)
	service.now = func() time.Time { return now }
	updated, _, err := service.TransitionRun(t.Context(), call.Scope, call.RunID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: status, WakeCondition: wake,
		Actor: ActivityActor{Type: "user", ID: "alice"}, Summary: "Update the current workstream",
	})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func mutateActionLifecycleRun(t *testing.T, store KernelStore, call *ActionCall, now time.Time, mutate func(*AgentRun)) *AgentRun {
	t.Helper()
	run := getActionLifecycleRun(t, store, call)
	expected := run.Revision
	mutate(run)
	run.Revision++
	run.UpdatedAt = now
	event := actionLifecycleEvent(call, fmt.Sprintf("lifecycle-mutation-%d", run.Revision), "run.updated", now)
	if _, err := store.UpdateAgentRunWithEvent(t.Context(), run, expected, event, nil); err != nil {
		t.Fatal(err)
	}
	return run
}

func assertActionLifecycleTerminalEvents(t *testing.T, store KernelStore, call *ActionCall, status ActionCallStatus, actions int64) {
	t.Helper()
	events, err := store.ListActivity(t.Context(), ActivityFilter{Scope: call.Scope, RunID: call.RunID, Limit: 100})
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

func assertActionLifecycleUnclaimable(t *testing.T, store KernelStore, call *ActionCall, now time.Time) {
	t.Helper()
	claimed, err := store.ClaimNextAction(t.Context(), ActionClaim{Scope: call.Scope, WorkerID: "later-worker", Now: now, LeaseDuration: time.Minute})
	if err != nil || claimed != nil {
		t.Fatalf("terminal action was eligible again: %#v, %v", claimed, err)
	}
}

func actionLifecycleEvent(call *ActionCall, id, eventType string, now time.Time) *ActivityEvent {
	return &ActivityEvent{
		ID: id, Scope: call.Scope, RunID: call.RunID, AgentID: call.DeploymentID,
		EventType: eventType, Severity: ActivitySeverityInfo, Summary: "Record action lifecycle state",
		Actor: ActivityActor{Type: "worker", ID: "lifecycle-worker"}, Visibility: ActivityVisibilityScope,
		CreatedAt: now, Payload: map[string]interface{}{"actionCallId": call.ID, "status": call.Status},
	}
}

type actionLifecycleCountingCatalog struct {
	ActionExecutionCatalog
	calls *int
}

func (c *actionLifecycleCountingCatalog) Resolve(ctx context.Context, scope skill.ScopeReference, deploymentID, skillID, version, action string, bindings ...skill.BindingReference) (*skill.BoundAction, error) {
	(*c.calls)++
	return c.ActionExecutionCatalog.Resolve(ctx, scope, deploymentID, skillID, version, action, bindings...)
}

type actionLifecycleConflictStore struct {
	KernelStore
	beforeFirstPersist  func()
	persistCalls        int
	sawRevisionConflict bool
}

type actionLifecycleAlwaysConflictStore struct {
	KernelStore
	persistCalls int
}

func (s *actionLifecycleAlwaysConflictStore) PersistActionExecution(context.Context, ActionExecutionRecord) (*ActionExecutionResult, error) {
	s.persistCalls++
	return nil, ErrRevisionConflict
}

func (s *actionLifecycleConflictStore) PersistActionExecution(ctx context.Context, execution ActionExecutionRecord) (*ActionExecutionResult, error) {
	s.persistCalls++
	if s.persistCalls == 1 {
		s.beforeFirstPersist()
	}
	result, err := s.KernelStore.PersistActionExecution(ctx, execution)
	if errors.Is(err, ErrRevisionConflict) {
		s.sawRevisionConflict = true
	}
	return result, err
}
