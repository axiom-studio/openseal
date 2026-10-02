package runtime

import (
	"context"
	"errors"
	"net/url"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type channelReviewStore interface {
	ActionStore
	ListPendingSkillSetupRequests(context.Context, Scope, int, int) ([]*SkillSetupRequest, error)
}

// Links identify existing canonical requests; they carry no decision, credential
// or authorization token. The host owns web routing and normal app authorization.
func (w *RunProgressAcknowledgementWorker) projectReviewRequests(ctx context.Context, scope Scope) ([]*ExternalConversationDelivery, error) {
	if w.config.ReviewURL == nil {
		return nil, nil
	}
	store, ok := w.store.(channelReviewStore)
	if !ok {
		return nil, errors.New("channel review storage unavailable")
	}
	var result []*ExternalConversationDelivery
	var failures []error
	for offset := 0; ; offset += 100 {
		approvals, err := store.ListApprovals(ctx, ApprovalFilter{Scope: scope, Status: []ApprovalStatus{ApprovalStatusPending}, Limit: 100, Offset: offset})
		if err != nil {
			return result, err
		}
		for _, approval := range approvals {
			delivery, err := w.projectReview(ctx, scope, "approval", approval.ID, approval.RunID, "", "", approval.Summary)
			if err != nil {
				failures = append(failures, err)
			} else if delivery != nil {
				result = append(result, delivery)
			}
		}
		if len(approvals) < 100 {
			break
		}
	}
	for offset := 0; ; offset += 100 {
		requests, err := store.ListPendingSkillSetupRequests(ctx, scope, 100, offset)
		if err != nil {
			return result, err
		}
		for _, request := range requests {
			delivery, err := w.projectReview(ctx, scope, "setup", request.ID, request.RunID, request.ConversationID, request.TriggerMessageID, request.Reason)
			if err != nil {
				failures = append(failures, err)
			} else if delivery != nil {
				result = append(result, delivery)
			}
		}
		if len(requests) < 100 {
			break
		}
	}
	return result, errors.Join(failures...)
}
func (w *RunProgressAcknowledgementWorker) projectReview(ctx context.Context, scope Scope, kind, id, runID, conversationID, triggerID, reason string) (*ExternalConversationDelivery, error) {
	run, err := w.store.GetAgentRun(ctx, scope, runID)
	if err != nil || run == nil {
		return nil, err
	}
	rootID := run.RootRunID
	if rootID == "" {
		rootID = run.ID
	}
	root, err := w.store.GetAgentRun(ctx, scope, rootID)
	if err != nil || root == nil {
		return nil, err
	}
	actualConversation, _ := root.Context["conversationId"].(string)
	actualTrigger, _ := root.Context["triggerMessageId"].(string)
	if actualConversation == "" || actualTrigger == "" {
		return nil, nil
	}
	if conversationID != "" && conversationID != actualConversation || triggerID != "" && triggerID != actualTrigger {
		return nil, ErrExternalConversationConflict
	}
	conversationID, triggerID = actualConversation, actualTrigger
	conversation, err := w.store.GetConversation(ctx, scope, conversationID)
	if err != nil || conversation == nil {
		return nil, err
	}
	if conversation.Owner != run.Owner || conversation.Origin == nil || conversation.Origin.Kind != ConversationReferenceExternalSource {
		return nil, nil
	}
	var origin *ExternalConversationInboxItem
	for offset := 0; ; offset += 100 {
		items, err := w.store.ListExternalConversationInbox(ctx, ExternalConversationInboxFilter{Scope: scope, EndpointID: conversation.Origin.ID, Statuses: []ExternalConversationInboxStatus{ExternalConversationInboxApplied}, Limit: 100, Offset: offset})
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if item.ConversationID == conversationID && item.ChannelMessageID == triggerID && item.RunID == rootID {
				origin = item
				break
			}
		}
		if origin != nil || len(items) < 100 {
			break
		}
	}
	if origin == nil {
		return nil, nil
	}
	endpoint, adapter, err := w.resolveEndpoint(ctx, origin)
	if err != nil || endpoint == nil {
		return nil, err
	}
	if endpoint.Owner != run.Owner || !containsConversationDeliveryOperation(adapter.Adapter.Delivery.Operations, capability.ConversationDeliveryMessageSend) {
		return nil, nil
	}
	key := "review-link:" + kind + ":" + id
	existing, err := w.store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{Scope: scope, EndpointID: endpoint.ID, CorrelationKind: "review_request", CorrelationID: kind + ":" + id, Limit: 1})
	if err != nil {
		return nil, err
	}
	if len(existing) > 0 {
		return nil, nil
	}
	link, err := w.config.ReviewURL(kind, id, endpoint.DeploymentID, conversationID, endpoint.Owner)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(link)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil {
		return nil, errors.New("invalid channel review URL")
	}
	title := "Review approval"
	if kind == "setup" {
		title = "Complete setup"
	}
	message, err := w.post(ctx, origin, endpoint, RunProgressAcknowledgement{RunID: rootID, Phase: key, Text: reason + "\n\n[" + title + "](" + link + ")"})
	if err != nil {
		return nil, err
	}
	thread := origin.Event.ExternalThreadID
	if thread == "" && endpoint.Policy.ReplyMode == ExternalConversationReplyThread {
		thread = origin.Event.ExternalMessageID
	}
	delivery, err := w.transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
		Scope: scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageSend, ConversationID: conversationID, ChannelMessageID: message.ID,
		ExternalConversationID: origin.Event.ExternalConversationID, ExternalThreadID: thread,
		Parameters:     map[string]interface{}{"reviewRequest": map[string]interface{}{"kind": kind, "id": id, "label": title, "reason": reason, "url": link}},
		IdempotencyKey: key, Correlation: &ExternalConversationDeliveryCorrelation{Kind: "review_request", ID: kind + ":" + id, Phase: "request"},
	})
	if err != nil {
		return nil, err
	}
	return delivery.Delivery, nil
}
