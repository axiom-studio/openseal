package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestActionCallDurableJSONPreservesEmptyArgumentObject(t *testing.T) {
	call := ActionCall{Arguments: map[string]interface{}{}}
	payload, err := json.Marshal(call)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ActionCall
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Arguments == nil {
		t.Fatalf("empty action arguments were lost from durable JSON: %s", payload)
	}
}

func TestSQLiteActionProposalIsAtomicIdempotentAndDurable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "actions.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "one"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "release-team"}, Goal: "deploy", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	const contenders = 20
	results := make(chan *ActionProposalResult, contenders)
	errs := make(chan error, contenders)
	var wg sync.WaitGroup
	for i := 0; i < contenders; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			result, err := store.CreateActionProposal(ctx, sqliteApprovalProposal(run, fmt.Sprintf("call-%d", index), "deploy-production", "event-"+fmt.Sprint(index)))
			results <- result
			errs <- err
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
	var persistedID string
	for result := range results {
		if result.Created {
			created++
		}
		persistedID = result.Call.ID
	}
	if created != 1 {
		t.Fatalf("created proposals = %d, want 1", created)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
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
	persistedRun, err := store.GetAgentRun(ctx, scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].ID != persistedID || len(approvals) != 1 || len(events) != 1 || persistedRun.Revision != run.Revision+1 || persistedRun.Status != AgentRunStatusWaitingForApproval {
		t.Fatalf("durable proposal mismatch: calls=%#v approvals=%#v events=%#v run=%#v", calls, approvals, events, persistedRun)
	}
	owned, err := store.ListApprovals(ctx, ApprovalFilter{Scope: scope, Owner: &ObjectiveOwner{Type: OwnerTypeTeam, ID: "release-team"}})
	if err != nil || len(owned) != 1 {
		t.Fatalf("owner approvals = %#v, err = %v", owned, err)
	}
	other, err := store.ListApprovals(ctx, ApprovalFilter{Scope: scope, Owner: &ObjectiveOwner{Type: OwnerTypeAgent, ID: "other"}})
	if err != nil || len(other) != 0 {
		t.Fatalf("other owner approvals = %#v, err = %v", other, err)
	}
	if _, err := store.GetActionCall(ctx, Scope{Kind: "tenant", ID: "other"}, persistedID); err != ErrActionNotFound {
		t.Fatalf("cross-scope action lookup error = %v", err)
	}
	conflict := sqliteApprovalProposal(persistedRun, "different-call", "deploy-production", "different-event")
	conflict.Call.Arguments["environment"] = "staging"
	conflict.Call.InvocationDigest = ComputeActionInvocationDigest(conflict.Call)
	if _, err := store.CreateActionProposal(ctx, conflict); err != ErrIdempotencyConflict {
		t.Fatalf("idempotency collision error = %v, want %v", err, ErrIdempotencyConflict)
	}
}

func TestSQLiteActionProposalRollsBackEveryRecordOnActivityFailure(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "rollback.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := Scope{Kind: "local", ID: "rollback"}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "deploy", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendActivity(ctx, &ActivityEvent{ID: "duplicate-event", Scope: scope, RunID: run.ID, EventType: "test", Summary: "existing", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	proposal := sqliteApprovalProposal(run, "rolled-back-call", "rollback-key", "duplicate-event")
	if _, err := store.CreateActionProposal(ctx, proposal); err == nil {
		t.Fatal("proposal should fail on duplicate activity id")
	}
	if _, err := store.GetActionCall(ctx, scope, "rolled-back-call"); err != ErrActionNotFound {
		t.Fatalf("action survived rollback: %v", err)
	}
	if _, err := store.GetApproval(ctx, scope, "approval-rolled-back-call"); err != ErrApprovalNotFound {
		t.Fatalf("approval survived rollback: %v", err)
	}
	persisted, err := store.GetAgentRun(ctx, scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Revision != run.Revision || persisted.Status != run.Status {
		t.Fatalf("run survived partial proposal: %#v", persisted)
	}
}

func TestSQLiteExternalOperationReceiptSurvivesRestartAndRejectsConcurrentClaim(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "external-receipts.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "one"}
	firstRun, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"}, Goal: "comment once", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := hashString("stable external operation")
	first := sqliteApprovalProposal(firstRun, "first-external-call", "first", "first-external-event")
	first.Call.ExternalOperationDigest = digest
	if _, err := store.CreateActionProposal(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	receipt, err := store.GetActionCallByExternalOperation(ctx, scope, digest)
	if err != nil || receipt.ID != first.Call.ID {
		t.Fatalf("restarted receipt = %#v, %v", receipt, err)
	}
	secondRun, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"}, Goal: "comment once again", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	second := sqliteApprovalProposal(secondRun, "second-external-call", "second", "second-external-event")
	second.Call.ExternalOperationDigest = digest
	if _, err := store.CreateActionProposal(ctx, second); !errors.Is(err, ErrExternalOperationClaimed) {
		t.Fatalf("second external claim error = %v", err)
	}
	if _, err := store.GetActionCall(ctx, scope, second.Call.ID); err != ErrActionNotFound {
		t.Fatalf("conflicting external call was persisted: %v", err)
	}
}

func sqliteApprovalProposal(run *AgentRun, callID, idempotencyKey, eventID string) ActionProposalRecord {
	now := time.Now().UTC()
	updatedRun := cloneAgentRun(run)
	updatedRun.Status = AgentRunStatusWaitingForApproval
	updatedRun.WakeCondition = &WakeCondition{Type: "approval", Reference: callID}
	updatedRun.Revision++
	updatedRun.UpdatedAt = now
	approval := &ApprovalCheckpoint{
		ID: "approval-" + callID, Scope: run.Scope, RunID: run.ID, ActionCallID: callID,
		Status: ApprovalStatusPending, Risk: skill.RiskLevelProduction, Summary: "Deploy to production",
		ProposedAction:    map[string]interface{}{"skill": "release", "action": "deploy", "environment": "production"},
		EligibleApprovers: []ApprovalPrincipal{{Type: "role", ID: "release-manager"}}, ExpiresAt: now.Add(time.Hour),
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	call := &ActionCall{
		ID: callID, Scope: run.Scope, RunID: run.ID, DeploymentID: "release-agent", SkillID: "release", SkillVersion: "1.0.0",
		Action: "deploy", Status: ActionCallStatusWaitingApproval, Risk: skill.RiskLevelProduction, SideEffect: skill.SideEffectExternal,
		Arguments: map[string]interface{}{"environment": "production"}, CredentialRefs: map[string]skill.CredentialReference{"token": {Kind: "api-token", ID: "release-token"}},
		IdempotencyKey: idempotencyKey, ApprovalID: approval.ID, MaxAttempts: 1, AvailableAt: now,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	call.InvocationDigest = ComputeActionInvocationDigest(call)
	return ActionProposalRecord{
		Call: call, Approval: approval, Run: updatedRun, ExpectedRunRevision: run.Revision,
		Event: &ActivityEvent{ID: eventID, Scope: run.Scope, RunID: run.ID, EventType: "action.approval_requested", Summary: "Approval requested for release.deploy", Payload: map[string]interface{}{"actionCallId": callID, "skill": "release", "action": "deploy"}, CreatedAt: now},
	}
}
