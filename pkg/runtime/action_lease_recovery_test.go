package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestActionClaimPersistsRunningRecoveryProvenance(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
		_, proposal := createRunnableAction(t, store, now)
		first, err := store.ClaimNextAction(t.Context(), ActionClaim{
			Scope: proposal.Call.Scope, WorkerID: "first", Now: now.Add(2 * time.Second), LeaseDuration: time.Minute,
		})
		if err != nil || first == nil || first.RecoveredRunning || first.Attempt != 1 || first.MaxAttempts != 1 {
			t.Fatalf("fresh Ready claim = %#v, %v", first, err)
		}
		// Recover even when the transport retry budget is one: recovery must
		// record ambiguity instead of treating this claim as permission to retry.
		recoveryTime := first.LeaseExpiresAt.Add(time.Millisecond)
		recovered, err := store.ClaimNextAction(t.Context(), ActionClaim{
			Scope: first.Scope, WorkerID: "recovery", Now: recoveryTime, LeaseDuration: time.Minute,
		})
		if err != nil || recovered == nil || !recovered.RecoveredRunning || recovered.Attempt != 2 {
			t.Fatalf("expired Running recovery = %#v, %v", recovered, err)
		}
		stored, err := store.GetActionCall(t.Context(), first.Scope, first.ID)
		if err != nil || stored == nil || !stored.RecoveredRunning {
			t.Fatalf("durable recovery provenance = %#v, %v", stored, err)
		}
		// A maintenance or pause race can defer the recovered claim before this
		// worker dispatches. That does not prove the previous worker did not.
		deferred := deferClaimedActionForRecoveryTest(t, store, recovered, recoveryTime.Add(time.Second))
		again, err := store.ClaimNextAction(t.Context(), ActionClaim{
			Scope: first.Scope, WorkerID: "after-deferral", Now: deferred.AvailableAt, LeaseDuration: time.Minute,
		})
		if err != nil || again == nil || !again.RecoveredRunning || again.Attempt != 3 {
			t.Fatalf("Ready deferral erased prior Running recovery = %#v, %v", again, err)
		}
	})
}

func TestActionReadyDeferralIsNotRunningRecovery(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
		_, proposal := createRunnableAction(t, store, now)
		claimed, err := store.ClaimNextAction(t.Context(), ActionClaim{
			Scope: proposal.Call.Scope, WorkerID: "first", Now: now.Add(2 * time.Second), LeaseDuration: time.Minute,
		})
		if err != nil || claimed == nil {
			t.Fatalf("fresh claim = %#v, %v", claimed, err)
		}
		for index := 0; index < 2; index++ {
			// Mirror the Ready transition used when a worker sees a maintenance
			// or pause race before dispatch. Multiple claims alone are harmless.
			deferred := deferClaimedActionForRecoveryTest(t, store, claimed, now.Add(time.Duration(3+index)*time.Second))
			claimed, err = store.ClaimNextAction(t.Context(), ActionClaim{
				Scope: claimed.Scope, WorkerID: fmt.Sprintf("deferred-%d", index), Now: deferred.AvailableAt, LeaseDuration: time.Minute,
			})
			if err != nil || claimed == nil || claimed.RecoveredRunning || claimed.Attempt != index+2 {
				t.Fatalf("Ready deferral incorrectly marked Running recovery = %#v, %v", claimed, err)
			}
		}
	})
}

func TestActionRunningRecoveryProvenanceSurvivesSQLiteRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "action-recovery.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	_, proposal := createRunnableAction(t, store, now)
	first, err := store.ClaimNextAction(t.Context(), ActionClaim{
		Scope: proposal.Call.Scope, WorkerID: "first", Now: now.Add(2 * time.Second), LeaseDuration: time.Minute,
	})
	if err != nil || first == nil {
		t.Fatalf("fresh claim = %#v, %v", first, err)
	}
	recoveryTime := first.LeaseExpiresAt.Add(time.Millisecond)
	recovered, err := store.ClaimNextAction(t.Context(), ActionClaim{
		Scope: first.Scope, WorkerID: "recovery", Now: recoveryTime, LeaseDuration: time.Minute,
	})
	if err != nil || recovered == nil || !recovered.RecoveredRunning {
		t.Fatalf("recovered claim = %#v, %v", recovered, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err := reopened.GetActionCall(t.Context(), first.Scope, first.ID)
	if err != nil || stored == nil || !stored.RecoveredRunning {
		t.Fatalf("restart lost recovery provenance = %#v, %v", stored, err)
	}
	deferred := deferClaimedActionForRecoveryTest(t, reopened, stored, recoveryTime.Add(time.Second))
	again, err := reopened.ClaimNextAction(t.Context(), ActionClaim{
		Scope: first.Scope, WorkerID: "after-restart", Now: deferred.AvailableAt, LeaseDuration: time.Minute,
	})
	if err != nil || again == nil || !again.RecoveredRunning {
		t.Fatalf("post-restart Ready deferral lost recovery provenance = %#v, %v", again, err)
	}
}

func TestActionWorkerRecoveredMutationStopsBeforeProviderAuthority(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
		catalog, proposal := createRunnableAction(t, store, now)
		first, err := store.ClaimNextAction(t.Context(), ActionClaim{
			Scope: proposal.Call.Scope, WorkerID: "interrupted-worker", Now: now.Add(2 * time.Second), LeaseDuration: time.Minute,
		})
		if err != nil || first == nil || first.MaxAttempts != 1 {
			t.Fatalf("interrupted claim = %#v, %v", first, err)
		}
		catalogCalls, credentialCalls, dispatches := 0, 0, 0
		worker := NewActionWorker(store, &actionLifecycleCountingCatalog{ActionExecutionCatalog: catalog, calls: &catalogCalls},
			CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
				credentialCalls++
				return map[string]string{"token": "private-secret"}, nil
			}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
				dispatches++
				return map[string]interface{}{"providerResult": "sent twice"}, nil
			}))
		worker.now = func() time.Time { return first.LeaseExpiresAt.Add(time.Millisecond) }
		result, err := worker.RunOnce(t.Context(), first.Scope, "recovery-worker", time.Minute)
		if err != nil || result == nil || result.Call == nil || result.Call.Status != ActionCallStatusFailed ||
			!result.Call.RecoveredRunning || result.Call.FailurePhase != ActionFailureAfterDispatch || result.Call.Attempt != 2 {
			t.Fatalf("recovered mutation did not record ambiguous prior dispatch = %#v, %v", result, err)
		}
		if catalogCalls != 0 || credentialCalls != 0 || dispatches != 0 {
			t.Fatalf("recovered mutation touched provider authority: catalog=%d credentials=%d dispatches=%d", catalogCalls, credentialCalls, dispatches)
		}
		if !strings.Contains(result.Call.Error, "may already have taken effect") || result.Run == nil ||
			!requiresFinalFailureExplanation(result.Run.Checkpoint) {
			t.Fatalf("uncertainty was not preserved for the final explanation = %#v", result)
		}
		if _, retryable := ReadToolFeedbackCorrection(result.Run.Checkpoint); retryable {
			t.Fatal("ambiguous recovered mutation opened a fresh correction budget")
		}
		assertActionLifecycleUnclaimable(t, store, first, worker.now().Add(2*time.Minute))
	})
}

func TestActionWorkerRecoveredReadMayExecute(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
		catalog, scope := feedbackReadCatalog(t)
		run := feedbackCreateRun(t, store, scope, now)
		proposal := feedbackPropose(t, store, catalog, run, now, "safe-read", "id", ActionDispositionAllow)
		first, err := store.ClaimNextAction(t.Context(), ActionClaim{
			Scope: scope, WorkerID: "interrupted-worker", Now: now.Add(2 * time.Second), LeaseDuration: time.Minute,
		})
		if err != nil || first == nil || first.ID != proposal.Call.ID || first.MaxAttempts != 1 {
			t.Fatalf("interrupted read claim = %#v, %v", first, err)
		}
		catalogCalls, credentialCalls, dispatches := 0, 0, 0
		worker := NewActionWorker(store, &actionLifecycleCountingCatalog{ActionExecutionCatalog: catalog, calls: &catalogCalls},
			CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
				credentialCalls++
				return map[string]string{"token": "private-secret"}, nil
			}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
				dispatches++
				return map[string]interface{}{"records": []interface{}{"read-result"}}, nil
			}))
		worker.now = func() time.Time { return first.LeaseExpiresAt.Add(time.Millisecond) }
		result, err := worker.RunOnce(t.Context(), scope, "recovery-worker", time.Minute)
		if err != nil || result == nil || result.Call == nil || result.Call.Status != ActionCallStatusSucceeded ||
			!result.Call.RecoveredRunning || result.Call.FailurePhase != "" || result.Call.Attempt != 2 {
			t.Fatalf("recovered read was not allowed = %#v, %v", result, err)
		}
		if catalogCalls != 1 || credentialCalls != 1 || dispatches != 1 {
			t.Fatalf("recovered read bypassed normal authority or repeated dispatch: catalog=%d credentials=%d dispatches=%d", catalogCalls, credentialCalls, dispatches)
		}
		assertActionLifecycleUnclaimable(t, store, first, worker.now().Add(2*time.Minute))
	})
}

func deferClaimedActionForRecoveryTest(t *testing.T, store KernelStore, claimed *ActionCall, now time.Time) *ActionCall {
	t.Helper()
	parent := getActionLifecycleRun(t, store, claimed)
	deferred := cloneActionCall(claimed)
	deferred.Status = ActionCallStatusReady
	deferred.LeaseOwner, deferred.LeaseExpiresAt = "", nil
	deferred.AvailableAt = now
	deferred.UpdatedAt = now
	deferred.Revision++
	result, err := store.PersistActionExecution(t.Context(), ActionExecutionRecord{
		Call: deferred, ExpectedCallRevision: claimed.Revision, ExpectedRunRevision: parent.Revision,
		WorkerID: claimed.LeaseOwner, Now: now,
		Event: actionLifecycleEvent(deferred, fmt.Sprintf("deferred-%d", deferred.Revision), "action.deferred", now),
	})
	if err != nil || result == nil || result.Call == nil {
		t.Fatalf("persist Ready deferral = %#v, %v", result, err)
	}
	return result.Call
}
