package runtime

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type channelReviewStore interface {
	ExternalConversationStore
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
			delivery, err := w.projectReview(ctx, scope, "approval", approval.ID, approval.RunID, "", "", PresentApproval(approval))
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
			delivery, err := w.projectReview(ctx, scope, "setup", request.ID, request.RunID, request.ConversationID, request.TriggerMessageID, ApprovalPresentation{Title: request.Reason})
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
	updates, err := w.syncReviewApprovalCards(ctx, scope)
	result = append(result, updates...)
	failures = append(failures, err)
	return result, errors.Join(failures...)
}

// projectReview posts a review request into the connector thread that started
// the Run. Seal Chat renders approvals and setup requests as its own cards, so
// only connector-originated conversations get this projection.
func (w *RunProgressAcknowledgementWorker) projectReview(ctx context.Context, scope Scope, kind, id, runID, conversationID, triggerID string, presentation ApprovalPresentation) (*ExternalConversationDelivery, error) {
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
	references := []ConversationReference{{Kind: ConversationReferenceRun, ID: rootID}}
	if kind == "approval" {
		references = append(references, ConversationReference{Kind: ConversationReferenceApproval, ID: id})
	}
	message, err := w.postReview(ctx, origin, references, key, presentation.Text()+"\n\n["+title+"]("+link+")")
	if err != nil {
		return nil, err
	}
	thread := origin.Event.ExternalThreadID
	if thread == "" && endpoint.Policy.ReplyMode == ExternalConversationReplyThread {
		thread = origin.Event.ExternalMessageID
	}
	review := map[string]interface{}{"kind": kind, "id": id, "label": title, "url": link, "presentation": presentation.Payload()}
	if kind == "approval" {
		store := w.store.(channelReviewStore)
		approval, err := store.GetApproval(ctx, scope, id)
		if err != nil {
			return nil, err
		}
		call, err := store.GetActionCall(ctx, scope, approval.ActionCallID)
		if err != nil {
			return nil, err
		}
		review["approval"] = channelReviewApprovalPayload(approval, call)
	}
	delivery, err := w.transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
		Scope: scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageSend, ConversationID: conversationID, ChannelMessageID: message.ID,
		ExternalConversationID: origin.Event.ExternalConversationID, ExternalThreadID: thread,
		Parameters:     map[string]interface{}{"reviewRequest": review},
		IdempotencyKey: key, Correlation: &ExternalConversationDeliveryCorrelation{Kind: "review_request", ID: kind + ":" + id, Phase: "request"},
	})
	if err != nil {
		return nil, err
	}
	return delivery.Delivery, nil
}

// ChannelReviewParticipantID sends review-request projections. Seal Chat shows
// its own approval and setup cards and hides these messages; they never wake
// the owning Agent.
const ChannelReviewParticipantID = "openseal.channel-review"

func (w *RunProgressAcknowledgementWorker) postReview(ctx context.Context, item *ExternalConversationInboxItem, references []ConversationReference, key, text string) (*ChannelMessage, error) {
	for range 3 {
		conversation, err := w.conversations.GetConversation(ctx, item.Scope, item.ConversationID)
		if err != nil {
			return nil, err
		}
		posted, err := w.conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
			Scope: item.Scope, ConversationID: item.ConversationID, ExpectedRevision: conversation.Revision,
			Sender: ConversationParticipant{Type: ConversationParticipantService, ID: ChannelReviewParticipantID},
			Intent: MessageIntentUpdate, Content: text,
			Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: item.ChannelMessageID,
			References: references, IdempotencyKey: "channel-review-message:" + item.ID + ":" + key,
		})
		if err == nil {
			return posted.Message, nil
		}
		if !errors.Is(err, ErrRevisionConflict) && !errors.Is(err, ErrMessageConflict) {
			return nil, err
		}
	}
	return nil, ErrRevisionConflict
}

// Decision metadata describes the exact reviewed invocation, never credentials.
func channelReviewApprovalPayload(approval *ApprovalCheckpoint, call *ActionCall) map[string]interface{} {
	return map[string]interface{}{"id": approval.ID, "revision": approval.Revision, "actionCallId": approval.ActionCallID, "invocationDigest": call.InvocationDigest, "expiresAt": approval.ExpiresAt, "status": approval.Status}
}

func (w *RunProgressAcknowledgementWorker) syncReviewApprovalCards(ctx context.Context, scope Scope) ([]*ExternalConversationDelivery, error) {
	store := w.store.(channelReviewStore)
	var updates []*ExternalConversationDelivery
	for offset := 0; ; offset += 100 {
		originals, err := store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{Scope: scope, CorrelationKind: "review_request", Statuses: []ExternalConversationDeliveryStatus{ExternalConversationDeliveryDelivered}, Limit: 100, Offset: offset})
		if err != nil {
			return updates, err
		}
		for _, original := range originals {
			if original.Operation != capability.ConversationDeliveryMessageSend || original.Correlation == nil || original.Correlation.Phase != "request" || !strings.HasPrefix(original.Correlation.ID, "approval:") {
				continue
			}
			id := strings.TrimPrefix(original.Correlation.ID, "approval:")
			approval, err := store.GetApproval(ctx, scope, id)
			if err != nil {
				return updates, err
			}
			if approval.Status == ApprovalStatusPending {
				continue
			}
			phase := "resolved:" + strconv.FormatInt(approval.Revision, 10)
			existing, err := store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{Scope: scope, EndpointID: original.EndpointID, CorrelationKind: "review_request", CorrelationID: original.Correlation.ID, Limit: 100})
			if err != nil {
				return updates, err
			}
			found := false
			for _, delivery := range existing {
				if delivery.Correlation != nil && delivery.Correlation.Phase == phase {
					found = true
					break
				}
			}
			if found {
				continue
			}
			endpoint, adapter, err := w.transport.resolveActiveEndpoint(ctx, scope, original.EndpointID)
			if err != nil {
				return updates, err
			}
			if !containsConversationDeliveryOperation(adapter.Adapter.Delivery.Operations, capability.ConversationDeliveryMessageUpdate) {
				continue
			}
			call, err := store.GetActionCall(ctx, scope, approval.ActionCallID)
			if err != nil {
				return updates, err
			}
			parameters := cloneMap(original.Parameters)
			review, ok := parameters["reviewRequest"].(map[string]interface{})
			if !ok {
				continue
			}
			review["approval"] = channelReviewApprovalPayload(approval, call)
			review["presentation"] = PresentApproval(approval).Payload()
			parameters["providerMessageId"] = original.ProviderMessageID
			result, err := w.transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{Scope: scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageUpdate, ConversationID: original.ConversationID, ChannelMessageID: original.ChannelMessageID, ExternalConversationID: original.ExternalConversationID, ExternalThreadID: original.ExternalThreadID, Parameters: parameters, IdempotencyKey: "review-card:" + id + ":" + endpoint.ID + ":" + phase, Correlation: &ExternalConversationDeliveryCorrelation{Kind: "review_request", ID: original.Correlation.ID, Phase: phase}})
			if err != nil {
				return updates, err
			}
			updates = append(updates, result.Delivery)
		}
		if len(originals) < 100 {
			break
		}
	}
	return updates, nil
}
