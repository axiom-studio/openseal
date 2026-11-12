package runtime

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

func TestExternalReviewLinksUseOriginAndCanonicalRequests(t *testing.T) {
	for _, provider := range []string{"slack", "webchat"} {
		t.Run(provider, func(t *testing.T) {
			store, db := newTestSQLiteStore(t)
			fixture := newRunProgressWorkerFixtureOn(t, store, provider, []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryMessageUpdate, capability.ConversationDeliveryTypingIndicator})
			rewriteRowPayload(t, db, "agent_runs", fixture.run.Scope, fixture.run.ID, func(run *AgentRun) {
				run.Context = map[string]interface{}{"conversationId": fixture.conversation.ID, "triggerMessageId": fixture.inbound.ID}
			})
			request := setupRequestFixture()
			request.Scope = fixture.run.Scope
			request.Owner, request.DeploymentID, request.AgentID = ObjectiveOwner{Type: OwnerTypeAgent, ID: fixture.endpoint.DeploymentID}, fixture.endpoint.DeploymentID, fixture.endpoint.DeploymentID
			request.ConversationID = fixture.conversation.ID
			request.TriggerMessageID = fixture.inbound.ID
			request.RunID = fixture.run.ID
			request.Reason = "Read **Slack members** for this request."
			if err := fixture.store.SaveSkillSetupRequest(t.Context(), request, 0); err != nil {
				t.Fatal(err)
			}
			approval := &ApprovalCheckpoint{ID: "review-approval", Revision: 1, Scope: fixture.run.Scope, RunID: fixture.run.ID, Status: ApprovalStatusPending, Summary: "Review **this change**.", ExpiresAt: time.Now().Add(time.Hour)}
			approval.ActionCallID = "review-call"
			reviews := &reviewApprovalStore{SQLiteStore: fixture.store, approval: approval,
				call: &ActionCall{ID: approval.ActionCallID, Scope: approval.Scope, InvocationDigest: strings.Repeat("a", 64)}}
			worker, err := NewRunProgressAcknowledgementWorker(reviews, fixture.catalog, RunProgressAcknowledgementRendererFunc(func(context.Context, RunProgressAcknowledgementRequest) (string, error) {
				t.Fatal("review invoked a model")
				return "", nil
			}), RunProgressAcknowledgementWorkerConfig{ReviewURL: func(kind, id, deploymentID, conversationID string, owner ObjectiveOwner) (string, error) {
				if deploymentID != fixture.endpoint.DeploymentID || conversationID != fixture.conversation.ID || owner != fixture.run.Owner {
					t.Fatal("foreign routing context")
				}
				return "https://seal.example/chat/agent?" + kind + "=" + id, nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := worker.ProcessCommentaryScope(t.Context(), fixture.run.Scope); err != nil {
					t.Fatal(err)
				}
			}
			deliveries, err := fixture.store.ListExternalConversationDeliveries(t.Context(), ExternalConversationDeliveryFilter{Scope: fixture.run.Scope, CorrelationKind: "review_request", Limit: 100})
			if err != nil || len(deliveries) != 2 {
				t.Fatalf("duplicate/missing review links: %#v %v", deliveries, err)
			}
			for _, delivery := range deliveries {
				if delivery.ExternalThreadID != "thread" || delivery.ExternalConversationID != "conversation" {
					t.Fatal("review escaped its origin")
				}
				review, ok := delivery.Parameters["reviewRequest"].(map[string]interface{})
				if !ok {
					t.Fatal("missing portable review metadata")
				}
				if _, stale := review["reason"]; stale {
					t.Fatal("review still carries the turn summary as its reason")
				}
				presentation, _ := review["presentation"].(map[string]interface{})
				message, err := fixture.store.GetChannelMessage(t.Context(), delivery.Scope, delivery.ConversationID, delivery.ChannelMessageID)
				if err != nil || message.Sender != (ConversationParticipant{Type: ConversationParticipantService, ID: ChannelReviewParticipantID}) ||
					conversationMessageStartsRun(fixture.conversation, message) {
					t.Fatalf("review message = %#v %v", message, err)
				}
				if delivery.Correlation.ID == "approval:"+approval.ID {
					if presentation["title"] != "Review **this change**" || !strings.HasPrefix(message.Content, "Review **this change**\n\n[Review approval](") {
						t.Fatalf("approval card = %#v %q", presentation, message.Content)
					}
				} else if presentation["title"] != request.Reason {
					t.Fatalf("setup card = %#v", presentation)
				}
			}
			markReviewCardDelivered(t, fixture.store, fixture.run.Scope, "approval:"+approval.ID, "provider-approval")
			request.Status = "dismissed"
			request.ResolvedBy = "user"
			request.Revision++
			if err := fixture.store.SaveSkillSetupRequest(t.Context(), request, 1); err != nil {
				t.Fatal(err)
			}
			approval.Status = ApprovalStatusApproved
			approval.Revision++
			if _, err := worker.ProcessCommentaryScope(t.Context(), fixture.run.Scope); err != nil {
				t.Fatal(err)
			}
			updates, err := fixture.store.ListExternalConversationDeliveries(t.Context(), ExternalConversationDeliveryFilter{Scope: fixture.run.Scope, CorrelationKind: "review_request", CorrelationID: "approval:" + approval.ID, Limit: 100})
			if err != nil || len(updates) != 2 {
				t.Fatalf("resolved card update missing: %v %v", updates, err)
			}
			decided := 0
			for _, update := range updates {
				if update.Operation != capability.ConversationDeliveryMessageUpdate {
					continue
				}
				decided++
				review := update.Parameters["reviewRequest"].(map[string]interface{})
				if review["presentation"].(map[string]interface{})["title"] != "Approved: Review **this change**" {
					t.Fatalf("decided card lost its description: %#v", review)
				}
			}
			if decided != 1 {
				t.Fatal("decided card update missing")
			}
			if countRows(t, db, "external_conversation_deliveries") != 4 {
				t.Fatal("terminal requests generated duplicate notifications")
			}
		})
	}
}
func TestPendingSetupProjectionIsTenantScoped(t *testing.T) {
	sqlite, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "pending.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlite.Close()
	type pendingStore interface {
		SkillSetupRequestStore
		ListPendingSkillSetupRequests(context.Context, Scope, int, int) ([]*SkillSetupRequest, error)
	}
	for _, store := range []pendingStore{newTestKernelStore(t), sqlite} {
		request := setupRequestFixture()
		if err := store.SaveSkillSetupRequest(t.Context(), request, 0); err != nil {
			t.Fatal(err)
		}
		items, err := store.ListPendingSkillSetupRequests(t.Context(), request.Scope, 100, 0)
		if err != nil || len(items) != 1 {
			t.Fatalf("pending list %v %v", items, err)
		}
		items, err = store.ListPendingSkillSetupRequests(t.Context(), Scope{Kind: "tenant", ID: "other"}, 100, 0)
		if err != nil || len(items) != 0 {
			t.Fatal("pending request leaked across tenant")
		}
		request.Status = "dismissed"
		request.ResolvedBy = "user"
		request.Revision++
		if err := store.SaveSkillSetupRequest(t.Context(), request, 1); err != nil {
			t.Fatal(err)
		}
		items, err = store.ListPendingSkillSetupRequests(t.Context(), request.Scope, 100, 0)
		if err != nil || len(items) != 0 {
			t.Fatal("terminal request appeared pending")
		}
	}
}

func TestOriginApprovalDestinationRequiresDurableImportedTrigger(t *testing.T) {
	fixture := newRunProgressWorkerFixture(t, "slack", []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend})
	run, err := fixture.store.GetAgentRun(t.Context(), fixture.run.Scope, fixture.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	run.Context = map[string]interface{}{"conversationId": fixture.conversation.ID, "triggerMessageId": fixture.inbound.ID}
	saveReviewTestRun(t, fixture.store, run)
	service := NewExternalConversationTransportService(fixture.store, fixture.catalog)
	approval := &ApprovalCheckpoint{Scope: fixture.run.Scope, RunID: fixture.run.ID}
	if allowed, err := service.approvalOriginDestination(t.Context(), approval, fixture.endpoint); err != nil || !allowed {
		t.Fatalf("origin rejected %v %v", allowed, err)
	}
	other := cloneExternalConversationEndpoint(fixture.endpoint)
	other.ID = "other-endpoint"
	if allowed, err := service.approvalOriginDestination(t.Context(), approval, other); err != nil || allowed {
		t.Fatalf("other endpoint allowed %v %v", allowed, err)
	}
	run.Context["triggerMessageId"] = "app-origin-message"
	saveReviewTestRun(t, fixture.store, run)
	if allowed, err := service.approvalOriginDestination(t.Context(), approval, fixture.endpoint); err != nil || allowed {
		t.Fatalf("app message reused Slack approval authority %v %v", allowed, err)
	}
}

func TestOriginApprovalConversationMembersMayDecide(t *testing.T) {
	for _, decision := range []string{"approve", "reject"} {
		fixture := newRunProgressWorkerFixture(t, "slack", []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend})
		now := time.Now().UTC()
		run, err := fixture.store.GetAgentRun(t.Context(), fixture.run.Scope, fixture.run.ID)
		if err != nil {
			t.Fatal(err)
		}
		// The Run's worker proposes the call and parks the Run on its
		// approval, as the loop does.
		run.Context = map[string]interface{}{"conversationId": fixture.conversation.ID, "triggerMessageId": fixture.inbound.ID}
		leaseExpires := now.Add(time.Minute)
		run.LeaseOwner, run.LeaseExpiresAt = "worker", &leaseExpires
		saveReviewTestRun(t, fixture.store, run)
		waiting := cloneAgentRun(run)
		waiting.Status = AgentRunStatusWaitingForApproval
		waiting.WakeCondition = &WakeCondition{Type: "approval", Reference: "approval"}
		waiting.LeaseOwner = ""
		waiting.LeaseExpiresAt = nil
		waiting.Revision++
		call := &ActionCall{ID: "call", Scope: run.Scope, RunID: run.ID, DeploymentID: fixture.endpoint.DeploymentID, SkillID: "browser", SkillVersion: "1", Action: "send", TurnID: "step-1", Origin: ActionOriginLoop, Execution: ActionExecutionLoop, Status: ActionCallStatusWaitingApproval, Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal, Arguments: map[string]interface{}{"text": "reviewed"}, ApprovalID: "approval", MaxAttempts: 1, AvailableAt: now, Revision: 1, CreatedAt: now, UpdatedAt: now}
		call.InvocationDigest = ComputeActionInvocationDigest(call)
		call.SemanticDigest = ComputeActionSemanticDigest(call)
		approval := &ApprovalCheckpoint{ID: "approval", Scope: run.Scope, RunID: run.ID, ActionCallID: call.ID, Status: ApprovalStatusPending, Risk: skill.RiskLevelExternal, Summary: "Send reviewed text", EligibleApprovers: []ApprovalPrincipal{{Type: "role", ID: "operator"}}, ExpiresAt: now.Add(time.Hour), Revision: 1, CreatedAt: now, UpdatedAt: now}
		proposed := &ActivityEvent{ID: "approval-requested", Scope: run.Scope, RunID: run.ID, EventType: "action.approval_requested", Summary: approval.Summary,
			Actor: ActivityActor{Type: "worker", ID: "worker"}, Visibility: ActivityVisibilityScope, CreatedAt: now}
		if _, err := fixture.store.CreateActionProposal(t.Context(), ActionProposalRecord{Call: call, Approval: approval, Run: waiting, ExpectedRunRevision: run.Revision,
			Lease: &AgentRunLeaseGuard{WorkerID: "worker", Now: now}, Event: proposed}); err != nil {
			t.Fatal(err)
		}
		event := NormalizedExternalConversationEvent{ID: "decision", Type: capability.ConversationEventApprovalDecided, ExternalConversationID: "C123", ExternalMessageID: "171.003", OrderingKey: "C123:171.003", OccurredAt: now, ExternalParticipantID: "UANY", Attributes: map[string]interface{}{"approvalId": approval.ID, "approvalRevision": int64(1), "actionCallId": call.ID, "invocationDigest": call.InvocationDigest, "decision": decision, "principalType": "external_participant", "principalId": "UOTHER"}}
		service := NewExternalConversationTransportService(fixture.store, fixture.catalog)
		if _, err := service.resolveExternalApprovalDecision(t.Context(), fixture.endpoint, event); err == nil {
			t.Fatal("mismatched provider identity accepted")
		}
		event.Attributes["principalId"] = "UANY"
		received, err := service.Receive(t.Context(), ReceiveExternalConversationEventRequest{Scope: fixture.endpoint.Scope, EndpointID: fixture.endpoint.ID, Event: event})
		var result *ApprovalResolutionResult
		if received != nil {
			result = received.Approval
		}
		expected := ApprovalStatusApproved
		if decision == "reject" {
			expected = ApprovalStatusRejected
		}
		if err != nil || result == nil || result.Approval.Status != expected || result.Approval.DecisionBy.ID != "UANY" {
			t.Fatalf("member decision failed: %v %v", result, err)
		}
		event.ID = "another-click"
		if _, err := service.resolveExternalApprovalDecision(t.Context(), fixture.endpoint, event); err == nil {
			t.Fatal("stale card changed a completed decision")
		}

	}
}

// reviewApprovalStore adds one approval and its action call to the reads of
// a store, for an approval a test sets directly instead of proposing it from
// a leased Run.
type reviewApprovalStore struct {
	*sqlite.SQLiteStore
	approval *ApprovalCheckpoint
	call     *ActionCall
}

func (s *reviewApprovalStore) ListApprovals(ctx context.Context, filter ApprovalFilter) ([]*ApprovalCheckpoint, error) {
	approvals, err := s.SQLiteStore.ListApprovals(ctx, filter)
	if err != nil {
		return nil, err
	}
	if filter.Offset == 0 && filter.Scope == s.approval.Scope && slices.Contains(filter.Status, s.approval.Status) {
		approvals = append(approvals, cloneApprovalCheckpoint(s.approval))
	}
	return approvals, nil
}

func (s *reviewApprovalStore) GetApproval(ctx context.Context, scope Scope, id string) (*ApprovalCheckpoint, error) {
	if scope == s.approval.Scope && id == s.approval.ID {
		return cloneApprovalCheckpoint(s.approval), nil
	}
	return s.SQLiteStore.GetApproval(ctx, scope, id)
}

func (s *reviewApprovalStore) GetActionCall(ctx context.Context, scope Scope, id string) (*ActionCall, error) {
	if scope == s.call.Scope && id == s.call.ID {
		return cloneActionCall(s.call), nil
	}
	return s.SQLiteStore.GetActionCall(ctx, scope, id)
}

// markReviewCardDelivered delivers the review card with correlation id, as
// the delivery worker does once the provider accepts it.
func markReviewCardDelivered(t *testing.T, store *sqlite.SQLiteStore, scope Scope, correlationID, providerMessageID string) {
	t.Helper()
	now := time.Now().UTC()
	leased, err := store.ClaimExternalConversationDelivery(t.Context(), scope, "delivery-worker", now, time.Minute)
	if err != nil || leased == nil || leased.Correlation == nil || leased.Correlation.ID != correlationID {
		t.Fatalf("claim review card %s = %#v, %v", correlationID, leased, err)
	}
	delivered := cloneExternalConversationDelivery(leased)
	delivered.Status, delivered.LeaseOwner, delivered.LeaseExpiresAt = ExternalConversationDeliveryDelivered, "", time.Time{}
	delivered.ProviderMessageID, delivered.DeliveredAt, delivered.UpdatedAt = providerMessageID, now, now
	delivered.Revision++
	if err := store.SaveExternalConversationDelivery(t.Context(), delivered, leased.Revision, "delivery-worker"); err != nil {
		t.Fatal(err)
	}
}

// saveReviewTestRun saves run as the next revision of the stored Run.
func saveReviewTestRun(t *testing.T, store *sqlite.SQLiteStore, run *AgentRun) {
	t.Helper()
	expected := run.Revision
	run.Revision++
	run.UpdatedAt = time.Now().UTC()
	event := &ActivityEvent{ID: uuid.NewString(), Scope: run.Scope, RunID: run.ID, EventType: "run.updated", Summary: "Update review test run",
		Actor: ActivityActor{Type: "test", ID: "review"}, Visibility: ActivityVisibilityScope, CreatedAt: run.UpdatedAt}
	if _, err := store.UpdateAgentRunWithEvent(t.Context(), run, expected, event, nil); err != nil {
		t.Fatal(err)
	}
}
