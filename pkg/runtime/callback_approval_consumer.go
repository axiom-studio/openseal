package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

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
}

func NewApprovalCallbackConsumer(store interface {
	PortfolioStore
	ActionStore
}) *ApprovalCallbackConsumer {
	return &ApprovalCallbackConsumer{store: store}
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
		(!replay && approval.Revision != revision) || !approvalHasDestination(approval, subscription.TargetID) {
		return fmt.Errorf("%w: approval decision does not match the reviewed action", ErrInvalidCallbackRegistration)
	}
	if decision == "request_changes" && strings.TrimSpace(reason) == "" {
		reason = "Changes requested through callback"
	}
	coordinator := NewApprovalCoordinator(c.store, c.store, EligibleApprovalAuthorizer{})
	_, err = coordinator.Resolve(ctx, ResolveApprovalRequest{
		Scope: event.Scope, ApprovalID: approval.ID, ExpectedRevision: revision,
		DecisionID: event.ID, Approve: decision == "approve",
		Principal: ApprovalPrincipal{Type: principalType, ID: principalID}, Reason: reason,
		CorrelationID: event.Subject,
	})
	return err
}
