package runtime

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestMemoryActionProposalIsAtomicAndIdempotent(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	scope := Scope{Kind: "local", ID: "test"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "deploy", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	const contenders = 20
	results := make(chan *ActionProposalResult, contenders)
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			callID := fmt.Sprintf("call-%d", i)
			proposalRun := cloneAgentRun(run)
			proposalRun.Status = AgentRunStatusWaitingForApproval
			proposalRun.WakeCondition = &WakeCondition{Type: "approval", Reference: callID}
			proposalRun.Revision++
			proposalRun.UpdatedAt = now
			approval := &ApprovalCheckpoint{
				ID: "approval-" + callID, Scope: scope, RunID: run.ID, ActionCallID: callID,
				Status: ApprovalStatusPending, Risk: skill.RiskLevelProduction, Summary: "Deploy",
				ProposedAction:    map[string]interface{}{"environment": "production"},
				EligibleApprovers: []ApprovalPrincipal{{Type: "role", ID: "release-manager"}},
				ExpiresAt:         now.Add(time.Hour), Revision: 1, CreatedAt: now, UpdatedAt: now,
			}
			call := &ActionCall{
				ID: callID, Scope: scope, RunID: run.ID, DeploymentID: "agent", SkillID: "release", SkillVersion: "1",
				Action: "deploy", Status: ActionCallStatusWaitingApproval, Risk: skill.RiskLevelProduction,
				SideEffect: skill.SideEffectExternal, IdempotencyKey: "deploy-production", ApprovalID: approval.ID,
				MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
			}
			call.InvocationDigest = ComputeActionInvocationDigest(call)
			result, proposalErr := store.CreateActionProposal(ctx, ActionProposalRecord{
				Call: call, Approval: approval, Run: proposalRun, ExpectedRunRevision: run.Revision,
				Event: &ActivityEvent{ID: "event-" + callID, Scope: scope, RunID: run.ID, EventType: "action.approval_requested", Summary: "Approval requested", CreatedAt: now},
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
	calls, err := store.ListActionCalls(ctx, ActionFilter{Scope: scope, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	approvals, err := store.ListApprovals(ctx, ApprovalFilter{Scope: scope, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: run.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || len(approvals) != 1 || len(events) != 1 || calls[0].ID != callID {
		t.Fatalf("proposal was not atomic: calls=%#v approvals=%#v events=%#v", calls, approvals, events)
	}
	owned, err := store.ListApprovals(ctx, ApprovalFilter{Scope: scope, Owner: &ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}})
	if err != nil || len(owned) != 1 {
		t.Fatalf("owner approvals = %#v, err = %v", owned, err)
	}
	other, err := store.ListApprovals(ctx, ApprovalFilter{Scope: scope, Owner: &ObjectiveOwner{Type: OwnerTypeTeam, ID: "other"}})
	if err != nil || len(other) != 0 {
		t.Fatalf("other owner approvals = %#v, err = %v", other, err)
	}
	if _, err := store.GetActionCall(ctx, scope, "missing"); err != ErrActionNotFound {
		t.Fatalf("missing action error = %v, want %v", err, ErrActionNotFound)
	}
	if _, err := store.GetApproval(ctx, scope, "missing"); err != ErrApprovalNotFound {
		t.Fatalf("missing approval error = %v, want %v", err, ErrApprovalNotFound)
	}
}

func TestMemoryApprovalPaginationUsesIDToBreakTimestampTies(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "local", ID: "review-pagination"}
	now := time.Now().UTC()
	for _, id := range []string{"approval-c", "approval-a", "approval-b"} {
		store.approvals[portfolioKey(scope, id)] = &ApprovalCheckpoint{ID: id, Scope: scope, Status: ApprovalStatusPending, CreatedAt: now}
	}
	for _, newest := range []bool{false, true} {
		for i := 0; i < 20; i++ {
			page, err := store.ListApprovals(t.Context(), ApprovalFilter{Scope: scope, Status: []ApprovalStatus{ApprovalStatusPending}, NewestFirst: newest, Limit: 1, Offset: 1})
			if err != nil || len(page) != 1 || page[0].ID != "approval-b" {
				t.Fatalf("unstable page = %#v, %v", page, err)
			}
			first, err := store.ListApprovals(t.Context(), ApprovalFilter{Scope: scope, NewestFirst: newest, Limit: 1})
			want := "approval-a"
			if newest {
				want = "approval-c"
			}
			if err != nil || len(first) != 1 || first[0].ID != want {
				t.Fatalf("wrong first page = %#v, %v", first, err)
			}
		}
	}
}
