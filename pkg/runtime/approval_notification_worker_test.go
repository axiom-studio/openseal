package runtime

import (
	"context"
	"strings"
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
	store, catalog, endpoint := externalConversationDeliveryFixtureWithOperations(t, ctx, "slack", []skill.ConversationDeliveryOperation{
		skill.ConversationDeliveryMessageSend, skill.ConversationDeliveryMessageUpdate,
	})
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: endpoint.Scope, Owner: endpoint.Owner, AssignedAgentID: endpoint.DeploymentID,
		Goal: "post reviewed comment", Source: RunSourceObjective,
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
		Destinations: []ApprovalDestination{{EndpointID: endpoint.ID}}, ExpiresAt: now.Add(24 * time.Hour), Revision: 1, CreatedAt: now, UpdatedAt: now,
		ContinuationCheckpoint: map[string]interface{}{
			"state": map[string]interface{}{
				"commentDraft": "The exact proposed public comment.",
				"destination":  "https://forum.example/posts/42",
				"apiToken":     "must-never-leave-the-kernel",
			},
			"output": map[string]interface{}{
				"commentDraft": "stale duplicate must not replace state",
				"postURL":      "https://forum.example/posts/42#comment-7",
				"accessToken":  "must-also-remain-secret",
			},
		},
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
	// A tenant's immutable approval history must not starve current,
	// deadline-bearing work when the worker uses a bounded page.
	historyCall := &ActionCall{ID: "historical-call", Scope: endpoint.Scope, RunID: claimed.ID, Status: ActionCallStatusSucceeded, Revision: 1, CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}
	store.mu.Lock()
	store.actions[portfolioKey(endpoint.Scope, historyCall.ID)] = cloneActionCall(historyCall)
	for index := range 11 {
		historical := &ApprovalCheckpoint{
			ID: "historical-approval-" + string(rune('a'+index)), Scope: endpoint.Scope, RunID: claimed.ID,
			ActionCallID: historyCall.ID, Status: ApprovalStatusApproved, Revision: 1,
			CreatedAt: now.Add(-time.Hour).Add(time.Duration(index) * time.Second), UpdatedAt: now.Add(-time.Hour),
		}
		if index == 0 {
			historical.Destinations = []ApprovalDestination{{EndpointID: endpoint.ID}}
		}
		store.approvals[portfolioKey(endpoint.Scope, historical.ID)] = historical
	}
	store.mu.Unlock()

	transport := NewExternalConversationTransportService(store, catalog)
	transport.now = func() time.Time { return now }
	worker := NewApprovalNotificationWorker(store, transport)
	worker.now = func() time.Time { return now }
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
	review := card["proposedAction"].(map[string]interface{})["reviewContext"].(map[string]interface{})
	if review["commentDraft"] != "The exact proposed public comment." ||
		review["destination"] != "https://forum.example/posts/42" ||
		review["postURL"] != "https://forum.example/posts/42#comment-7" ||
		review["apiToken"] != nil ||
		review["accessToken"] != nil {
		t.Fatalf("review context = %#v", review)
	}
	original, err := store.ClaimExternalConversationDelivery(ctx, endpoint.Scope, "delivery-worker", now, time.Minute)
	if err != nil || original == nil {
		t.Fatalf("claim notification = %#v, %v", original, err)
	}
	delivered := cloneExternalConversationDelivery(original)
	delivered.Status, delivered.LeaseOwner, delivered.LeaseExpiresAt = ExternalConversationDeliveryDelivered, "", time.Time{}
	delivered.ProviderMessageID, delivered.DeliveredAt, delivered.UpdatedAt = "1720000000.123", now.Add(time.Second), now.Add(time.Second)
	delivered.Revision++
	if err = store.SaveExternalConversationDelivery(ctx, delivered, original.Revision, "delivery-worker"); err != nil {
		t.Fatal(err)
	}

	decision := NormalizedExternalConversationEvent{
		ID: "slack:approval:T1:1720000000.1:U1", Type: capability.ConversationEventApprovalDecided,
		ExternalConversationID: endpoint.Address, ExternalMessageID: "1720000000.0", ExternalParticipantID: "U1",
		OrderingKey: endpoint.Address + ":1720000000.0", OccurredAt: now,
		Attributes: map[string]interface{}{"approvalId": approval.ID, "approvalRevision": int64(1), "actionCallId": call.ID,
			"invocationDigest": call.InvocationDigest, "decision": "approve", "principalType": "role", "principalId": "operator", "providerUserId": "U1"},
	}
	registration := &CallbackRegistration{Scope: endpoint.Scope}
	subscription := CallbackSubscription{TargetID: endpoint.ID}
	callbackEvent := EventEnvelope{
		ID: decision.ID, Scope: endpoint.Scope, Type: capability.CallbackEventApprovalDecided,
		Source: "slack", Subject: decision.ExternalMessageID, OccurredAt: now,
		Attributes: cloneMap(decision.Attributes), Actor: ActivityActor{Type: "callback", ID: "slack-approval"},
	}
	consumer := NewApprovalCallbackConsumer(store, transport)
	tampered := callbackEvent
	tampered.ID = "slack:approval:T1:1720000000.2:U1"
	tampered.Attributes = cloneMap(callbackEvent.Attributes)
	tampered.Attributes["invocationDigest"] = "edited-after-review"
	if err := consumer.ConsumeCallbackEvent(ctx, registration, subscription, tampered); err == nil {
		t.Fatal("tampered approval decision was accepted")
	}
	unauthorized := callbackEvent
	unauthorized.ID = "slack:approval:T1:1720000000.3:U2"
	unauthorized.Attributes = cloneMap(callbackEvent.Attributes)
	unauthorized.Attributes["principalId"] = "viewer"
	if err := consumer.ConsumeCallbackEvent(ctx, registration, subscription, unauthorized); err == nil {
		t.Fatal("ineligible approval principal was accepted")
	}
	pending, err := store.GetApproval(ctx, endpoint.Scope, approval.ID)
	if err != nil || pending.Status != ApprovalStatusPending {
		t.Fatalf("approval after rejected decisions = %#v, %v", pending, err)
	}
	if err := consumer.ConsumeCallbackEvent(ctx, registration, subscription, callbackEvent); err != nil {
		t.Fatal(err)
	}
	resolvedApproval, err := store.GetApproval(ctx, endpoint.Scope, approval.ID)
	if err != nil || resolvedApproval.Status != ApprovalStatusApproved {
		t.Fatalf("resolution = %#v, %v", resolvedApproval, err)
	}
	resolvedCall, err := store.GetActionCall(ctx, endpoint.Scope, call.ID)
	if err != nil || resolvedCall.Status != ActionCallStatusReady {
		t.Fatalf("resolved call = %#v, %v", resolvedCall, err)
	}
	deliveries, err = store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{Scope: endpoint.Scope, EndpointID: endpoint.ID, Limit: 10})
	if err != nil || len(deliveries) != 2 {
		t.Fatalf("decision deliveries = %#v, %v", deliveries, err)
	}
	var decisionDelivery *ExternalConversationDelivery
	for _, delivery := range deliveries {
		if delivery.Operation == capability.ConversationDeliveryMessageUpdate {
			decisionDelivery = delivery
			break
		}
	}
	if decisionDelivery == nil {
		t.Fatalf("decision update missing: %#v", deliveries)
	}
	decisionCard, _ := decisionDelivery.Parameters["approval"].(map[string]interface{})
	if decisionCard["status"] != ApprovalStatusApproved || decisionCard["actionStatus"] != ActionCallStatusReady || decisionCard["providerApproverId"] != "U1" {
		t.Fatalf("decision card = %#v in %#v", decisionCard, decisionDelivery)
	}
	if err := consumer.ConsumeCallbackEvent(ctx, registration, subscription, callbackEvent); err != nil {
		t.Fatalf("replay = %v", err)
	}
	deliveries, err = store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{Scope: endpoint.Scope, EndpointID: endpoint.ID, Limit: 10})
	if err != nil || len(deliveries) != 2 {
		t.Fatalf("replay deliveries = %#v, %v", deliveries, err)
	}
	completedCall := cloneActionCall(resolvedCall)
	completedCall.Status = ActionCallStatusSucceeded
	if err := worker.notifyOutcome(ctx, resolvedApproval, completedCall, resolvedApproval.Destinations[0]); err != nil {
		t.Fatal(err)
	}
	if err := worker.notifyOutcome(ctx, resolvedApproval, completedCall, resolvedApproval.Destinations[0]); err != nil {
		t.Fatalf("outcome replay = %v", err)
	}
	deliveries, err = store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{Scope: endpoint.Scope, EndpointID: endpoint.ID, Limit: 10})
	if err != nil || len(deliveries) != 4 {
		t.Fatalf("outcome deliveries = %#v, %v", deliveries, err)
	}
	var outcomeMessage, outcomeCard bool
	for _, delivery := range deliveries {
		if strings.HasPrefix(delivery.IdempotencyKey, "approval-outcome-delivery:") {
			outcomeMessage = true
		}
		if delivery.Operation == capability.ConversationDeliveryMessageUpdate {
			projected, _ := delivery.Parameters["approval"].(map[string]interface{})
			if projected["actionStatus"] == ActionCallStatusSucceeded {
				outcomeCard = true
			}
		}
	}
	if !outcomeMessage || !outcomeCard {
		t.Fatalf("terminal channel projection missing: %#v", deliveries)
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
	worker.now = func() time.Time { return now }
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

func TestApprovalNotificationExpiresCheckpointAndQueuesTerminalCardUpdate(t *testing.T) {
	ctx := t.Context()
	store, catalog, endpoint := externalConversationDeliveryFixtureWithOperations(t, ctx, "slack", []skill.ConversationDeliveryOperation{
		skill.ConversationDeliveryMessageSend, skill.ConversationDeliveryMessageUpdate,
	})
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: endpoint.Scope, Owner: endpoint.Owner, AssignedAgentID: endpoint.DeploymentID,
		Goal: "post reviewed comment", Source: RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: endpoint.Scope, WorkerID: "worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	call := &ActionCall{
		ID: "call-expiry", Scope: endpoint.Scope, RunID: claimed.ID, DeploymentID: endpoint.DeploymentID,
		SkillID: "browser", SkillVersion: "1.0.0", Action: "comment", Status: ActionCallStatusWaitingApproval,
		Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal, Arguments: map[string]interface{}{"comment": "Useful context"},
		ApprovalID: "approval-expiry", MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	call.InvocationDigest = ComputeActionInvocationDigest(call)
	call.SemanticDigest = ComputeActionSemanticDigest(call)
	approval := &ApprovalCheckpoint{
		ID: "approval-expiry", Scope: endpoint.Scope, RunID: claimed.ID, ActionCallID: call.ID, Status: ApprovalStatusPending,
		Risk: skill.RiskLevelExternal, Summary: "Post reviewed comment", PolicyReason: "external write",
		ProposedAction: map[string]interface{}{"comment": "Useful context"}, EligibleApprovers: []ApprovalPrincipal{{Type: "role", ID: "operator"}},
		Destinations: []ApprovalDestination{{EndpointID: endpoint.ID}}, ExpiresAt: now.Add(15 * time.Minute), Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	waiting := cloneAgentRun(claimed)
	waiting.Status = AgentRunStatusWaitingForApproval
	waiting.WakeCondition = &WakeCondition{Type: "approval", Reference: approval.ID}
	waiting.LeaseOwner, waiting.LeaseExpiresAt = "", nil
	waiting.Revision++
	waiting.UpdatedAt = now
	if _, err = store.CreateActionProposal(ctx, ActionProposalRecord{
		Call: call, Approval: approval, Run: waiting, ExpectedRunRevision: claimed.Revision,
		Lease: &AgentRunLeaseGuard{WorkerID: "worker", Now: now},
		Event: &ActivityEvent{ID: "approval-expiry-requested", Scope: endpoint.Scope, EventType: "action.approval_requested", Severity: ActivitySeverityInfo,
			AgentID: endpoint.DeploymentID, RunID: claimed.ID, Actor: ActivityActor{Type: "worker", ID: "worker"}, Summary: approval.Summary,
			Visibility: ActivityVisibilityScope, CreatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	transport := NewExternalConversationTransportService(store, catalog)
	transport.now = func() time.Time { return now }
	worker := NewApprovalNotificationWorker(store, transport)
	worker.now = func() time.Time { return now }
	if count, err := worker.ProcessScope(ctx, endpoint.Scope, 10); err != nil || count != 1 {
		t.Fatalf("initial notification = %d, %v", count, err)
	}
	original, err := store.ClaimExternalConversationDelivery(ctx, endpoint.Scope, "delivery-worker", now, time.Minute)
	if err != nil || original == nil {
		t.Fatalf("claim notification = %#v, %v", original, err)
	}
	delivered := cloneExternalConversationDelivery(original)
	delivered.Status, delivered.LeaseOwner, delivered.LeaseExpiresAt = ExternalConversationDeliveryDelivered, "", time.Time{}
	delivered.ProviderMessageID, delivered.DeliveredAt, delivered.UpdatedAt = "1720000000.123", now.Add(time.Second), now.Add(time.Second)
	delivered.Revision++
	if err = store.SaveExternalConversationDelivery(ctx, delivered, original.Revision, "delivery-worker"); err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return approval.ExpiresAt.Add(time.Second) }
	if count, err := worker.ProcessScope(ctx, endpoint.Scope, 10); err != nil || count != 0 {
		t.Fatalf("expiry pass = %d, %v", count, err)
	}
	expired, err := store.GetApproval(ctx, endpoint.Scope, approval.ID)
	if err != nil || expired.Status != ApprovalStatusExpired {
		t.Fatalf("expired approval = %#v, %v", expired, err)
	}
	deliveries, err := store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{Scope: endpoint.Scope, EndpointID: endpoint.ID, Limit: 10})
	if err != nil || len(deliveries) != 2 {
		t.Fatalf("deliveries = %#v, %v", deliveries, err)
	}
	var update *ExternalConversationDelivery
	for _, delivery := range deliveries {
		if delivery.Operation == capability.ConversationDeliveryMessageUpdate {
			update = delivery
			break
		}
	}
	if update == nil {
		t.Fatalf("expiry update missing: %#v", deliveries)
	}
	if update.Operation != capability.ConversationDeliveryMessageUpdate || update.Parameters["providerMessageId"] != delivered.ProviderMessageID {
		t.Fatalf("expiry update = %#v", update)
	}
	card, _ := update.Parameters["approval"].(map[string]interface{})
	if card["status"] != ApprovalStatusExpired {
		t.Fatalf("expiry card = %#v", card)
	}
}
