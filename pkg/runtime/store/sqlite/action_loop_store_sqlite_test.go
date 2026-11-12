package sqlite

import (
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

// Store test: which loop calls the SQLite store's claim selects, seeded with
// the SQLite row fixtures, so this file moves to store/sqlite with
// action_sqlite.go and sqlite_fixture_test.go (U2.9).

func TestClaimSelectsLoopCalls(t *testing.T) {
	now := time.Now().UTC()
	scope := models.Scope{Kind: "tenant", ID: "claim-selects"}
	claimable := func(t *testing.T, claim models.ActionClaim, call *models.ActionCall, run *models.AgentRun) bool {
		t.Helper()
		store := newTestKernelStore(t)
		if run != nil {
			run.ID, run.Scope, run.RootRunID, run.CreatedAt, run.UpdatedAt = "run", scope, "run", now, now
			putStoredRun(t, store, run)
		}
		call.Scope, call.RunID, call.Revision, call.CreatedAt, call.UpdatedAt = scope, "run", 1, now, now
		putStoredAction(t, store, call)
		claim.Scope, claim.WorkerID, claim.LeaseDuration = scope, "worker", time.Minute
		claimed, err := store.ClaimNextAction(t.Context(), claim)
		if err != nil {
			t.Fatal(err)
		}
		return claimed != nil && claimed.ID == call.ID
	}
	loopCall := func() *models.ActionCall {
		return &models.ActionCall{ID: "call", Execution: models.ActionExecutionLoop, Status: models.ActionCallStatusReady, AvailableAt: now}
	}
	claim := models.ActionClaim{Now: now}
	for status, want := range map[models.AgentRunStatus]bool{
		models.AgentRunStatusQueued: false, models.AgentRunStatusRunning: false, models.AgentRunStatusPaused: false, models.AgentRunStatusSleeping: false,
		models.AgentRunStatusWaitingForApproval: false, models.AgentRunStatusWaitingForEvent: false, models.AgentRunStatusWaitingForDependency: false,
		models.AgentRunStatusCompleted: true, models.AgentRunStatusFailed: true, models.AgentRunStatusCanceled: true,
	} {
		if got := claimable(t, claim, loopCall(), &models.AgentRun{Status: status}); got != want {
			t.Errorf("pool claim of a loop call with Run %s = %v, want %v", status, got, want)
		}
	}
	if !claimable(t, claim, loopCall(), nil) {
		t.Error("pool claim of a loop call without its Run must close it")
	}
	own := models.ActionClaim{Now: now, OwnActionID: "call"}
	if !claimable(t, own, loopCall(), &models.AgentRun{Status: models.AgentRunStatusRunning}) {
		t.Error("own claim of a loop call was refused")
	}
	if claimable(t, own, &models.ActionCall{ID: "call", Status: models.ActionCallStatusReady, AvailableAt: now}, nil) {
		t.Error("own claim took a worker-executed call")
	}
	if !claimable(t, claim, &models.ActionCall{ID: "worker", Status: models.ActionCallStatusReady, AvailableAt: now}, &models.AgentRun{Status: models.AgentRunStatusRunning}) {
		t.Error("pool claim of a worker-executed call was refused")
	}
}
