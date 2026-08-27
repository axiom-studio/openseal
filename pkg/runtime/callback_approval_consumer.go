package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// ApprovalCallbackConsumer applies one verified approval decision to the
// canonical checkpoint coordinator. The callback target must be one of the
// checkpoint's reviewed destinations and every immutable action fact must
// match before the suspended Run can resume.
type ApprovalCallbackConsumer struct {
	store interface {
		PortfolioStore
		ActionStore
	}
	notifications ApprovalNotificationStore
	transport     *ExternalConversationTransportService
	now           func() time.Time
}

func NewApprovalCallbackConsumer(store interface {
	PortfolioStore
	ActionStore
}, transport ...*ExternalConversationTransportService) *ApprovalCallbackConsumer {
	consumer := &ApprovalCallbackConsumer{store: store, now: time.Now}
	consumer.notifications, _ = store.(ApprovalNotificationStore)
	if len(transport) > 0 {
		consumer.transport = transport[0]
	}
	return consumer
}

func (c *ApprovalCallbackConsumer) ConsumeCallbackEvent(
	ctx context.Context,
	registration *CallbackRegistration,
	subscription CallbackSubscription,
	event EventEnvelope,
) error {
	if c == nil || c.store == nil {
		return errors.New("approval callback consumer is not configured")
	}
	if registration == nil || event.Scope != registration.Scope || event.Type != capability.CallbackEventApprovalDecided ||
		strings.TrimSpace(subscription.TargetID) == "" {
		return fmt.Errorf("%w: approval callback routing is invalid", ErrInvalidCallbackRegistration)
	}
	approvalID, _ := event.Attributes["approvalId"].(string)
	actionCallID, _ := event.Attributes["actionCallId"].(string)
	invocationDigest, _ := event.Attributes["invocationDigest"].(string)
	decision, _ := event.Attributes["decision"].(string)
	principalType, _ := event.Attributes["principalType"].(string)
	principalID, _ := event.Attributes["principalId"].(string)
	reason, _ := event.Attributes["reason"].(string)
	revision, ok := integerAttribute(event.Attributes["approvalRevision"])
	if strings.TrimSpace(approvalID) == "" || strings.TrimSpace(actionCallID) == "" ||
		strings.TrimSpace(invocationDigest) == "" || revision < 1 || !ok ||
		(decision != "approve" && decision != "reject" && decision != "request_changes") ||
		strings.TrimSpace(principalType) == "" || strings.TrimSpace(principalID) == "" {
		return fmt.Errorf("%w: approval decision attributes are invalid", ErrInvalidCallbackRegistration)
	}
	approval, err := c.store.GetApproval(ctx, event.Scope, approvalID)
	if err != nil {
		return err
	}
	call, err := c.store.GetActionCall(ctx, event.Scope, approval.ActionCallID)
	if err != nil {
		return err
	}
	replay := approval.Status != ApprovalStatusPending && approval.DecisionID == event.ID
	if approval.ActionCallID != actionCallID || call.InvocationDigest != invocationDigest ||
		(!replay && approval.Revision != revision) {
		return fmt.Errorf("%w: approval decision does not match the reviewed action", ErrInvalidCallbackRegistration)
	}
	destinationID, err := c.resolveReviewedDestination(ctx, registration, subscription, event, approval)
	if err != nil {
		return err
	}
	coordinator := NewApprovalCoordinator(c.store, c.store, EligibleApprovalAuthorizer{})
	if c.now != nil {
		coordinator.now = c.now
	}
	resolution, err := coordinator.Resolve(ctx, ResolveApprovalRequest{
		Scope: event.Scope, ApprovalID: approval.ID, ExpectedRevision: revision,
		DecisionID: event.ID, Decision: ApprovalDecision(decision),
		Principal: ApprovalPrincipal{Type: principalType, ID: principalID}, Reason: reason,
		CorrelationID: event.Subject,
	})
	if err != nil {
		return err
	}
	if c.transport == nil {
		return nil
	}
	if c.notifications == nil {
		return errors.New("external approval notification coordination is unavailable")
	}
	providerApproverID, _ := event.Attributes["providerUserId"].(string)
	if err := enqueueApprovalCardUpdate(
		ctx, c.notifications, c.transport, resolution.Approval, resolution.Call,
		ApprovalDestination{EndpointID: destinationID}, providerApproverID,
	); err != nil {
		return fmt.Errorf("enqueue approval card update: %w", err)
	}
	return nil
}

// resolveReviewedDestination binds a provider decision back to the exact
// approval card that was durably delivered. Callback registrations can outlive
// individual Agent definitions and endpoints, so their consumer target is not
// authoritative for a later card. The immutable approval envelope plus the
// provider's exact message identifier is the durable correlation boundary.
func (c *ApprovalCallbackConsumer) resolveReviewedDestination(
	ctx context.Context,
	registration *CallbackRegistration,
	subscription CallbackSubscription,
	event EventEnvelope,
	approval *ApprovalCheckpoint,
) (string, error) {
	if approvalHasDestination(approval, subscription.TargetID) {
		return subscription.TargetID, nil
	}
	if c.notifications == nil {
		return "", fmt.Errorf("%w: approval decision does not match a reviewed destination", ErrInvalidCallbackRegistration)
	}
	providerMessageID, _ := event.Payload["messageId"].(string)
	providerMessageID = strings.TrimSpace(providerMessageID)
	if providerMessageID == "" {
		return "", fmt.Errorf("%w: approval decision does not identify the reviewed card", ErrInvalidCallbackRegistration)
	}
	deliveries, err := c.notifications.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{
		Scope: approval.Scope, EndpointID: subscription.TargetID,
		CorrelationKind: "approval", CorrelationID: approval.ID,
		Statuses: []ExternalConversationDeliveryStatus{ExternalConversationDeliveryDelivered}, Limit: 100,
	})
	if err != nil {
		return "", err
	}
	for _, delivery := range deliveries {
		if delivery != nil && delivery.Correlation != nil && delivery.Correlation.Phase == "request" &&
			delivery.ProviderMessageID == providerMessageID {
			return subscription.TargetID, nil
		}
	}
	for _, destination := range approval.Destinations {
		endpoint, err := c.notifications.GetExternalConversationEndpoint(ctx, approval.Scope, destination.EndpointID)
		if err != nil {
			return "", err
		}
		if endpoint == nil || endpoint.Provider != registration.Provider {
			continue
		}
		delivery, err := findDeliveredApprovalNotification(ctx, c.notifications, approval, destination)
		if err != nil {
			return "", err
		}
		if delivery != nil && delivery.ProviderMessageID == providerMessageID {
			return destination.EndpointID, nil
		}
	}
	return "", fmt.Errorf("%w: approval decision does not match the reviewed card", ErrInvalidCallbackRegistration)
}
