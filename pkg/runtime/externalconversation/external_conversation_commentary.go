package externalconversation

import (
	"context"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
)

// Forward the Agent's already-rendered public updates as durable messages.
// Typing indicators remain advisory; they must not replace conversation text.
// No provider identity, internal activity, or extra model invocation is needed.
// Each Team member forwards its own updates under its own name.
func (w *RunProgressAcknowledgementWorker) projectCommentary(ctx context.Context, item *models.ExternalConversationInboxItem, reply externalItemReply) ([]*models.ExternalConversationDelivery, bool, error) {
	if item == nil || item.Status != models.ExternalConversationInboxApplied || item.RunID == "" || item.ConversationID == "" || item.ChannelMessageID == "" {
		return nil, false, nil
	}
	run := reply.run
	var result []*models.ExternalConversationDelivery
	seen := false
	var after int64
	for {
		events, err := w.store.ListActivity(ctx, models.ActivityFilter{Scope: item.Scope, RunID: run.ID, EventTypes: []string{"turn.commentary"}, AfterSequence: after, Limit: w.config.PageSize})
		if err != nil {
			return result, seen, err
		}
		for _, event := range events {
			after = event.Sequence
			if event.Scope != item.Scope || event.RunID != run.ID || event.EventType != "turn.commentary" || event.Visibility == models.ActivityVisibilityPrivate || strings.TrimSpace(event.Summary) == "" {
				continue
			}
			seen = true
			endpoint, adapter, err := w.resolveEndpoint(ctx, item)
			if err != nil || endpoint == nil {
				return result, seen, err
			}
			if !containsConversationDeliveryOperation(adapter.Adapter.Delivery.Operations, capability.ConversationDeliveryMessageSend) {
				return result, seen, nil
			}
			if run.Owner != endpoint.Owner {
				return result, seen, models.ErrExternalConversationConflict
			}
			if conversationrun.ExternalInboxItemIsFollowUp(item, run) {
				return nil, false, nil // The trigger's item already forwards this Run's updates.
			}
			if strings.TrimSpace(run.AssignedAgentID) == "" {
				continue // Only an Agent's own turn has commentary to forward.
			}
			phase := "commentary-" + event.ID
			message, err := w.post(ctx, item, reply.id, RunProgressAcknowledgement{RunID: run.ID, Phase: phase, Text: event.Summary, SourceEventID: event.ID}, run.AssignedAgentID)
			if err != nil {
				return result, seen, err
			}
			thread := strings.TrimSpace(item.Event.ExternalThreadID)
			if thread == "" && endpoint.Policy.ReplyMode == models.ExternalConversationReplyThread {
				thread = strings.TrimSpace(item.Event.ExternalMessageID)
			}
			delivery, err := w.transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
				Scope: item.Scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageSend,
				ConversationID: item.ConversationID, ChannelMessageID: message.ID,
				ExternalConversationID: item.Event.ExternalConversationID, ExternalThreadID: thread,
				Correlation:    &models.ExternalConversationDeliveryCorrelation{Kind: "run_commentary", ID: run.ID, Phase: phase},
				IdempotencyKey: "run-commentary-delivery:" + reply.id + ":" + event.ID,
			})
			if err != nil {
				return result, seen, err
			}
			result = append(result, delivery.Delivery)
		}
		if len(events) < w.config.PageSize {
			return result, seen, nil
		}
	}
}
