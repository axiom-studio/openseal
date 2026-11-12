package runtime

import (
	"testing"
	"time"
)

// Store test: which loop calls the SQLite store's claim selects, seeded with
// the SQLite row fixtures, so this file moves to store/sqlite with
// action_sqlite.go and sqlite_fixture_test.go (U2.9).

func TestClaimSelectsLoopCalls(t *testing.T) {
	now := time.Now().UTC()
	scope := Scope{Kind: "tenant", ID: "claim-selects"}
	claimable := func(t *testing.T, claim ActionClaim, call *ActionCall, run *AgentRun) bool {
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
	loopCall := func() *ActionCall {
		return &ActionCall{ID: "call", Execution: ActionExecutionLoop, Status: ActionCallStatusReady, AvailableAt: now}
	}
	claim := ActionClaim{Now: now}
	for status, want := range map[AgentRunStatus]bool{
		AgentRunStatusQueued: false, AgentRunStatusRunning: false, AgentRunStatusPaused: false, AgentRunStatusSleeping: false,
		AgentRunStatusWaitingForApproval: false, AgentRunStatusWaitingForEvent: false, AgentRunStatusWaitingForDependency: false,
		AgentRunStatusCompleted: true, AgentRunStatusFailed: true, AgentRunStatusCanceled: true,
	} {
		if got := claimable(t, claim, loopCall(), &AgentRun{Status: status}); got != want {
			t.Errorf("pool claim of a loop call with Run %s = %v, want %v", status, got, want)
		}
	}
	if !claimable(t, claim, loopCall(), nil) {
		t.Error("pool claim of a loop call without its Run must close it")
	}
	own := ActionClaim{Now: now, OwnActionID: "call"}
	if !claimable(t, own, loopCall(), &AgentRun{Status: AgentRunStatusRunning}) {
		t.Error("own claim of a loop call was refused")
	}
	if claimable(t, own, &ActionCall{ID: "call", Status: ActionCallStatusReady, AvailableAt: now}, nil) {
		t.Error("own claim took a worker-executed call")
	}
	if !claimable(t, claim, &ActionCall{ID: "worker", Status: ActionCallStatusReady, AvailableAt: now}, &AgentRun{Status: AgentRunStatusRunning}) {
		t.Error("pool claim of a worker-executed call was refused")
	}
}
