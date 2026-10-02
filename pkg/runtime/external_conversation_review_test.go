package runtime

import (
	"context"
	"github.com/axiom-studio/openseal/pkg/capability"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExternalReviewLinksUseOriginAndCanonicalRequests(t *testing.T) {
	for _, provider := range []string{"slack", "webchat"} {
		t.Run(provider, func(t *testing.T) {
			fixture := newRunProgressWorkerFixture(t, provider, []capability.ConversationDeliveryOperation{capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryMessageUpdate, capability.ConversationDeliveryTypingIndicator})
			fixture.store.agentRuns[portfolioKey(fixture.run.Scope, fixture.run.ID)].Context = map[string]interface{}{"conversationId": fixture.conversation.ID, "triggerMessageId": fixture.inbound.ID}
			request := setupRequestFixture()
			request.Scope = fixture.run.Scope
			request.DeploymentID = fixture.endpoint.DeploymentID
			request.ConversationID = fixture.conversation.ID
			request.TriggerMessageID = fixture.inbound.ID
			request.RunID = fixture.run.ID
			request.Reason = "Read **Slack members** for this request."
			if err := fixture.store.SaveSkillSetupRequest(t.Context(), request, 0); err != nil {
				t.Fatal(err)
			}
			approval := &ApprovalCheckpoint{ID: "review-approval", Revision: 1, Scope: fixture.run.Scope, RunID: fixture.run.ID, Status: ApprovalStatusPending, Summary: "Review **this change**.", ExpiresAt: time.Now().Add(time.Hour)}
			approval.ActionCallID = "review-call"
			fixture.store.actions[portfolioKey(approval.Scope, approval.ActionCallID)] = &ActionCall{ID: approval.ActionCallID, InvocationDigest: strings.Repeat("a", 64)}
			fixture.store.approvals[portfolioKey(approval.Scope, approval.ID)] = approval
			worker, err := NewRunProgressAcknowledgementWorker(fixture.store, fixture.catalog, RunProgressAcknowledgementRendererFunc(func(context.Context, RunProgressAcknowledgementRequest) (string, error) {
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
				if _, ok := delivery.Parameters["reviewRequest"]; !ok {
					t.Fatal("missing portable review metadata")
				}
			}
			for _, delivery := range deliveries {
				if delivery.Correlation.ID == "approval:"+approval.ID {
					original := fixture.store.externalDeliveries[externalConversationDeliveryKey(delivery.Scope, delivery.ID)]
					original.Status = ExternalConversationDeliveryDelivered
					original.ProviderMessageID = "provider-approval"
					original.DeliveredAt = time.Now()
				}
			}
			request.Status = "dismissed"
			request.ResolvedBy = "user"
			request.Revision++
			if err := fixture.store.SaveSkillSetupRequest(t.Context(), request, 1); err != nil {
				t.Fatal(err)
			}
			fixture.store.approvals[portfolioKey(approval.Scope, approval.ID)].Status = ApprovalStatusApproved
			fixture.store.approvals[portfolioKey(approval.Scope, approval.ID)].Revision++
			if _, err := worker.ProcessCommentaryScope(t.Context(), fixture.run.Scope); err != nil {
				t.Fatal(err)
			}
			updates, err := fixture.store.ListExternalConversationDeliveries(t.Context(), ExternalConversationDeliveryFilter{Scope: fixture.run.Scope, CorrelationKind: "review_request", CorrelationID: "approval:" + approval.ID, Limit: 100})
			if err != nil || len(updates) != 2 {
				t.Fatalf("resolved card update missing: %v %v", updates, err)
			}
			if len(fixture.store.externalDeliveries) != 4 {
				t.Fatal("terminal requests generated duplicate notifications")
			}
		})
	}
}
func TestPendingSetupProjectionIsTenantScoped(t *testing.T) {
	sqlite, err := NewSQLiteStore(filepath.Join(t.TempDir(), "pending.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlite.Close()
	type pendingStore interface {
		SkillSetupRequestStore
		ListPendingSkillSetupRequests(context.Context, Scope, int, int) ([]*SkillSetupRequest, error)
	}
	for _, store := range []pendingStore{NewMemoryStore(), sqlite} {
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
	run := fixture.store.agentRuns[portfolioKey(fixture.run.Scope, fixture.run.ID)]
	run.Context = map[string]interface{}{"conversationId": fixture.conversation.ID, "triggerMessageId": fixture.inbound.ID}
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
	if allowed, err := service.approvalOriginDestination(t.Context(), approval, fixture.endpoint); err != nil || allowed {
		t.Fatalf("app message reused Slack approval authority %v %v", allowed, err)
	}
}
