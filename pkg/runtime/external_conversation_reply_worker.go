package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// ExternalConversationReplyStore is the durable kernel view required to turn
// a finished conversation Run into one canonical channel reply and one
// provider delivery. It deliberately contains no provider-specific behavior.
type ExternalConversationReplyStore interface {
	ExternalConversationStore
	PortfolioStore
}

// ExternalConversationReplyWorker projects applied inbox work after its
// canonical Run finishes. Direct Agent/Team handlers normally already wrote
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
	if run == nil || (run.Status != AgentRunStatusCompleted && run.Status != AgentRunStatusFailed) {
		return nil, nil
	}
	endpoint, err := w.store.GetExternalConversationEndpoint(ctx, item.Scope, item.EndpointID)
	if err != nil {
		return nil, err
	}
	// Pausing or retiring an endpoint intentionally cancels any response that
	// has not yet crossed the external delivery boundary. Applied inbox items
	// remain immutable audit records and are revisited by this projector, so an
	// inactive endpoint is a successful no-op rather than a permanent warning.
	if endpoint != nil && endpoint.Status != ExternalConversationEndpointActive {
		return nil, nil
	}
	if !externalConversationSnapshotMatchesEndpoint(endpoint, item.Scope, item.EndpointID, item.EndpointRevision, item.Adapter, item.Event.ExternalConversationID) {
		return nil, ErrExternalConversationConflict
	}
	// Projection is already durable. Do not reinterpret an old item against a
	// newer Skill version or enqueue another status clear while new work runs.
	key := "external-conversation-reply-delivery:" + item.ID
	existingID := stableExternalConversationID(item.Scope, item.EndpointID, "delivery", key)
	existing, err := w.store.GetExternalConversationDelivery(ctx, item.Scope, existingID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.Scope != item.Scope || existing.EndpointID != item.EndpointID || existing.ConversationID != item.ConversationID ||
			existing.Operation != capability.ConversationDeliveryMessageSend || existing.IdempotencyKey != key ||
			(existing.ExternalConversationID != "" && existing.ExternalConversationID != item.Event.ExternalConversationID) ||
			(item.Event.ExternalThreadID != "" && existing.ExternalThreadID != item.Event.ExternalThreadID) ||
			existing.EndpointRevision < item.EndpointRevision || existing.Adapter.BindingRevision < item.Adapter.BindingRevision ||
			existing.Adapter.SkillID != item.Adapter.SkillID || existing.Adapter.SourceIdentity != item.Adapter.SourceIdentity ||
			!externalConversationAdapterBelongsToEndpoint(existing.Adapter, item.Adapter) {
			return nil, ErrExternalConversationConflict
		}
		return existing, nil
	}
	_, adapter, err := w.transport.resolveActiveEndpoint(ctx, item.Scope, item.EndpointID)
	if err != nil {
		return nil, err
	}
	if !externalConversationSnapshotMatchesBinding(endpoint, item.Adapter, adapter) || endpoint.Owner != run.Owner ||
		(endpoint.Handler.Kind == ExternalConversationHandlerAgent && endpoint.DeploymentID != run.AssignedAgentID) {
		return nil, ErrExternalConversationConflict
	}
	message, err := w.findCanonicalReply(ctx, item, run)
	if err != nil {
		return nil, err
	}
	if message == nil {
		if run.Status == AgentRunStatusFailed {
			superseded, checkErr := w.failureSuperseded(ctx, item, endpoint)
			if checkErr != nil {
				return nil, checkErr
			}
			if superseded {
				return nil, nil
			}
		}
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
			References:     append([]ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}}, conversationActionArtifactReferences(run)...),
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
	busy, busyErr := externalThreadHasActiveWork(ctx, w.store, run)
	if busyErr != nil || busy {
		return result.Delivery, nil
	}
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
	if run == nil {
		return "", false
	}
	if run.Status == AgentRunStatusFailed {
		// Failures may follow partially executed actions. Report only the reply
		// interruption, without exposing internal errors or guessing whether
		// those actions had external effects.
		return "I couldn’t finish this reply. Your message is saved.", true
	}
	if run.Status != AgentRunStatusCompleted {
		return "", false
	}
	reply, ok := run.Output["reply"].(string)
	reply = strings.TrimSpace(reply)
	return reply, ok && reply != "" && len(reply) <= 65536
}

// A later question or answer in the same thread makes an old failure notice
// stale. Other threads in the channel must not suppress this reply. Canceled
// runs are deliberately excluded by project: replacement turns handle them.
func (w *ExternalConversationReplyWorker) failureSuperseded(
	ctx context.Context,
	item *ExternalConversationInboxItem,
	endpoint *ExternalConversationEndpoint,
) (bool, error) {
	// Older workers acknowledged failed runs only by clearing thread status.
	// Respect that durable terminal projection rather than replaying historical
	// failures as new messages after an upgrade. New failed runs clear status
	// through the reply path and therefore never create this legacy key.
	legacyKey := "run-thread-status:" + item.ID + ":active:"
	legacyID := stableExternalConversationID(item.Scope, endpoint.ID, "delivery", legacyKey)
	legacy, err := w.store.GetExternalConversationDelivery(ctx, item.Scope, legacyID)
	if err != nil {
		return false, err
	}
	if legacy != nil && legacy.EndpointID == endpoint.ID && legacy.ConversationID == item.ConversationID &&
		legacy.ChannelMessageID == item.ChannelMessageID && legacy.Operation == capability.ConversationDeliveryTypingIndicator &&
		legacy.IdempotencyKey == legacyKey {
		return true, nil
	}
	trigger, err := w.conversations.GetChannelMessage(ctx, item.Scope, item.ConversationID, item.ChannelMessageID)
	if err != nil {
		return false, err
	}
	threadRoot := trigger.ThreadRootID
	if threadRoot == "" && (item.Event.ExternalThreadID != "" || endpoint.Policy.ReplyMode == ExternalConversationReplyThread) {
		threadRoot = trigger.ID
	}
	filter := ChannelMessageFilter{
		Scope: item.Scope, ConversationID: item.ConversationID,
		ThreadRootID: threadRoot, AfterSequence: trigger.Sequence, Limit: 500,
	}
	for {
		messages, err := w.store.ListChannelMessages(ctx, filter)
		if err != nil {
			return false, err
		}
		for _, message := range messages {
			if message == nil || message.Historical ||
				(threadRoot == "" && message.ThreadRootID != "" && message.ThreadRootID != trigger.ID) {
				continue
			}
			if message.Sender.Type == ConversationParticipantUser || message.Intent == MessageIntentAnswer {
				return true, nil
			}
		}
		if len(messages) < filter.Limit {
			return false, nil
		}
		filter.AfterSequence = messages[len(messages)-1].Sequence
	}
}
