package runtime

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestActionProposalIsAtomicAndIdempotent(t *testing.T) {
	store := newTestKernelStore(t)
	ctx := context.Background()
	scope := models.Scope{Kind: "local", ID: "test"}
	run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Goal: "deploy", Source: models.RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	run = claimRunForLoopProposal(t, store, run)
	now := time.Now()
	const contenders = 20
	results := make(chan *models.ActionProposalResult, contenders)
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			callID := fmt.Sprintf("call-%d", i)
			proposalRun := models.CloneAgentRun(run)
			// A loop call never parks its Run; the Run update stands in for
			// its budget reservation, which must commit atomically with it.
			proposalRun.Revision++
			proposalRun.UpdatedAt = now
			approval := &models.ApprovalCheckpoint{
				ID: "approval-" + callID, Scope: scope, RunID: run.ID, ActionCallID: callID,
				Status: models.ApprovalStatusPending, Risk: skill.RiskLevelProduction, Summary: "Deploy",
				ProposedAction:    map[string]interface{}{"environment": "production"},
				EligibleApprovers: []models.ApprovalPrincipal{{Type: "role", ID: "release-manager"}},
				ExpiresAt:         now.Add(time.Hour), Revision: 1, CreatedAt: now, UpdatedAt: now,
			}
			call := &models.ActionCall{
				ID: callID, Scope: scope, RunID: run.ID, TurnID: "step-1", DeploymentID: "agent", SkillID: "release", SkillVersion: "1",
				Action: "deploy", Origin: models.ActionOriginLoop, Execution: models.ActionExecutionLoop,
				Status: models.ActionCallStatusWaitingApproval, Risk: skill.RiskLevelProduction,
				SideEffect: skill.SideEffectExternal, IdempotencyKey: "deploy-production", ApprovalID: approval.ID,
				MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
			}
			call.InvocationDigest = models.ComputeActionInvocationDigest(call)
			result, proposalErr := store.CreateActionProposal(ctx, models.ActionProposalRecord{
				Call: call, Approval: approval, Run: proposalRun, ExpectedRunRevision: run.Revision,
				Lease: &models.AgentRunLeaseGuard{WorkerID: "loop-worker", Now: now},
				Event: &models.ActivityEvent{ID: "event-" + callID, Scope: scope, RunID: run.ID, EventType: "action.approval_requested", Summary: "Approval requested", CreatedAt: now},
			})
			results <- result
			errs <- proposalErr
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	created := 0
	var callID string
	for result := range results {
		if result.Created {
			created++
		}
		callID = result.Call.ID
	}
	if created != 1 {
		t.Fatalf("created proposals = %d, want 1", created)
	}
	calls, err := store.ListActionCalls(ctx, models.ActionFilter{Scope: scope, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	approvals, err := store.ListApprovals(ctx, models.ApprovalFilter{Scope: scope, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ListActivity(ctx, models.ActivityFilter{Scope: scope, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || len(approvals) != 1 || len(events) != 1 || calls[0].ID != callID {
		t.Fatalf("proposal was not atomic: calls=%#v approvals=%#v events=%#v", calls, approvals, events)
	}
	owned, err := store.ListApprovals(ctx, models.ApprovalFilter{Scope: scope, Owner: &models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}})
	if err != nil || len(owned) != 1 {
		t.Fatalf("owner approvals = %#v, err = %v", owned, err)
	}
	other, err := store.ListApprovals(ctx, models.ApprovalFilter{Scope: scope, Owner: &models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "other"}})
	if err != nil || len(other) != 0 {
		t.Fatalf("other owner approvals = %#v, err = %v", other, err)
	}
	if _, err := store.GetActionCall(ctx, scope, "missing"); err != models.ErrActionNotFound {
		t.Fatalf("missing action error = %v, want %v", err, models.ErrActionNotFound)
	}
	if _, err := store.GetApproval(ctx, scope, "missing"); err != models.ErrApprovalNotFound {
		t.Fatalf("missing approval error = %v, want %v", err, models.ErrApprovalNotFound)
	}
}
