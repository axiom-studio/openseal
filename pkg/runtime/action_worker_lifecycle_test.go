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

func TestActionWorkerLifecycleCancelsUnownedDependencies(t *testing.T) {
	for _, parentState := range []string{"canceled", "queued", "replaced_dependency", "paused_replaced_dependency"} {
		t.Run(parentState, func(t *testing.T) {
			forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
				now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
				catalog, proposal := createRunnableAction(t, store, now)
				addActionLifecycleBudget(t, store, proposal.Call, now)
				switch parentState {
				case "canceled":
					transitionActionLifecycleRun(t, store, proposal.Call, AgentRunStatusCanceled, nil, now.Add(time.Second))
				case "queued":
					transitionActionLifecycleRun(t, store, proposal.Call, AgentRunStatusQueued, nil, now.Add(time.Second))
				case "replaced_dependency":
					mutateActionLifecycleRun(t, store, proposal.Call, now.Add(time.Second), func(run *AgentRun) {
						run.WakeCondition = &WakeCondition{Type: "action", Reference: "newer-action"}
					})
				case "paused_replaced_dependency":
					transitionActionLifecycleRun(t, store, proposal.Call, AgentRunStatusPaused, nil, now.Add(time.Second))
					mutateActionLifecycleRun(t, store, proposal.Call, now.Add(time.Second), func(run *AgentRun) {
						run.PausedWakeCondition = &WakeCondition{Type: "action", Reference: "newer-action"}
					})
				}
				before := getActionLifecycleRun(t, store, proposal.Call)
				catalogCalls, credentialCalls, dispatchCalls := 0, 0, 0
				countingCatalog := &actionLifecycleCountingCatalog{ActionExecutionCatalog: catalog, calls: &catalogCalls}
				worker := NewActionWorker(store, countingCatalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
					credentialCalls++
					return map[string]string{"token": "secret"}, nil
				}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
					dispatchCalls++
					return map[string]interface{}{"ok": true}, nil
				}))
				worker.now = func() time.Time { return now.Add(2 * time.Second) }
				result, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "lifecycle-worker", time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				if result == nil || result.Call.Status != ActionCallStatusCanceled || result.Call.CompletedAt == nil || result.Call.LeaseOwner != "" || result.Call.LeaseExpiresAt != nil {
					t.Fatalf("orphan action was not durably canceled: %#v", result)
				}
				if catalogCalls != 0 || credentialCalls != 0 || dispatchCalls != 0 {
					t.Fatalf("unowned action reached execution: catalog=%d credentials=%d dispatch=%d", catalogCalls, credentialCalls, dispatchCalls)
				}
				after := getActionLifecycleRun(t, store, proposal.Call)
				if after.Status != before.Status || !reflect.DeepEqual(after.WakeCondition, before.WakeCondition) || !reflect.DeepEqual(after.PausedWakeCondition, before.PausedWakeCondition) || !reflect.DeepEqual(after.Checkpoint, before.Checkpoint) {
					t.Fatalf("cancellation overwrote the current parent state: before=%#v after=%#v", before, after)
				}
				assertActionLifecycleBudget(t, after, proposal.Call.ID, 0)
				assertActionLifecycleTerminalEvents(t, store, proposal.Call, ActionCallStatusCanceled, 0)
				assertActionLifecycleUnclaimable(t, store, proposal.Call, now.Add(2*time.Minute))
			})
		})
	}
}

func TestActionWorkerLifecycleSuspendsPausedDependencyUntilResume(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
		catalog, proposal := createRunnableAction(t, store, now)
		paused := transitionActionLifecycleRun(t, store, proposal.Call, AgentRunStatusPaused, nil, now.Add(time.Second))
		catalogCalls, credentialCalls, dispatchCalls := 0, 0, 0
		worker := NewActionWorker(store, &actionLifecycleCountingCatalog{ActionExecutionCatalog: catalog, calls: &catalogCalls}, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
			credentialCalls++
			return map[string]string{"token": "secret"}, nil
		}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
			dispatchCalls++
			return map[string]interface{}{"ok": true}, nil
		}))
		clock := now.Add(2 * time.Second)
		worker.now = func() time.Time { return clock }
		for attempt := 0; attempt < 3; attempt++ {
			result, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "lifecycle-worker", time.Minute)
			if err != nil || result != nil {
				t.Fatalf("paused dependency was claimed: %#v, %v", result, err)
			}
			clock = clock.Add(time.Minute)
		}
		call, err := store.GetActionCall(t.Context(), proposal.Call.Scope, proposal.Call.ID)
		if err != nil || call.Status != ActionCallStatusReady || call.Attempt != 0 || call.Revision != proposal.Call.Revision || call.LeaseOwner != "" {
			t.Fatalf("paused queue consumed attempts or acquired a lease: %#v, %v", call, err)
		}
		if catalogCalls != 0 || credentialCalls != 0 || dispatchCalls != 0 {
			t.Fatalf("paused action reached execution: catalog=%d credentials=%d dispatch=%d", catalogCalls, credentialCalls, dispatchCalls)
		}
		transitionActionLifecycleRun(t, store, proposal.Call, paused.PausedFrom, paused.PausedWakeCondition, clock)
		result, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "lifecycle-worker", time.Minute)
		if err != nil || result == nil || result.Call.Status != ActionCallStatusSucceeded || result.Call.Attempt != 1 {
			t.Fatalf("resumed dependency did not execute: %#v, %v", result, err)
		}
		if catalogCalls != 1 || credentialCalls != 1 || dispatchCalls != 1 {
			t.Fatalf("resumed dependency execution count: catalog=%d credentials=%d dispatch=%d", catalogCalls, credentialCalls, dispatchCalls)
		}
		assertActionLifecycleUnclaimable(t, store, proposal.Call, clock.Add(2*time.Minute))
	})
}

func TestActionWorkerLifecyclePreservesCompletedDispatchAcrossParentChanges(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		parentStatus AgentRunStatus
		fail         bool
		attempts     int
	}{
		{name: "paused_success", parentStatus: AgentRunStatusPaused, attempts: 1},
		{name: "canceled_success", parentStatus: AgentRunStatusCanceled, attempts: 1},
		{name: "paused_terminal_failure", parentStatus: AgentRunStatusPaused, fail: true, attempts: 3},
		{name: "canceled_retryable_failure", parentStatus: AgentRunStatusCanceled, fail: true, attempts: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
				now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
				catalog, proposal := createRunnableAction(t, store, now)
				addActionLifecycleBudget(t, store, proposal.Call, now)
				clock := now.Add(2 * time.Second)
				dispatches := 0
				var changedParent *AgentRun
				worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
					return map[string]string{"token": "secret"}, nil
				}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
					dispatches++
					if dispatches == testCase.attempts {
						changedParent = transitionActionLifecycleRun(t, store, proposal.Call, testCase.parentStatus, nil, clock)
					}
					if testCase.fail {
						return nil, errors.New("provider rejected operation")
					}
					return map[string]interface{}{"providerResult": "delivered"}, nil
				}))
				worker.now = func() time.Time { return clock }
				var result *ActionExecutionResult
				for attempt := 0; attempt < testCase.attempts; attempt++ {
					var err error
					result, err = worker.RunOnce(t.Context(), proposal.Call.Scope, "lifecycle-worker", time.Minute)
					if err != nil {
						t.Fatal(err)
					}
					clock = clock.Add(time.Hour)
				}
				expectedStatus := ActionCallStatusSucceeded
				if testCase.fail {
					expectedStatus = ActionCallStatusFailed
				}
				if result == nil || result.Call.Status != expectedStatus || result.Call.CompletedAt == nil || result.Call.LeaseOwner != "" || result.Call.LeaseExpiresAt != nil || dispatches != testCase.attempts {
					t.Fatalf("completed dispatch was not preserved: %#v dispatches=%d", result, dispatches)
				}
				if testCase.fail && result.Call.Error == "" || !testCase.fail && result.Call.Output["providerResult"] != "delivered" {
					t.Fatalf("completed dispatch lost its result: %#v", result.Call)
				}
				after := getActionLifecycleRun(t, store, proposal.Call)
				if after.Status != testCase.parentStatus {
					t.Fatalf("completed dispatch revived its parent: %#v", after)
				}
				if testCase.parentStatus == AgentRunStatusPaused {
					lastAction, _ := after.Checkpoint["lastAction"].(map[string]interface{})
					if after.PausedFrom != AgentRunStatusQueued || after.PausedWakeCondition != nil || after.WakeCondition != nil || lastAction["actionCallId"] != proposal.Call.ID || len(actionHistoryEntries(after.Checkpoint)) != 1 {
						t.Fatalf("paused completion lost its resumable checkpoint: %#v", after)
					}
				} else if !reflect.DeepEqual(after.Checkpoint, changedParent.Checkpoint) || after.CompletedAt == nil || !after.CompletedAt.Equal(*changedParent.CompletedAt) {
					t.Fatalf("completed dispatch replaced canceled state: before=%#v after=%#v", changedParent, after)
				}
				assertActionLifecycleBudget(t, after, proposal.Call.ID, 1)
				assertActionLifecycleTerminalEvents(t, store, proposal.Call, expectedStatus, 1)
				assertActionLifecycleUnclaimable(t, store, proposal.Call, clock)
			})
		})
	}
}

func TestActionWorkerLifecycleSuspendsRetryCreatedDuringPause(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
		catalog, proposal := createRunnableAction(t, store, now)
		clock := now.Add(2 * time.Second)
		dispatches := 0
		worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
			return map[string]string{"token": "secret"}, nil
		}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
			dispatches++
			if dispatches == 1 {
				transitionActionLifecycleRun(t, store, proposal.Call, AgentRunStatusPaused, nil, clock)
				return nil, errors.New("temporary provider failure")
			}
			return map[string]interface{}{"providerResult": "delivered"}, nil
		}))
		worker.now = func() time.Time { return clock }
		first, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "lifecycle-worker", time.Minute)
		if err != nil || first == nil || first.Call.Status != ActionCallStatusReady || first.Call.Attempt != 1 {
			t.Fatalf("paused retry did not preserve its actual execution state: %#v, %v", first, err)
		}
		paused := getActionLifecycleRun(t, store, proposal.Call)
		if paused.Status != AgentRunStatusPaused || paused.PausedFrom != AgentRunStatusWaitingForDependency || paused.PausedWakeCondition == nil || paused.PausedWakeCondition.Reference != proposal.Call.ID {
			t.Fatalf("paused retry lost its dependency: %#v", paused)
		}
		clock = first.Call.AvailableAt.Add(time.Minute)
		blocked, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "lifecycle-worker", time.Minute)
		if err != nil || blocked != nil || dispatches != 1 {
			t.Fatalf("paused retry ran before resume: %#v, %v, dispatches=%d", blocked, err, dispatches)
		}
		persisted, err := store.GetActionCall(t.Context(), proposal.Call.Scope, proposal.Call.ID)
		if err != nil || persisted.Attempt != 1 || persisted.Revision != first.Call.Revision {
			t.Fatalf("paused retry consumed another attempt: %#v, %v", persisted, err)
		}
		transitionActionLifecycleRun(t, store, proposal.Call, paused.PausedFrom, paused.PausedWakeCondition, clock)
		completed, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "lifecycle-worker", time.Minute)
		if err != nil || completed == nil || completed.Call.Status != ActionCallStatusSucceeded || completed.Call.Attempt != 2 || dispatches != 2 {
			t.Fatalf("resumed retry failed: %#v, %v, dispatches=%d", completed, err, dispatches)
		}
		assertActionLifecycleUnclaimable(t, store, proposal.Call, clock.Add(time.Minute))
	})
}

func TestActionWorkerLifecycleRechecksParentAfterCredentialResolution(t *testing.T) {
	for _, parentStatus := range []AgentRunStatus{AgentRunStatusPaused, AgentRunStatusCanceled} {
		t.Run(string(parentStatus), func(t *testing.T) {
			forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
				now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
				catalog, proposal := createRunnableAction(t, store, now)
				dispatches := 0
				worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
					transitionActionLifecycleRun(t, store, proposal.Call, parentStatus, nil, now.Add(2*time.Second))
					return map[string]string{"token": "secret"}, nil
				}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
					dispatches++
					return map[string]interface{}{"ok": true}, nil
				}))
				worker.now = func() time.Time { return now.Add(2 * time.Second) }
				result, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "lifecycle-worker", time.Minute)
				if err != nil || result == nil || dispatches != 0 {
					t.Fatalf("parent change during credential resolution reached dispatch: %#v, %v, dispatches=%d", result, err, dispatches)
				}
				after := getActionLifecycleRun(t, store, proposal.Call)
				if after.Status != parentStatus || after.Checkpoint["lastAction"] != nil {
					t.Fatalf("unexecuted action overwrote parent state: %#v", after)
				}
				if parentStatus == AgentRunStatusPaused {
					if result.Call.Status != ActionCallStatusReady || result.Call.CompletedAt != nil || result.Call.LeaseOwner != "" || after.PausedWakeCondition == nil || after.PausedWakeCondition.Reference != proposal.Call.ID {
						t.Fatalf("paused action was not deferred: %#v, parent=%#v", result.Call, after)
					}
				} else if result.Call.Status != ActionCallStatusCanceled || result.Call.CompletedAt == nil {
					t.Fatalf("canceled action was not terminalized: %#v", result.Call)
				}
				assertActionLifecycleUnclaimable(t, store, proposal.Call, now.Add(2*time.Minute))
			})
		})
	}
}

func TestActionWorkerLifecyclePreservesSupersedingContinuation(t *testing.T) {
	for _, continuation := range []string{"queued", "new_dependency"} {
		t.Run(continuation, func(t *testing.T) {
			forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
				now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
				catalog, proposal := createRunnableAction(t, store, now)
				addActionLifecycleBudget(t, store, proposal.Call, now)
				var superseding *AgentRun
				dispatches := 0
				worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
					return map[string]string{"token": "secret"}, nil
				}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
					dispatches++
					if continuation == "queued" {
						transitionActionLifecycleRun(t, store, proposal.Call, AgentRunStatusQueued, nil, now.Add(2*time.Second))
					}
					superseding = mutateActionLifecycleRun(t, store, proposal.Call, now.Add(2*time.Second), func(run *AgentRun) {
						run.Checkpoint = map[string]interface{}{"latestUserInput": "new continuation"}
						if continuation == "new_dependency" {
							run.WakeCondition = &WakeCondition{Type: "action", Reference: "newer-action"}
						}
					})
					return map[string]interface{}{"providerResult": "delivered"}, nil
				}))
				worker.now = func() time.Time { return now.Add(2 * time.Second) }
				result, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "lifecycle-worker", time.Minute)
				if err != nil || result == nil || result.Call.Status != ActionCallStatusSucceeded || dispatches != 1 {
					t.Fatalf("completed dispatch was not preserved: %#v, %v, dispatches=%d", result, err, dispatches)
				}
				after := getActionLifecycleRun(t, store, proposal.Call)
				if after.Status != superseding.Status || !reflect.DeepEqual(after.WakeCondition, superseding.WakeCondition) || !reflect.DeepEqual(after.Checkpoint, superseding.Checkpoint) || after.LastWakeSignalID != superseding.LastWakeSignalID {
					t.Fatalf("completed action overwrote a newer continuation: before=%#v after=%#v", superseding, after)
				}
				assertActionLifecycleBudget(t, after, proposal.Call.ID, 1)
				assertActionLifecycleTerminalEvents(t, store, proposal.Call, ActionCallStatusSucceeded, 1)
				assertActionLifecycleUnclaimable(t, store, proposal.Call, now.Add(2*time.Minute))
			})
		})
	}
}

func TestActionWorkerLifecycleRetriesPersistenceWithoutRedispatch(t *testing.T) {
	for _, parentMutation := range []string{"checkpoint", "cancel"} {
		t.Run(parentMutation, func(t *testing.T) {
			forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
				now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
				catalog, proposal := createRunnableAction(t, store, now)
				addActionLifecycleBudget(t, store, proposal.Call, now)
				wrapped := &actionLifecycleConflictStore{KernelStore: store}
				wrapped.beforeFirstPersist = func() {
					if parentMutation == "cancel" {
						transitionActionLifecycleRun(t, store, proposal.Call, AgentRunStatusCanceled, nil, now.Add(2*time.Second))
					} else {
						mutateActionLifecycleRun(t, store, proposal.Call, now.Add(2*time.Second), func(run *AgentRun) {
							run.Checkpoint = map[string]interface{}{"latestUserInput": "preserve me"}
						})
					}
				}
				dispatches := 0
				worker := NewActionWorker(wrapped, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
					return map[string]string{"token": "secret"}, nil
				}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
					dispatches++
					return map[string]interface{}{"providerResult": "delivered"}, nil
				}))
				worker.now = func() time.Time { return now.Add(2 * time.Second) }
				result, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "lifecycle-worker", time.Minute)
				if err != nil || result == nil || result.Call.Status != ActionCallStatusSucceeded || result.Call.Output["providerResult"] != "delivered" {
					t.Fatalf("completion did not survive revision conflict: %#v, %v", result, err)
				}
				if dispatches != 1 || wrapped.persistCalls != 2 || !wrapped.sawRevisionConflict {
					t.Fatalf("persistence conflict redispatched action or was not retried: dispatches=%d persists=%d conflict=%t", dispatches, wrapped.persistCalls, wrapped.sawRevisionConflict)
				}
				after := getActionLifecycleRun(t, store, proposal.Call)
				if parentMutation == "cancel" {
					if after.Status != AgentRunStatusCanceled || after.Checkpoint["lastAction"] != nil {
						t.Fatalf("completion revived canceled parent: %#v", after)
					}
				} else if after.Status != AgentRunStatusQueued || after.Checkpoint["latestUserInput"] != "preserve me" || len(actionHistoryEntries(after.Checkpoint)) != 1 {
					t.Fatalf("completion overwrote concurrent checkpoint: %#v", after)
				}
				assertActionLifecycleBudget(t, after, proposal.Call.ID, 1)
				assertActionLifecycleTerminalEvents(t, store, proposal.Call, ActionCallStatusSucceeded, 1)
				assertActionLifecycleUnclaimable(t, store, proposal.Call, now.Add(2*time.Minute))
			})
		})
	}
}

func TestActionWorkerLifecycleCallOnlyCompletionFencesParentRevision(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
		_, proposal := createRunnableAction(t, store, now)
		call, err := store.ClaimNextAction(t.Context(), ActionClaim{Scope: proposal.Call.Scope, WorkerID: "lifecycle-worker", Now: now.Add(2 * time.Second), LeaseDuration: time.Minute})
		if err != nil || call == nil {
			t.Fatalf("claim = %#v, %v", call, err)
		}
		parent := getActionLifecycleRun(t, store, proposal.Call)
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
		completionTime := now.Add(3 * time.Second)
		completed.UpdatedAt = completionTime
		completed.CompletedAt = &completionTime
		event := actionLifecycleEvent(completed, "call-only-completion", "action.succeeded", completionTime)
		record := ActionExecutionRecord{
			Call: completed, ExpectedCallRevision: call.Revision, ExpectedRunRevision: parent.Revision + 1,
			WorkerID: "lifecycle-worker", Now: completionTime, Event: event,
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
		after := getActionLifecycleRun(t, store, proposal.Call)
		if !reflect.DeepEqual(parent, after) {
			t.Fatalf("call-only completion altered the parent: before=%#v after=%#v", parent, after)
		}
		events, err = store.ListActivity(t.Context(), ActivityFilter{Scope: call.Scope, RunID: call.RunID})
		if err != nil || len(events) != len(baselineEvents)+1 || events[len(events)-1].ID != event.ID {
			t.Fatalf("successful completion did not append exactly one activity: %#v, %v", events, err)
		}
	})
}

func TestActionWorkerLifecycleDefersPausedSnapshotAfterConcurrentResume(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
		catalog, proposal := createRunnableAction(t, store, now)
		wrapped := &actionLifecycleRunSnapshotStore{KernelStore: store}
		wrapped.firstSnapshot = func() *AgentRun {
			paused := transitionActionLifecycleRun(t, store, proposal.Call, AgentRunStatusPaused, nil, now.Add(2*time.Second))
			transitionActionLifecycleRun(t, store, proposal.Call, paused.PausedFrom, paused.PausedWakeCondition, now.Add(2*time.Second))
			return paused
		}
		catalogCalls, credentialCalls, dispatches := 0, 0, 0
		worker := NewActionWorker(wrapped, &actionLifecycleCountingCatalog{ActionExecutionCatalog: catalog, calls: &catalogCalls}, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
			credentialCalls++
			return map[string]string{"token": "secret"}, nil
		}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
			dispatches++
			return map[string]interface{}{"providerResult": "delivered"}, nil
		}))
		worker.now = func() time.Time { return now.Add(2 * time.Second) }
		deferred, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "lifecycle-worker", time.Minute)
		if err != nil || deferred == nil || deferred.Call.Status != ActionCallStatusReady || deferred.Call.CompletedAt != nil || deferred.Call.Output != nil {
			t.Fatalf("undispatched paused snapshot became a completed result: %#v, %v", deferred, err)
		}
		if catalogCalls != 0 || credentialCalls != 0 || dispatches != 0 {
			t.Fatalf("paused snapshot reached execution: catalog=%d credentials=%d dispatches=%d", catalogCalls, credentialCalls, dispatches)
		}
		after := getActionLifecycleRun(t, store, proposal.Call)
		if after.Status != AgentRunStatusWaitingForDependency || after.WakeCondition == nil || after.WakeCondition.Reference != proposal.Call.ID || after.Checkpoint["lastAction"] != nil {
			t.Fatalf("deferral overwrote the resumed dependency: %#v", after)
		}
		completed, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "lifecycle-worker", time.Minute)
		if err != nil || completed == nil || completed.Call.Status != ActionCallStatusSucceeded || completed.Call.Output["providerResult"] != "delivered" || dispatches != 1 {
			t.Fatalf("deferred action did not execute exactly once on next claim: %#v, %v, dispatches=%d", completed, err, dispatches)
		}
		assertActionLifecycleTerminalEvents(t, store, proposal.Call, ActionCallStatusSucceeded, 1)
	})
}

func TestActionWorkerLifecycleBoundsPersistenceConflicts(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC)
		catalog, proposal := createRunnableAction(t, store, now)
		wrapped := &actionLifecycleAlwaysConflictStore{KernelStore: store}
		dispatches := 0
		worker := NewActionWorker(wrapped, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
			return map[string]string{"token": "secret"}, nil
		}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
			dispatches++
			return map[string]interface{}{"providerResult": "delivered"}, nil
		}))
		worker.now = func() time.Time { return now.Add(2 * time.Second) }
		result, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "lifecycle-worker", time.Minute)
		if result != nil || !errors.Is(err, ErrRevisionConflict) || dispatches != 1 || wrapped.persistCalls != 8 {
			t.Fatalf("persistence conflict retries were unbounded or repeated dispatch: %#v, %v, dispatches=%d persists=%d", result, err, dispatches, wrapped.persistCalls)
		}
		after := getActionLifecycleRun(t, store, proposal.Call)
		if after.Status != AgentRunStatusWaitingForDependency || after.Checkpoint["lastAction"] != nil {
			t.Fatalf("rejected completion changed the parent: %#v", after)
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

func addActionLifecycleBudget(t *testing.T, store KernelStore, call *ActionCall, now time.Time) {
	t.Helper()
	mutateActionLifecycleRun(t, store, call, now, func(run *AgentRun) {
		run.Budget = &BudgetPolicy{MaxActions: 10}
		run.BudgetState = BudgetStateActive
		run.BudgetReservations = map[string]BudgetReservation{
			actionBudgetReservationID(call.ID): {ID: actionBudgetReservationID(call.ID), Usage: BudgetUsage{Actions: 1}, CreatedAt: now},
			"unrelated-work":                   {ID: "unrelated-work", Usage: BudgetUsage{Actions: 2}, CreatedAt: now},
		}
	})
}

func assertActionLifecycleBudget(t *testing.T, run *AgentRun, actionID string, actions int64) {
	t.Helper()
	if run.BudgetUsage.Actions != actions || len(run.BudgetReservations) != 1 || run.BudgetReservations["unrelated-work"].Usage.Actions != 2 {
		t.Fatalf("completion did not settle only its own budget reservation: %#v", run)
	}
	if _, reserved := run.BudgetReservations[actionBudgetReservationID(actionID)]; reserved {
		t.Fatalf("action reservation remains after completion: %#v", run.BudgetReservations)
	}
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

type actionLifecycleRunSnapshotStore struct {
	KernelStore
	firstSnapshot func() *AgentRun
	runReads      int
}

func (s *actionLifecycleRunSnapshotStore) GetAgentRun(ctx context.Context, scope Scope, id string) (*AgentRun, error) {
	run, err := s.KernelStore.GetAgentRun(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	s.runReads++
	if s.runReads == 1 {
		return s.firstSnapshot(), nil
	}
	return run, nil
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
