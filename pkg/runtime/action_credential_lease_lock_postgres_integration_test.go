//go:build integration

package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Hold the owner Run while redemption and submission acquire their authority
// locks. Releasing it reproduces the former binding/Run lock inversion: one
// transaction held the Run and needed an exclusive binding lock while the
// other held its binding lock and needed the same Run.
func TestPostgresActionCredentialLeaseSerializesWithProposalWithoutDeadlock(t *testing.T) {
	ctx, primary, replica := newActionCredentialLeasePostgresStores(t)
	request, _ := createPostgresActionCredentialLeaseFixture(t, ctx, primary, "nonce-proposal-lock-order-0001")
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	call, err := primary.GetActionCall(ctx, request.Lease.Scope, request.Lease.ActionCallID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := primary.GetAgentRun(ctx, call.Scope, call.RunID)
	if err != nil {
		t.Fatal(err)
	}
	blocker, err := primary.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	var revision int64
	if err := blocker.QueryRowContext(ctx, `SELECT revision FROM `+primary.table("agent_runs")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 FOR UPDATE`, run.Scope.Kind, run.Scope.ID, run.ID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	redeemed := make(chan error, 1)
	go func() { redeemed <- replica.RedeemActionCredentialLease(ctx, request) }()
	waitForBlockedPostgresActionRunQueries(t, ctx, primary, 1)

	now := time.Now().UTC()
	proposedCall := cloneActionCall(call)
	proposedCall.ID = uuid.NewString()
	proposedCall.Status = ActionCallStatusReady
	proposedCall.Revision = 1
	proposedCall.Attempt = 0
	proposedCall.LeaseOwner = ""
	proposedCall.LeaseExpiresAt = nil
	proposedCall.Arguments = map[string]interface{}{}
	proposedCall.CreatedAt, proposedCall.UpdatedAt, proposedCall.AvailableAt = now, now, now
	proposedCall.IdempotencyKey = "concurrent-proposal"
	proposedCall.InvocationDigest = ComputeActionInvocationDigest(proposedCall)
	proposedRun := cloneAgentRun(run)
	proposedRun.Status = AgentRunStatusWaitingForDependency
	proposedRun.WakeCondition = &WakeCondition{Type: "action", Reference: proposedCall.ID}
	proposedRun.Revision++
	proposedRun.UpdatedAt = now
	submitted := make(chan error, 1)
	go func() {
		_, err := primary.CreateActionProposal(ctx, ActionProposalRecord{
			Call: proposedCall, Run: proposedRun, ExpectedRunRevision: run.Revision, RequireBindingFence: true,
			Event: &ActivityEvent{ID: uuid.NewString(), Scope: run.Scope, RunID: run.ID, EventType: "action.proposed", Summary: "Concurrent proposal", CreatedAt: now},
		})
		submitted <- err
	}()
	waitForBlockedPostgresActionRunQueries(t, ctx, primary, 2)
	if err := blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-redeemed; err != nil {
		t.Fatalf("credential redemption failed under submission contention: %v", err)
	}
	if err := <-submitted; err != nil {
		t.Fatalf("proposal failed under redemption contention: %v", err)
	}
	storedCall, err := primary.GetActionCall(ctx, proposedCall.Scope, proposedCall.ID)
	if err != nil || storedCall.Status != ActionCallStatusReady {
		t.Fatalf("concurrent proposal did not commit: %#v %v", storedCall, err)
	}
}

func waitForBlockedPostgresActionRunQueries(t *testing.T, ctx context.Context, store *PostgresStore, minimum int) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked int
		if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND strpos(query,$1)>0 AND strpos(query,'FOR UPDATE')>0 AND pid<>pg_backend_pid()`, store.table("agent_runs")).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked >= minimum {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("only %d/%d transactions reached the Run lock: %v", blocked, minimum, ctx.Err())
		case <-ticker.C:
		}
	}
}
