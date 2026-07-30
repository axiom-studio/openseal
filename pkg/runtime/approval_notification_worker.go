package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// ApprovalNotificationStore is the durable intersection needed to project a
// canonical approval into an external conversation. No provider-specific
// fields or credentials cross this boundary.
type ApprovalNotificationStore interface {
	ExternalConversationStore
	ActionStore
	PortfolioStore
}

type ApprovalNotificationWorker struct {
	store         ApprovalNotificationStore
	conversations *ConversationService
	transport     *ExternalConversationTransportService
	now           func() time.Time
}

const approvalNotificationPostAttempts = 5

func NewApprovalNotificationWorker(store ApprovalNotificationStore, transport *ExternalConversationTransportService) *ApprovalNotificationWorker {
	return &ApprovalNotificationWorker{store: store, conversations: NewConversationService(store), transport: transport, now: time.Now}
}

// ProcessScope idempotently materializes pending approvals as canonical
// approval_request messages and durable endpoint deliveries.
func (w *ApprovalNotificationWorker) ProcessScope(ctx context.Context, scope Scope, limit int) (int, error) {
	if w == nil || w.store == nil || w.transport == nil {
		return 0, errors.New("approval notification worker is not configured")
	}
	approvals, err := w.store.ListApprovals(ctx, ApprovalFilter{Scope: scope, Status: []ApprovalStatus{ApprovalStatusPending}, Limit: limit})
	if err != nil {
		return 0, err
	}
	processed := 0
	var processErrors []error
	now := w.now().UTC()
	for _, approval := range approvals {
		if !now.Before(approval.ExpiresAt) {
			if err := w.expire(ctx, approval, now); err != nil {
				processErrors = append(processErrors, fmt.Errorf("expire approval %s: %w", approval.ID, err))
			}
			continue
		}
		for _, destination := range approval.Destinations {
			if err := w.notify(ctx, approval, destination); err != nil {
				processErrors = append(processErrors, fmt.Errorf("notify approval %s at endpoint %s: %w", approval.ID, destination.EndpointID, err))
				continue
			}
			processed++
		}
	}
	return processed, errors.Join(processErrors...)
}

func (w *ApprovalNotificationWorker) expire(ctx context.Context, approval *ApprovalCheckpoint, now time.Time) error {
	coordinator := NewApprovalCoordinator(w.store, w.store, EligibleApprovalAuthorizer{})
	coordinator.now = func() time.Time { return now }
	resolved, err := coordinator.Resolve(ctx, ResolveApprovalRequest{
		Scope: approval.Scope, ApprovalID: approval.ID, ExpectedRevision: approval.Revision,
		DecisionID: "approval-expiry:" + approval.ID + ":" + approval.ExpiresAt.UTC().Format(time.RFC3339Nano),
		Approve:    false, Principal: ApprovalPrincipal{Type: "system", ID: "approval-expiry-worker"},
		Reason: "Approval deadline elapsed", CorrelationID: approval.ID,
	})
	if err != nil {
		return err
	}
	if resolved == nil || resolved.Approval == nil || resolved.Approval.Status != ApprovalStatusExpired {
		return errors.New("approval expiry did not reach the terminal expired state")
	}
	for _, destination := range approval.Destinations {
		if err := w.updateExpiredCard(ctx, resolved.Approval, destination); err != nil {
			return err
		}
	}
	return nil
}

func (w *ApprovalNotificationWorker) updateExpiredCard(ctx context.Context, approval *ApprovalCheckpoint, destination ApprovalDestination) error {
	call, err := w.store.GetActionCall(ctx, approval.Scope, approval.ActionCallID)
	if err != nil {
		return err
	}
	return enqueueApprovalCardUpdate(ctx, w.store, w.transport, approval, call, destination, "")
}

func enqueueApprovalCardUpdate(
	ctx context.Context,
	store ApprovalNotificationStore,
	transport *ExternalConversationTransportService,
	approval *ApprovalCheckpoint,
	call *ActionCall,
	destination ApprovalDestination,
	providerApproverID string,
) error {
	if approval == nil || call == nil {
		return errors.New("approval and action are required")
	}
	_, resolved, err := transport.resolveActiveEndpoint(ctx, approval.Scope, destination.EndpointID)
	if err != nil {
		return err
	}
	if !containsConversationDeliveryOperation(resolved.Adapter.Delivery.Operations, capability.ConversationDeliveryMessageUpdate) {
		return nil
	}
	deliveries, err := store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{
		Scope: approval.Scope, EndpointID: destination.EndpointID,
		Statuses: []ExternalConversationDeliveryStatus{ExternalConversationDeliveryDelivered}, Limit: 1000,
	})
	if err != nil {
		return err
	}
	for _, delivery := range deliveries {
		if delivery.Operation != capability.ConversationDeliveryMessageSend {
			continue
		}
		projected, _ := delivery.Parameters["approval"].(map[string]interface{})
		if projected == nil || projected["id"] != approval.ID || strings.TrimSpace(delivery.ProviderMessageID) == "" {
			continue
		}
		parameters := map[string]interface{}{
			"providerMessageId": delivery.ProviderMessageID,
			"approval":          approvalNotificationPayload(approval, call),
		}
		if strings.TrimSpace(providerApproverID) != "" {
			parameters["providerApproverId"] = strings.TrimSpace(providerApproverID)
			parameters["approval"].(map[string]interface{})["providerApproverId"] = strings.TrimSpace(providerApproverID)
		}
		_, err = transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
			Scope: approval.Scope, EndpointID: destination.EndpointID,
			Operation:      capability.ConversationDeliveryMessageUpdate,
			ConversationID: delivery.ConversationID, ChannelMessageID: delivery.ChannelMessageID,
			ExternalThreadID: delivery.ExternalThreadID,
			Parameters:       parameters,
			IdempotencyKey:   "approval-card-update:" + approval.ID + ":" + destination.EndpointID + ":" + approvalCardPhase(approval, call),
		})
		return err
	}
	// A decision can arrive only after the provider has rendered the original
	// message, but expiry can race its delivery. Canonical state remains
	// authoritative when there is not yet a remote card to update.
	return nil
}

func approvalCardPhase(approval *ApprovalCheckpoint, call *ActionCall) string {
	if approval.Status != ApprovalStatusApproved {
		return string(approval.Status)
	}
	switch call.Status {
	case ActionCallStatusSucceeded:
		return "succeeded"
	case ActionCallStatusFailed, ActionCallStatusDenied:
		return "failed"
	case ActionCallStatusCanceled, ActionCallStatusCompensated:
		return "canceled"
	default:
		return "going-ahead"
	}
}

func (w *ApprovalNotificationWorker) notify(ctx context.Context, approval *ApprovalCheckpoint, destination ApprovalDestination) error {
	endpoint, err := w.store.GetExternalConversationEndpoint(ctx, approval.Scope, strings.TrimSpace(destination.EndpointID))
	if err != nil {
		return err
	}
	if endpoint == nil || endpoint.Status != ExternalConversationEndpointActive {
		return fmt.Errorf("%w: approval endpoint is unavailable", ErrInvalidExternalConversation)
	}
	call, err := w.store.GetActionCall(ctx, approval.Scope, approval.ActionCallID)
	if err != nil {
		return err
	}
	conversationKey := "approval-notifications:" + endpoint.ID
	conversation, _, err := w.conversations.CreateConversation(ctx, CreateConversationRequest{
		Scope: approval.Scope, Owner: endpoint.Owner, Title: endpoint.Name + " approvals", IdempotencyKey: conversationKey,
	})
	if err != nil {
		return err
	}
	messageKey := "approval-request:" + approval.ID
	posted, err := w.postApprovalMessage(ctx, approval, call, conversation.ID, messageKey)
	if err != nil {
		return fmt.Errorf("post canonical approval message: %w", err)
	}
	_, err = w.transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
		Scope: approval.Scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageSend,
		ConversationID: conversation.ID, ChannelMessageID: posted.Message.ID,
		Parameters:     map[string]interface{}{"approval": approvalNotificationPayload(approval, call)},
		IdempotencyKey: "approval-delivery:" + approval.ID + ":" + endpoint.ID,
	})
	if err != nil {
		return fmt.Errorf("enqueue approval delivery: %w", err)
	}
	return nil
}

func approvalNotificationPayload(approval *ApprovalCheckpoint, call *ActionCall) map[string]interface{} {
	payload := map[string]interface{}{
		"id": approval.ID, "revision": approval.Revision, "actionCallId": approval.ActionCallID,
		"risk": approval.Risk, "summary": approval.Summary, "status": approval.Status,
		"policyReason": approval.PolicyReason, "proposedAction": cloneMap(approval.ProposedAction),
		"expiresAt": approval.ExpiresAt,
	}
	if call != nil {
		payload["invocationDigest"] = call.InvocationDigest
		payload["actionStatus"] = call.Status
		if strings.TrimSpace(call.Error) != "" {
			payload["actionError"] = call.Error
		}
	}
	if approval.DecisionBy != nil {
		payload["decisionBy"] = map[string]interface{}{"type": approval.DecisionBy.Type, "id": approval.DecisionBy.ID}
	}
	if approval.DecidedAt != nil {
		payload["decidedAt"] = approval.DecidedAt.UTC()
	}
	if strings.TrimSpace(approval.DecisionReason) != "" {
		payload["decisionReason"] = approval.DecisionReason
	}
	return payload
}

func (w *ApprovalNotificationWorker) postApprovalMessage(
	ctx context.Context,
	approval *ApprovalCheckpoint,
	call *ActionCall,
	conversationID string,
	messageKey string,
) (*ChannelMessageCommitResult, error) {
	request := PostChannelMessageRequest{
		Scope: approval.Scope, ConversationID: conversationID,
		Sender:            ConversationParticipant{Type: ConversationParticipantService, ID: "approval-coordinator"},
		SenderDisplayName: "Approval coordinator", Intent: MessageIntentApprovalRequest,
		Content: approvalNotificationText(approval, call), Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		RequiresResponse: true, IdempotencyKey: messageKey,
	}
	for range approvalNotificationPostAttempts {
		conversation, err := w.conversations.GetConversation(ctx, approval.Scope, conversationID)
		if err != nil {
			return nil, err
		}
		request.ExpectedRevision = conversation.Revision
		posted, err := w.conversations.PostChannelMessage(ctx, request)
		if err == nil {
			return posted, nil
		}
		if !errors.Is(err, ErrRevisionConflict) {
			return nil, err
		}
	}
	return nil, ErrRevisionConflict
}

func approvalNotificationText(approval *ApprovalCheckpoint, call *ActionCall) string {
	return fmt.Sprintf("Approval required: %s (%s.%s, risk %s). Expires %s.",
		approval.Summary, call.SkillID, call.Action, approval.Risk, approval.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"))
}
