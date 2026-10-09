package runtime

import (
	"context"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// Forward the Agent's already-rendered public updates as durable messages.
// Typing indicators remain advisory; they must not replace conversation text.
// No provider identity, internal activity, or extra model invocation is needed.
func (w *RunProgressAcknowledgementWorker) projectCommentary(ctx context.Context, item *ExternalConversationInboxItem) ([]*ExternalConversationDelivery, bool, error) {
	if item == nil || item.Status != ExternalConversationInboxApplied || item.RunID == "" || item.ConversationID == "" || item.ChannelMessageID == "" {
		return nil, false, nil
	}
	var result []*ExternalConversationDelivery
	seen := false
	var after int64
	for {
		events, err := w.store.ListActivity(ctx, ActivityFilter{Scope: item.Scope, RunID: item.RunID, EventTypes: []string{"turn.commentary"}, AfterSequence: after, Limit: w.config.PageSize})
		if err != nil {
			return result, seen, err
		}
		for _, event := range events {
			after = event.Sequence
			if event.Scope != item.Scope || event.RunID != item.RunID || event.EventType != "turn.commentary" || event.Visibility == ActivityVisibilityPrivate || strings.TrimSpace(event.Summary) == "" {
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
			run, err := w.store.GetAgentRun(ctx, item.Scope, item.RunID)
			if err != nil {
				return result, seen, err
			}
			if run == nil || run.Owner != endpoint.Owner {
				return result, seen, ErrExternalConversationConflict
			}
			if externalInboxItemIsFollowUp(item, run) {
				return nil, false, nil // The trigger's item already forwards this Run's updates.
			}
			phase := "commentary-" + event.ID
			message, err := w.post(ctx, item, endpoint, RunProgressAcknowledgement{RunID: run.ID, Phase: phase, Text: event.Summary, SourceEventID: event.ID})
			if err != nil {
				return result, seen, err
			}
			thread := strings.TrimSpace(item.Event.ExternalThreadID)
			if thread == "" && endpoint.Policy.ReplyMode == ExternalConversationReplyThread {
				thread = strings.TrimSpace(item.Event.ExternalMessageID)
			}
			delivery, err := w.transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
				Scope: item.Scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageSend,
				ConversationID: item.ConversationID, ChannelMessageID: message.ID,
				ExternalConversationID: item.Event.ExternalConversationID, ExternalThreadID: thread,
				Correlation:    &ExternalConversationDeliveryCorrelation{Kind: "run_commentary", ID: run.ID, Phase: phase},
				IdempotencyKey: "run-commentary-delivery:" + item.ID + ":" + event.ID,
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
