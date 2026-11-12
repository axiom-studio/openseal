package action_test

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
)

func TestActionClaimPersistsRunningRecoveryProvenance(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store models.KernelStore) {
		now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
		_, proposal := createRunnableAction(t, store, now)
		first, err := store.ClaimNextAction(t.Context(), models.ActionClaim{
			Scope: proposal.Call.Scope, WorkerID: "agent-worker", Now: now.Add(2 * time.Second), LeaseDuration: time.Minute, OwnActionID: proposal.Call.ID,
		})
		if err != nil || first == nil || first.RecoveredRunning || first.Attempt != 1 || first.MaxAttempts != 1 {
			t.Fatalf("fresh Ready claim = %#v, %v", first, err)
		}
		// Recover even when the transport retry budget is one: recovery must
		// record ambiguity instead of treating this claim as permission to retry.
		recoveryTime := first.LeaseExpiresAt.Add(time.Millisecond)
		recovered, err := store.ClaimNextAction(t.Context(), models.ActionClaim{
			Scope: first.Scope, WorkerID: "agent-worker", Now: recoveryTime, LeaseDuration: time.Minute, OwnActionID: first.ID,
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
		again, err := store.ClaimNextAction(t.Context(), models.ActionClaim{
			Scope: first.Scope, WorkerID: "agent-worker", Now: deferred.AvailableAt, LeaseDuration: time.Minute, OwnActionID: first.ID,
		})
		if err != nil || again == nil || !again.RecoveredRunning || again.Attempt != 3 {
			t.Fatalf("Ready deferral erased prior Running recovery = %#v, %v", again, err)
		}
	})
}

func TestActionReadyDeferralIsNotRunningRecovery(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store models.KernelStore) {
		now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
		_, proposal := createRunnableAction(t, store, now)
		claimed, err := store.ClaimNextAction(t.Context(), models.ActionClaim{
			Scope: proposal.Call.Scope, WorkerID: "agent-worker", Now: now.Add(2 * time.Second), LeaseDuration: time.Minute, OwnActionID: proposal.Call.ID,
		})
		if err != nil || claimed == nil {
			t.Fatalf("fresh claim = %#v, %v", claimed, err)
		}
		for index := 0; index < 2; index++ {
			// Mirror the Ready transition used when a worker sees a maintenance
			// or pause race before dispatch. Multiple claims alone are harmless.
			deferred := deferClaimedActionForRecoveryTest(t, store, claimed, now.Add(time.Duration(3+index)*time.Second))
			claimed, err = store.ClaimNextAction(t.Context(), models.ActionClaim{
				Scope: claimed.Scope, WorkerID: "agent-worker", Now: deferred.AvailableAt, LeaseDuration: time.Minute, OwnActionID: claimed.ID,
			})
			if err != nil || claimed == nil || claimed.RecoveredRunning || claimed.Attempt != index+2 {
				t.Fatalf("Ready deferral incorrectly marked Running recovery = %#v, %v", claimed, err)
			}
		}
	})
}

func TestActionRunningRecoveryProvenanceSurvivesSQLiteRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "action-recovery.db")
	store, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	_, proposal := createRunnableAction(t, store, now)
	first, err := store.ClaimNextAction(t.Context(), models.ActionClaim{
		Scope: proposal.Call.Scope, WorkerID: "agent-worker", Now: now.Add(2 * time.Second), LeaseDuration: time.Minute, OwnActionID: proposal.Call.ID,
	})
	if err != nil || first == nil {
		t.Fatalf("fresh claim = %#v, %v", first, err)
	}
	recoveryTime := first.LeaseExpiresAt.Add(time.Millisecond)
	recovered, err := store.ClaimNextAction(t.Context(), models.ActionClaim{
		Scope: first.Scope, WorkerID: "agent-worker", Now: recoveryTime, LeaseDuration: time.Minute, OwnActionID: first.ID,
	})
	if err != nil || recovered == nil || !recovered.RecoveredRunning {
		t.Fatalf("recovered claim = %#v, %v", recovered, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, err := reopened.GetActionCall(t.Context(), first.Scope, first.ID)
	if err != nil || stored == nil || !stored.RecoveredRunning {
		t.Fatalf("restart lost recovery provenance = %#v, %v", stored, err)
	}
	deferred := deferClaimedActionForRecoveryTest(t, reopened, stored, recoveryTime.Add(time.Second))
	again, err := reopened.ClaimNextAction(t.Context(), models.ActionClaim{
		Scope: first.Scope, WorkerID: "agent-worker", Now: deferred.AvailableAt, LeaseDuration: time.Minute, OwnActionID: first.ID,
	})
	if err != nil || again == nil || !again.RecoveredRunning {
		t.Fatalf("post-restart Ready deferral lost recovery provenance = %#v, %v", again, err)
	}
}

func deferClaimedActionForRecoveryTest(t *testing.T, store models.KernelStore, claimed *models.ActionCall, now time.Time) *models.ActionCall {
	t.Helper()
	parent := getActionLifecycleRun(t, store, claimed)
	deferred := models.CloneActionCall(claimed)
	deferred.Status = models.ActionCallStatusReady
	deferred.LeaseOwner, deferred.LeaseExpiresAt = "", nil
	deferred.AvailableAt = now
	deferred.UpdatedAt = now
	deferred.Revision++
	result, err := store.PersistActionExecution(t.Context(), models.ActionExecutionRecord{
		Call: deferred, ExpectedCallRevision: claimed.Revision, ExpectedRunRevision: parent.Revision,
		WorkerID: claimed.LeaseOwner, Now: now,
		Event: actionLifecycleEvent(deferred, fmt.Sprintf("deferred-%d", deferred.Revision), "action.deferred", now),
	})
	if err != nil || result == nil || result.Call == nil {
		t.Fatalf("persist Ready deferral = %#v, %v", result, err)
	}
	return result.Call
}
