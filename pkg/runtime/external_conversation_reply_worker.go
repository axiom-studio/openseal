package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// ExternalConversationReplyStore is the durable kernel view required to turn
// a completed conversation Run into one canonical channel reply and one
// provider delivery. It deliberately contains no provider-specific behavior.
type ExternalConversationReplyStore interface {
	ExternalConversationStore
	PortfolioStore
}

// ExternalConversationReplyWorker projects applied inbox work after its
// canonical Run completes. Direct Agent/Team handlers normally already wrote
// the reply; event Runbooks expose the reply as their typed "reply" output.
// Both paths converge on the same idempotent Conversation message and outbox.
type ExternalConversationReplyWorker struct {
	store         ExternalConversationReplyStore
	conversations *ConversationService
	transport     *ExternalConversationTransportService
}

func NewExternalConversationReplyWorker(
	store ExternalConversationReplyStore,
	resolver ExternalConversationAdapterResolver,
) (*ExternalConversationReplyWorker, error) {
	if store == nil || resolver == nil {
		return nil, errors.New("external conversation reply store and adapter resolver are required")
	}
	return &ExternalConversationReplyWorker{
		store: store, conversations: NewConversationService(store),
		transport: NewExternalConversationTransportService(store, resolver),
	}, nil
}

// ProcessScope projects up to limit applied inbound messages. Reprocessing is
// expected: message and delivery idempotency keys make every successful pass a
// no-op after the first commit, including across process restarts.
func (w *ExternalConversationReplyWorker) ProcessScope(
	ctx context.Context,
	scope Scope,
	limit int,
) ([]*ExternalConversationDelivery, error) {
	if w == nil || w.store == nil || w.conversations == nil || w.transport == nil {
		return nil, errors.New("external conversation reply worker is not configured")
	}
	if limit <= 0 {
		limit = 100
	}
	items, err := w.store.ListExternalConversationInbox(ctx, ExternalConversationInboxFilter{
		Scope: scope, Statuses: []ExternalConversationInboxStatus{ExternalConversationInboxApplied}, Limit: limit,
	})
	if err != nil {
		return nil, err
	}
	deliveries := make([]*ExternalConversationDelivery, 0, len(items))
	var projectErrors []error
	for _, item := range items {
		delivery, projectErr := w.project(ctx, item)
		if projectErr != nil {
			projectErrors = append(projectErrors, fmt.Errorf("project external conversation inbox item %s: %w", item.ID, projectErr))
			continue
		}
		if delivery != nil {
			deliveries = append(deliveries, delivery)
		}
	}
	return deliveries, errors.Join(projectErrors...)
}

func (w *ExternalConversationReplyWorker) project(
	ctx context.Context,
	item *ExternalConversationInboxItem,
) (*ExternalConversationDelivery, error) {
	if item == nil || item.Status != ExternalConversationInboxApplied ||
		strings.TrimSpace(item.RunID) == "" || strings.TrimSpace(item.ConversationID) == "" ||
		strings.TrimSpace(item.ChannelMessageID) == "" {
		return nil, nil
	}
	run, err := w.store.GetAgentRun(ctx, item.Scope, item.RunID)
	if err != nil {
		return nil, err
	}
	if run == nil || run.Status != AgentRunStatusCompleted {
		return nil, nil
	}
	endpoint, err := w.store.GetExternalConversationEndpoint(ctx, item.Scope, item.EndpointID)
	if err != nil {
		return nil, err
	}
	if endpoint == nil || endpoint.Status != ExternalConversationEndpointActive ||
		endpoint.Revision != item.EndpointRevision ||
		!externalConversationAdapterBelongsToEndpoint(endpoint.Adapter, item.Adapter) {
		return nil, ErrExternalConversationConflict
	}
	message, err := w.findCanonicalReply(ctx, item, run)
	if err != nil {
		return nil, err
	}
	if message == nil {
		reply, ok := externalConversationRunReply(run)
		if !ok {
			return nil, fmt.Errorf("%w: completed conversation Run has no canonical reply output", ErrInvalidExternalConversation)
		}
		current, getErr := w.conversations.GetConversation(ctx, item.Scope, item.ConversationID)
		if getErr != nil {
			return nil, getErr
		}
		senderID := strings.TrimSpace(endpoint.Handler.AssignedAgentID)
		if senderID == "" {
			senderID = strings.TrimSpace(run.AssignedAgentID)
		}
		if senderID == "" {
			senderID = endpoint.Owner.ID
		}
		posted, postErr := w.conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
			Scope: item.Scope, ConversationID: item.ConversationID, ExpectedRevision: current.Revision,
			Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: senderID},
			Intent: MessageIntentAnswer, Content: reply,
			Audience:         ConversationAudience{Kind: ConversationAudienceChannel},
			ReplyToMessageID: item.ChannelMessageID, ResolvesMessageID: item.ChannelMessageID,
			References:     []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}},
			IdempotencyKey: "external-conversation-reply:" + item.ID,
		})
		if postErr != nil {
			return nil, postErr
		}
		message = posted.Message
	}
	externalThreadID := strings.TrimSpace(item.Event.ExternalThreadID)
	if externalThreadID == "" && endpoint.Policy.ReplyMode == ExternalConversationReplyThread {
		externalThreadID = strings.TrimSpace(item.Event.ExternalMessageID)
	}
	result, err := w.transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
		Scope: item.Scope, EndpointID: endpoint.ID,
		Operation:      capability.ConversationDeliveryMessageSend,
		ConversationID: item.ConversationID, ChannelMessageID: message.ID,
		ExternalConversationID: item.Event.ExternalConversationID,
		ExternalThreadID:       externalThreadID,
		IdempotencyKey:         "external-conversation-reply-delivery:" + item.ID,
	})
	if err != nil {
		return nil, err
	}
	// Thread status is advisory and provider-capability dependent. Enqueueing it
	// after the reply preserves thread ordering so providers clear "Thinking…"
	// only after the durable answer has been accepted.
	_, _ = w.transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
		Scope: item.Scope, EndpointID: endpoint.ID,
		Operation:      capability.ConversationDeliveryTypingIndicator,
		ConversationID: item.ConversationID, ChannelMessageID: message.ID,
		ExternalConversationID: item.Event.ExternalConversationID,
		ExternalThreadID:       externalThreadID,
		Parameters:             map[string]interface{}{"status": ""},
		IdempotencyKey:         "external-conversation-reply-status:" + item.ID,
	})
	return result.Delivery, nil
}

func (w *ExternalConversationReplyWorker) findCanonicalReply(
	ctx context.Context,
	item *ExternalConversationInboxItem,
	run *AgentRun,
) (*ChannelMessage, error) {
	messages, err := w.store.ListChannelMessages(ctx, ChannelMessageFilter{
		Scope: item.Scope, ConversationID: item.ConversationID, Limit: 1000,
	})
	if err != nil {
		return nil, err
	}
	for _, message := range messages {
		if message == nil || message.ReplyToMessageID != item.ChannelMessageID ||
			message.Intent != MessageIntentAnswer || message.Sender.Type == ConversationParticipantUser || message.Historical {
			continue
		}
		for _, reference := range message.References {
			if reference.Kind == ConversationReferenceRun && reference.ID == run.ID {
				return message, nil
			}
		}
	}
	return nil, nil
}

func externalConversationRunReply(run *AgentRun) (string, bool) {
	if run == nil || run.Status != AgentRunStatusCompleted {
		return "", false
	}
	reply, ok := run.Output["reply"].(string)
	reply = strings.TrimSpace(reply)
	return reply, ok && reply != "" && len(reply) <= 65536
}
