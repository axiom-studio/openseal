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

type approvalNotificationDeliveryLookupStore struct {
	ApprovalNotificationStore
	deliveryScans atomic.Int64
}

func (s *approvalNotificationDeliveryLookupStore) ListExternalConversationDeliveries(ctx context.Context, filter ExternalConversationDeliveryFilter) ([]*ExternalConversationDelivery, error) {
	s.deliveryScans.Add(1)
	return s.ApprovalNotificationStore.ListExternalConversationDeliveries(ctx, filter)
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
	workerWithoutCallback := NewApprovalNotificationWorker(store, transport, catalog)
	workerWithoutCallback.now = func() time.Time { return now }
	if count, err := workerWithoutCallback.ProcessScope(ctx, endpoint.Scope, 10); err == nil || count != 0 ||
		!strings.Contains(err.Error(), "no active provider callback") {
		t.Fatalf("missing approval callback = %d, %v", count, err)
	}
	otherEndpoint := cloneExternalConversationEndpoint(endpoint)
	otherEndpoint.ID = "other-approval-endpoint"
	otherEndpoint.Owner = ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent:other"}
	otherEndpoint.DeploymentID = "agent:other"
	registerApprovalNotificationCallback(t, ctx, store, catalog, otherEndpoint)
	if count, err := workerWithoutCallback.ProcessScope(ctx, endpoint.Scope, 10); err == nil || count != 0 ||
		!strings.Contains(err.Error(), "no active provider callback") {
		t.Fatalf("other destination callback = %d, %v", count, err)
	}
	registerApprovalNotificationCallback(t, ctx, store, catalog, endpoint)
	lookupStore := &approvalNotificationDeliveryLookupStore{ApprovalNotificationStore: store}
	worker := NewApprovalNotificationWorker(lookupStore, transport, catalog)
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
	if deliveries[0].Correlation == nil || deliveries[0].Correlation.Kind != "approval" ||
		deliveries[0].Correlation.ID != approval.ID || deliveries[0].Correlation.Phase != "request" {
		t.Fatalf("approval request correlation = %#v", deliveries[0].Correlation)
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
	registration := &CallbackRegistration{Scope: endpoint.Scope, Provider: endpoint.Provider}
	// Callback registrations are installation-scoped and may outlive the Agent
	// endpoint that originally created them. The exact delivered card remains
	// the authoritative correlation boundary when this target is stale.
	subscription := CallbackSubscription{TargetID: "retired-slack-destination"}
	callbackEvent := EventEnvelope{
		ID: decision.ID, Scope: endpoint.Scope, Type: capability.CallbackEventApprovalDecided,
		Source: "slack", Subject: decision.ExternalMessageID, OccurredAt: now,
		Attributes: cloneMap(decision.Attributes), Payload: map[string]interface{}{"messageId": delivered.ProviderMessageID},
		Actor: ActivityActor{Type: "callback", ID: "slack-approval"},
	}
	consumer := NewApprovalCallbackConsumer(store, transport)
	consumer.now = func() time.Time { return now }
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
	if decisionDelivery.Correlation == nil || decisionDelivery.Correlation.Phase != "card_update" {
		t.Fatalf("decision correlation = %#v", decisionDelivery.Correlation)
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
	// Reconciliation may later observe more review context for the same durable
	// terminal phase. The already-enqueued projection owns retries and must not
	// conflict with a newly rendered payload under the same idempotency key.
	resolvedApproval.ContinuationCheckpoint = map[string]interface{}{
		"state": map[string]interface{}{"additionalContext": "arrived after terminal projection"},
	}
	if err := worker.notifyOutcome(ctx, resolvedApproval, completedCall, resolvedApproval.Destinations[0]); err != nil {
		t.Fatalf("outcome replay = %v", err)
	}
	if scans := lookupStore.deliveryScans.Load(); scans != 0 {
		t.Fatalf("approval outcome scanned endpoint delivery history %d times", scans)
	}
	deliveries, err = store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{Scope: endpoint.Scope, EndpointID: endpoint.ID, Limit: 10})
	if err != nil || len(deliveries) != 4 {
		t.Fatalf("outcome deliveries = %#v, %v", deliveries, err)
	}
	var outcomeMessage, outcomeCard bool
	for _, delivery := range deliveries {
		if strings.HasPrefix(delivery.IdempotencyKey, "approval-outcome-delivery:") {
			outcomeMessage = delivery.Correlation != nil && delivery.Correlation.Phase == "outcome"
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
	// The delivery intent is the durable idempotency boundary. A later pass
	// must not attempt to recreate a canonical message whose presentation has
	// drifted after the delivery was already accepted.
	messages, err := store.ListChannelMessages(ctx, ChannelMessageFilter{
		Scope: endpoint.Scope, ConversationID: deliveries[0].ConversationID, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if strings.HasPrefix(message.IdempotencyKey, "approval-outcome:"+approval.ID+":") {
			store.mu.Lock()
			store.channelMessageIDs[channelMessageStoreKey(message.Scope, message.ConversationID, message.ID)].Content = "historical presentation"
			store.mu.Unlock()
		}
	}
	if err := worker.notifyOutcome(ctx, resolvedApproval, completedCall, resolvedApproval.Destinations[0]); err != nil {
		t.Fatalf("delivered outcome replay = %v", err)
	}
}

func TestApprovalNotificationAlwaysCreatesAgentApprovalConversation(t *testing.T) {
	ctx := t.Context()
	store, catalog, endpoint := externalConversationDeliveryFixtureWithOperations(t, ctx, "slack", []skill.ConversationDeliveryOperation{
		skill.ConversationDeliveryMessageSend,
	})
	now := time.Date(2026, 8, 24, 21, 0, 0, 0, time.UTC)
	portfolio := NewPortfolioService(store)
	portfolio.now = func() time.Time { return now }
	run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: endpoint.Scope, Owner: endpoint.Owner, AssignedAgentID: endpoint.DeploymentID,
		Goal: "review pull request", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: endpoint.Scope, WorkerID: "worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	call := &ActionCall{
		ID: "call-internal", Scope: endpoint.Scope, RunID: claimed.ID, DeploymentID: endpoint.DeploymentID,
		SkillID: "summarize", SkillVersion: "1.0.0", Action: "execute", Status: ActionCallStatusWaitingApproval,
		Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal, Arguments: map[string]interface{}{"url": "https://example.com/pr/17"},
		ApprovalID: "approval-internal", MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	call.InvocationDigest = ComputeActionInvocationDigest(call)
	call.SemanticDigest = ComputeActionSemanticDigest(call)
	approval := &ApprovalCheckpoint{
		ID: "approval-internal", Scope: endpoint.Scope, RunID: claimed.ID, ActionCallID: call.ID, Status: ApprovalStatusPending,
		Risk: skill.RiskLevelExternal, Summary: "Summarize PR 17", PolicyReason: "external action",
		ProposedAction: map[string]interface{}{"url": "https://example.com/pr/17"}, EligibleApprovers: []ApprovalPrincipal{{Type: "role", ID: "operator"}},
		ExpiresAt: now.Add(time.Hour), Revision: 1, CreatedAt: now, UpdatedAt: now,
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
		Event: &ActivityEvent{ID: "approval-internal-requested", Scope: endpoint.Scope, EventType: "action.approval_requested", Severity: ActivitySeverityInfo,
			AgentID: endpoint.DeploymentID, RunID: claimed.ID, Actor: ActivityActor{Type: "worker", ID: "worker"}, Summary: approval.Summary,
			Visibility: ActivityVisibilityScope, CreatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
	worker := NewApprovalNotificationWorker(store, NewExternalConversationTransportService(store, catalog), catalog)
	worker.now = func() time.Time { return now }
	if count, err := worker.ProcessScope(ctx, endpoint.Scope, 10); err != nil || count != 0 {
		t.Fatalf("internal notification = %d, %v", count, err)
	}
	conversation, err := store.FindConversationByIdempotencyKey(ctx, endpoint.Scope, agentApprovalConversationKey(endpoint.Owner))
	if err != nil || conversation == nil || conversation.Title != "Approvals" || conversation.Owner != endpoint.Owner ||
		conversation.Origin == nil || conversation.Origin.Kind != ConversationReferenceAgentApprovals || conversation.Origin.ID != endpoint.Owner.ID {
		t.Fatalf("approval conversation = %#v, %v", conversation, err)
	}
	message, err := store.FindChannelMessageByIdempotencyKey(ctx, endpoint.Scope, conversation.ID, "approval-request:"+approval.ID)
	if err != nil || message == nil || message.Intent != MessageIntentApprovalRequest || len(message.References) != 2 ||
		message.References[0] != (ConversationReference{Kind: ConversationReferenceApproval, ID: approval.ID}) ||
		message.References[1] != (ConversationReference{Kind: ConversationReferenceRun, ID: run.ID}) {
		t.Fatalf("approval message = %#v, %v", message, err)
	}
}

func registerApprovalNotificationCallback(
	t *testing.T,
	ctx context.Context,
	store *MemoryStore,
	catalog *skill.Catalog,
	endpoint *ExternalConversationEndpoint,
) *CallbackRegistration {
	t.Helper()
	definition := &skill.Definition{
		ID: endpoint.Provider + "-approval-callback", Version: "1.0.0", Name: endpoint.Provider + " approval callback",
		Actions: map[string]skill.Action{},
		CallbackAdapters: map[string]skill.CallbackAdapter{"interactions": {
			ProtocolVersion: skill.CallbackAdapterProtocolV1, Name: "Approval interactions",
			Description: "Verify signed interactive approval decisions.", Provider: endpoint.Provider,
			EventTypes:  []string{capability.CallbackEventApprovalDecided},
			Credentials: []capability.CredentialRequirement{{Name: "signing_secret", Kind: "callback_signing_secret"}},
			Transport: skill.CallbackAdapterTransport{
				Kind: "http", IngressEndpoint: "/v1/callbacks/" + endpoint.Provider,
				IngressCredentials: []string{"signing_secret"},
			},
		}},
	}
	if existing, err := catalog.GetDefinition(ctx, definition.ID, definition.Version); err != nil || existing == nil {
		if err := catalog.Register(ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	binding := &skill.Binding{
		ID: endpoint.Provider + "-approval-callback", Scope: skill.ScopeReference{Kind: endpoint.Scope.Kind, ID: endpoint.Scope.ID},
		DeploymentID: endpoint.DeploymentID, SkillID: definition.ID, SkillVersion: definition.Version,
		EnabledCallbackAdapters: []string{"interactions"}, MaximumRisk: skill.RiskLevelRead, Revision: 1,
		Credentials: map[string]skill.CredentialReference{
			"signing_secret": {Kind: "callback_signing_secret", ID: "credential://callback-signing-secret"},
		},
	}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	registry := NewCallbackRegistry(store, catalog)
	created, err := registry.Create(ctx, CreateCallbackRegistrationRequest{
		ID: endpoint.ID + "-approval-callback", Scope: endpoint.Scope, Owner: endpoint.Owner,
		DeploymentID: endpoint.DeploymentID, Name: endpoint.Name + " approval callback", Provider: endpoint.Provider,
		Adapter: CallbackAdapterReference{
			SkillID: definition.ID, SkillVersion: definition.Version, BindingID: binding.ID,
			BindingRevision: binding.Revision, AdapterID: "interactions",
		},
		Subscriptions: []CallbackSubscription{{
			EventType: capability.CallbackEventApprovalDecided, Consumer: "approvals", TargetID: endpoint.ID,
		}},
		Actor: ActivityActor{Type: "user", ID: "operator"}, Reason: "enable interactive approval decisions",
	})
	if err != nil {
		t.Fatal(err)
	}
	active := CallbackRegistrationActive
	updated, err := registry.Update(ctx, created.Scope, created.ID, UpdateCallbackRegistrationRequest{
		ExpectedRevision: created.Revision, Status: &active,
		Actor: ActivityActor{Type: "user", ID: "operator"}, Reason: "activate interactive approval decisions",
	})
	if err != nil {
		t.Fatal(err)
	}
	return updated
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
	registerApprovalNotificationCallback(t, ctx, store, catalog, endpoint)
	worker := NewApprovalNotificationWorker(conflicts, NewExternalConversationTransportService(store, catalog), catalog)
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

func TestApprovalNotificationReusesConversationAfterEndpointRename(t *testing.T) {
	ctx := t.Context()
	store, catalog, endpoint := externalConversationDeliveryFixture(t, ctx, "slack")
	registerApprovalNotificationCallback(t, ctx, store, catalog, endpoint)
	worker := NewApprovalNotificationWorker(store, NewExternalConversationTransportService(store, catalog), catalog)

	first, err := worker.approvalConversation(ctx, endpoint.Scope, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	renamed := cloneExternalConversationEndpoint(endpoint)
	renamed.Name = "Approvals in the new channel"
	renamed.Revision++
	renamed.UpdatedAt = renamed.UpdatedAt.Add(time.Second)
	if err := store.UpdateExternalConversationEndpoint(ctx, renamed, endpoint.Revision); err != nil {
		t.Fatal(err)
	}

	replayed, err := worker.approvalConversation(ctx, renamed.Scope, renamed)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ID != first.ID || replayed.Owner != first.Owner {
		t.Fatalf("renamed endpoint conversation = %#v, want stable %#v", replayed, first)
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
	registerApprovalNotificationCallback(t, ctx, store, catalog, endpoint)
	worker := NewApprovalNotificationWorker(store, transport, catalog)
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
