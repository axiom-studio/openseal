package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
}

func NewApprovalNotificationWorker(store ApprovalNotificationStore, transport *ExternalConversationTransportService) *ApprovalNotificationWorker {
	return &ApprovalNotificationWorker{store: store, conversations: NewConversationService(store), transport: transport}
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
	for _, approval := range approvals {
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
	posted, err := w.conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: approval.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender:            ConversationParticipant{Type: ConversationParticipantService, ID: "approval-coordinator"},
		SenderDisplayName: "Approval coordinator", Intent: MessageIntentApprovalRequest,
		Content: approvalNotificationText(approval, call), Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		RequiresResponse: true, IdempotencyKey: messageKey,
	})
	if errors.Is(err, ErrRevisionConflict) {
		conversation, err = w.conversations.GetConversation(ctx, approval.Scope, conversation.ID)
		if err == nil {
			posted, err = w.conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
				Scope: approval.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
				Sender:            ConversationParticipant{Type: ConversationParticipantService, ID: "approval-coordinator"},
				SenderDisplayName: "Approval coordinator", Intent: MessageIntentApprovalRequest,
				Content: approvalNotificationText(approval, call), Audience: ConversationAudience{Kind: ConversationAudienceChannel},
				RequiresResponse: true, IdempotencyKey: messageKey,
			})
		}
	}
	if err != nil {
		return err
	}
	_, err = w.transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
		Scope: approval.Scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageSend,
		ConversationID: conversation.ID, ChannelMessageID: posted.Message.ID,
		Parameters: map[string]interface{}{"approval": map[string]interface{}{
			"id": approval.ID, "revision": approval.Revision, "actionCallId": approval.ActionCallID,
			"invocationDigest": call.InvocationDigest, "risk": approval.Risk, "summary": approval.Summary,
			"policyReason": approval.PolicyReason, "proposedAction": cloneMap(approval.ProposedAction),
			"expiresAt": approval.ExpiresAt,
		}},
		IdempotencyKey: "approval-delivery:" + approval.ID + ":" + endpoint.ID,
	})
	return err
}

func approvalNotificationText(approval *ApprovalCheckpoint, call *ActionCall) string {
	return fmt.Sprintf("Approval required: %s (%s.%s, risk %s). Expires %s.",
		approval.Summary, call.SkillID, call.Action, approval.Risk, approval.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"))
}
