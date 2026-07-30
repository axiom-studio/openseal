package runtime

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type approvalNotificationConflictStore struct {
	ApprovalNotificationStore
	remaining atomic.Int32
}

func (s *approvalNotificationConflictStore) CommitChannelMessage(ctx context.Context, record ChannelMessageCommitRecord) (*ChannelMessageCommitResult, error) {
	if s.remaining.Add(-1) >= 0 {
		return nil, ErrRevisionConflict
	}
	return s.ApprovalNotificationStore.CommitChannelMessage(ctx, record)
}

func TestApprovalNotificationDeliversOnceAndSignedDecisionResolvesCanonicalCheckpoint(t *testing.T) {
	ctx := t.Context()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: endpoint.Scope, Owner: endpoint.Owner, AssignedAgentID: endpoint.DeploymentID,
		Goal: "post reviewed comment", Source: RunSourceObjective, Budget: &BudgetPolicy{MaxActions: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: endpoint.Scope, WorkerID: "worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	call := &ActionCall{
		ID: "call-1", Scope: endpoint.Scope, RunID: claimed.ID, DeploymentID: endpoint.DeploymentID,
		SkillID: "browser", SkillVersion: "1.0.0", Action: "comment", Status: ActionCallStatusWaitingApproval,
		Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal, Arguments: map[string]interface{}{"comment": "Useful context"},
		ApprovalID: "approval-1", MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	call.InvocationDigest = ComputeActionInvocationDigest(call)
	call.SemanticDigest = ComputeActionSemanticDigest(call)
	approval := &ApprovalCheckpoint{
		ID: "approval-1", Scope: endpoint.Scope, RunID: claimed.ID, ActionCallID: call.ID, Status: ApprovalStatusPending,
		Risk: skill.RiskLevelExternal, Summary: "Post reviewed comment", PolicyReason: "external write",
		ProposedAction: map[string]interface{}{"comment": "Useful context"}, EligibleApprovers: []ApprovalPrincipal{{Type: "role", ID: "operator"}},
		Destinations: []ApprovalDestination{{EndpointID: endpoint.ID}}, ExpiresAt: now.Add(time.Hour), Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	waiting := cloneAgentRun(claimed)
	waiting.Status = AgentRunStatusWaitingForApproval
	waiting.WakeCondition = &WakeCondition{Type: "approval", Reference: approval.ID}
	waiting.LeaseOwner = ""
	waiting.LeaseExpiresAt = nil
	waiting.Revision++
	waiting.UpdatedAt = now
	event := &ActivityEvent{ID: "approval-requested", Scope: endpoint.Scope, EventType: "action.approval_requested", Severity: ActivitySeverityInfo,
		AgentID: endpoint.DeploymentID, RunID: claimed.ID, Actor: ActivityActor{Type: "worker", ID: "worker"}, Summary: approval.Summary,
		Visibility: ActivityVisibilityScope, CreatedAt: now}
	if _, err := store.CreateActionProposal(ctx, ActionProposalRecord{Call: call, Approval: approval, Run: waiting, Event: event, ExpectedRunRevision: claimed.Revision, Lease: &AgentRunLeaseGuard{WorkerID: "worker", Now: now}}); err != nil {
		t.Fatal(err)
	}

	transport := NewExternalConversationTransportService(store, catalog)
	worker := NewApprovalNotificationWorker(store, transport)
	if count, err := worker.ProcessScope(ctx, endpoint.Scope, 10); err != nil || count != 1 {
		t.Fatalf("notify = %d, %v", count, err)
	}
	if count, err := worker.ProcessScope(ctx, endpoint.Scope, 10); err != nil || count != 1 {
		t.Fatalf("replay notify = %d, %v", count, err)
	}
	deliveries, err := store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{Scope: endpoint.Scope, EndpointID: endpoint.ID, Limit: 10})
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("deliveries = %#v, %v", deliveries, err)
	}
	card := deliveries[0].Parameters["approval"].(map[string]interface{})
	if card["id"] != approval.ID || card["invocationDigest"] != call.InvocationDigest {
		t.Fatalf("card = %#v", card)
	}

	decision := NormalizedExternalConversationEvent{
		ID: "slack:approval:T1:1720000000.1:U1", Type: capability.ConversationEventApprovalDecided,
		ExternalConversationID: endpoint.Address, ExternalMessageID: "1720000000.0", ExternalParticipantID: "U1",
		OrderingKey: endpoint.Address + ":1720000000.0", OccurredAt: now,
		Attributes: map[string]interface{}{"approvalId": approval.ID, "approvalRevision": int64(1), "actionCallId": call.ID,
			"invocationDigest": call.InvocationDigest, "decision": "approve", "principalType": "role", "principalId": "operator"},
	}
	tampered := decision
	tampered.ID = "slack:approval:T1:1720000000.2:U1"
	tampered.Attributes = cloneMap(decision.Attributes)
	tampered.Attributes["invocationDigest"] = "edited-after-review"
	if _, err := transport.Receive(ctx, ReceiveExternalConversationEventRequest{Scope: endpoint.Scope, EndpointID: endpoint.ID, Event: tampered}); err == nil {
		t.Fatal("tampered approval decision was accepted")
	}
	unauthorized := decision
	unauthorized.ID = "slack:approval:T1:1720000000.3:U2"
	unauthorized.ExternalParticipantID = "U2"
	unauthorized.Attributes = cloneMap(decision.Attributes)
	unauthorized.Attributes["principalId"] = "viewer"
	if _, err := transport.Receive(ctx, ReceiveExternalConversationEventRequest{Scope: endpoint.Scope, EndpointID: endpoint.ID, Event: unauthorized}); err == nil {
		t.Fatal("ineligible approval principal was accepted")
	}
	pending, err := store.GetApproval(ctx, endpoint.Scope, approval.ID)
	if err != nil || pending.Status != ApprovalStatusPending {
		t.Fatalf("approval after rejected decisions = %#v, %v", pending, err)
	}
	resolved, err := transport.Receive(ctx, ReceiveExternalConversationEventRequest{Scope: endpoint.Scope, EndpointID: endpoint.ID, Event: decision})
	if err != nil || resolved.Approval == nil || !resolved.Approval.Resolved || resolved.Approval.Approval.Status != ApprovalStatusApproved || resolved.Approval.Call.Status != ActionCallStatusReady {
		t.Fatalf("resolution = %#v, %v", resolved, err)
	}
	replay, err := transport.Receive(ctx, ReceiveExternalConversationEventRequest{Scope: endpoint.Scope, EndpointID: endpoint.ID, Event: decision})
	if err != nil || replay.Approval == nil || !replay.Replayed {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
}

func TestApprovalNotificationConvergesAcrossRevisionConflictsAndConcurrentPasses(t *testing.T) {
	ctx := t.Context()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: endpoint.Scope, Owner: endpoint.Owner, AssignedAgentID: endpoint.DeploymentID,
		Goal: "post reviewed comment", Source: RunSourceObjective, Budget: &BudgetPolicy{MaxActions: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: endpoint.Scope, WorkerID: "worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	call := &ActionCall{
		ID: "call-conflict", Scope: endpoint.Scope, RunID: claimed.ID, DeploymentID: endpoint.DeploymentID,
		SkillID: "browser", SkillVersion: "1.0.0", Action: "comment", Status: ActionCallStatusWaitingApproval,
		Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal, Arguments: map[string]interface{}{"comment": "Useful context"},
		ApprovalID: "approval-conflict", MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	call.InvocationDigest = ComputeActionInvocationDigest(call)
	call.SemanticDigest = ComputeActionSemanticDigest(call)
	approval := &ApprovalCheckpoint{
		ID: "approval-conflict", Scope: endpoint.Scope, RunID: claimed.ID, ActionCallID: call.ID, Status: ApprovalStatusPending,
		Risk: skill.RiskLevelExternal, Summary: "Post reviewed comment", PolicyReason: "external write",
		ProposedAction: map[string]interface{}{"comment": "Useful context"}, EligibleApprovers: []ApprovalPrincipal{{Type: "role", ID: "operator"}},
		Destinations: []ApprovalDestination{{EndpointID: endpoint.ID}}, ExpiresAt: now.Add(time.Hour), Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	waiting := cloneAgentRun(claimed)
	waiting.Status = AgentRunStatusWaitingForApproval
	waiting.WakeCondition = &WakeCondition{Type: "approval", Reference: approval.ID}
	waiting.LeaseOwner = ""
	waiting.LeaseExpiresAt = nil
	waiting.Revision++
	waiting.UpdatedAt = now
	event := &ActivityEvent{ID: "approval-conflict-requested", Scope: endpoint.Scope, EventType: "action.approval_requested", Severity: ActivitySeverityInfo,
		AgentID: endpoint.DeploymentID, RunID: claimed.ID, Actor: ActivityActor{Type: "worker", ID: "worker"}, Summary: approval.Summary,
		Visibility: ActivityVisibilityScope, CreatedAt: now}
	if _, err := store.CreateActionProposal(ctx, ActionProposalRecord{Call: call, Approval: approval, Run: waiting, Event: event, ExpectedRunRevision: claimed.Revision, Lease: &AgentRunLeaseGuard{WorkerID: "worker", Now: now}}); err != nil {
		t.Fatal(err)
	}

	conflicts := &approvalNotificationConflictStore{ApprovalNotificationStore: store}
	conflicts.remaining.Store(2)
	worker := NewApprovalNotificationWorker(conflicts, NewExternalConversationTransportService(store, catalog))
	var group sync.WaitGroup
	errorsByPass := make(chan error, 2)
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := worker.ProcessScope(ctx, endpoint.Scope, 10)
			errorsByPass <- err
		}()
	}
	group.Wait()
	close(errorsByPass)
	for err := range errorsByPass {
		if err != nil {
			t.Fatal(err)
		}
	}
	deliveries, err := store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{Scope: endpoint.Scope, EndpointID: endpoint.ID, Limit: 10})
	if err != nil || len(deliveries) != 1 {
		t.Fatalf("deliveries = %#v, %v", deliveries, err)
	}
}
