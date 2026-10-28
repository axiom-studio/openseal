package runtime

import (
	"context"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// A connector-channel reply streams into the chat by editing one provider
// message in place. The provisional text is the Run's public
// turn.answer_preview Activity; the committed canonical reply then finishes
// the same message. Every step is a durable outbox delivery keyed by the
// inbox item, so the streamed message's provider ID survives worker restarts.
const (
	externalAnswerStreamCorrelationKind = "answer_stream"
	externalAnswerStreamPreviewPrefix   = "preview-"
	externalAnswerStreamDeletePhase     = "delete"
	// Telegram's edit limits and Slack's chat.update tier both allow about
	// one edit per second per conversation.
	externalAnswerStreamInterval     = 1500 * time.Millisecond
	externalAnswerStreamMinimumRunes = 24
	// Provisional text stays below common chat message limits (Telegram 4096
	// UTF-16 units, Slack's recommended 4000 characters). The committed
	// reply is always delivered in full.
	externalAnswerStreamDisplayUnits = 3500
)

func externalAnswerStreamKey(itemID, phase string) string {
	return "external-conversation-answer-stream:" + itemID + ":" + phase
}

func externalConversationReplyKey(itemID string) string {
	return "external-conversation-reply-delivery:" + itemID
}

// externalAnswerStream is the durable outbox view of one item's stream.
type externalAnswerStream struct {
	opened   *ExternalConversationDelivery // preview-1, the message.send
	latest   *ExternalConversationDelivery
	sequence int
}

func loadExternalAnswerStream(ctx context.Context, store ExternalConversationStore, item *ExternalConversationInboxItem) (*externalAnswerStream, error) {
	opened, err := store.GetExternalConversationDelivery(ctx, item.Scope,
		stableExternalConversationID(item.Scope, item.EndpointID, "delivery", externalAnswerStreamKey(item.ID, externalAnswerStreamPreviewPrefix+"1")))
	if err != nil || opened == nil {
		return nil, err
	}
	latest, err := store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{
		Scope: item.Scope, EndpointID: item.EndpointID, ConversationID: item.ConversationID,
		CorrelationKind: externalAnswerStreamCorrelationKind, CorrelationID: item.ID, Limit: 1,
	})
	if err != nil {
		return nil, err
	}
	stream := &externalAnswerStream{opened: opened, latest: opened, sequence: 1}
	if len(latest) == 1 && latest[0] != nil && latest[0].Correlation != nil {
		stream.latest = latest[0]
		if sequence, err := strconv.Atoi(strings.TrimPrefix(latest[0].Correlation.Phase, externalAnswerStreamPreviewPrefix)); err == nil {
			stream.sequence = sequence
		}
	}
	return stream, nil
}

// settled reports that no stream delivery is still in flight, so a later
// delivery cannot be overtaken by an older edit.
func (s *externalAnswerStream) settled() bool {
	return externalConversationDeliveryTerminal(s.latest.Status)
}

// live reports a delivered stream message that has not been deleted.
func (s *externalAnswerStream) live() bool {
	return s.opened.Status == ExternalConversationDeliveryDelivered && s.opened.ProviderMessageID != "" &&
		s.latest.Correlation.Phase != externalAnswerStreamDeletePhase
}

func externalConversationDeliveryTerminal(status ExternalConversationDeliveryStatus) bool {
	return status == ExternalConversationDeliveryDelivered || status == ExternalConversationDeliveryFailed ||
		status == ExternalConversationDeliveryCanceled
}

// externalAnswerStreamPreview returns the provisional body of a stream
// preview delivery. The canonical trigger message only anchors it.
func externalAnswerStreamPreview(delivery *ExternalConversationDelivery) (string, bool) {
	if delivery == nil || delivery.Correlation == nil || delivery.Correlation.Kind != externalAnswerStreamCorrelationKind ||
		!strings.HasPrefix(delivery.Correlation.Phase, externalAnswerStreamPreviewPrefix) {
		return "", false
	}
	text, ok := delivery.Parameters["preview"].(string)
	return text, ok && text != ""
}

// externalAnswerStreamText is the display text for a preview, or empty while
// the preview is too short to be worth posting.
func externalAnswerStreamText(text string) string {
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) < externalAnswerStreamMinimumRunes {
		return ""
	}
	units := 0
	for i, r := range text {
		units += utf16.RuneLen(r)
		if units >= externalAnswerStreamDisplayUnits {
			return strings.TrimRightFunc(text[:i], unicode.IsSpace) + "…"
		}
	}
	return text
}

func externalConversationReplyThread(item *ExternalConversationInboxItem, endpoint *ExternalConversationEndpoint) string {
	thread := strings.TrimSpace(item.Event.ExternalThreadID)
	if thread == "" && endpoint.Policy.ReplyMode == ExternalConversationReplyThread {
		thread = strings.TrimSpace(item.Event.ExternalMessageID)
	}
	return thread
}

// projectAnswerStream posts the first meaningful preview and then edits it as
// the preview grows, at most once per interval and with one edit in flight.
// Adapters that cannot edit messages keep the single committed reply.
func (w *RunProgressAcknowledgementWorker) projectAnswerStream(ctx context.Context, item *ExternalConversationInboxItem) (*ExternalConversationDelivery, error) {
	if item == nil || item.Status != ExternalConversationInboxApplied || item.RunID == "" || item.ConversationID == "" || item.ChannelMessageID == "" {
		return nil, nil
	}
	run, err := w.store.GetAgentRun(ctx, item.Scope, item.RunID)
	if err != nil || run == nil || isTerminalAgentRunStatus(run.Status) || externalInboxItemIsFollowUp(item, run) {
		return nil, err
	}
	endpoint, adapter, err := w.resolveEndpoint(ctx, item)
	if err != nil || endpoint == nil || run.Owner != endpoint.Owner {
		return nil, err
	}
	operations := adapter.Adapter.Delivery.Operations
	if !containsConversationDeliveryOperation(operations, capability.ConversationDeliveryMessageSend) ||
		!containsConversationDeliveryOperation(operations, capability.ConversationDeliveryMessageUpdate) {
		return nil, nil
	}
	events, err := w.store.ListActivity(ctx, ActivityFilter{Scope: item.Scope, RunID: run.ID, EventTypes: []string{"turn.answer_preview"}, Descending: true, Limit: 1})
	if err != nil || len(events) == 0 {
		return nil, err
	}
	// A reset keeps the current message; the next attempt or the committed
	// reply replaces its text.
	preview := projectTurnAnswerPreview(events[0])
	if preview == nil || preview.Reset {
		return nil, nil
	}
	text := externalAnswerStreamText(preview.Text)
	if text == "" {
		return nil, nil
	}
	stream, err := loadExternalAnswerStream(ctx, w.store, item)
	if err != nil {
		return nil, err
	}
	operation := capability.ConversationDeliveryMessageSend
	phase := externalAnswerStreamPreviewPrefix + "1"
	parameters := map[string]interface{}{"preview": text}
	if stream != nil {
		if !stream.settled() || !stream.live() || w.now().UTC().Sub(stream.latest.CreatedAt) < externalAnswerStreamInterval {
			return nil, nil
		}
		if shown, _ := stream.latest.Parameters["preview"].(string); shown == text {
			return nil, nil
		}
		operation = capability.ConversationDeliveryMessageUpdate
		phase = externalAnswerStreamPreviewPrefix + strconv.Itoa(stream.sequence+1)
		parameters["providerMessageId"] = stream.opened.ProviderMessageID
	}
	// The committed reply owns the message once its delivery exists.
	final, err := w.store.GetExternalConversationDelivery(ctx, item.Scope, stableExternalConversationID(item.Scope, endpoint.ID, "delivery", externalConversationReplyKey(item.ID)))
	if err != nil || final != nil {
		return nil, err
	}
	result, err := w.transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
		Scope: item.Scope, EndpointID: endpoint.ID, Operation: operation,
		ConversationID: item.ConversationID, ChannelMessageID: item.ChannelMessageID,
		ExternalConversationID: item.Event.ExternalConversationID, ExternalThreadID: externalConversationReplyThread(item, endpoint),
		Parameters:     parameters,
		Correlation:    &ExternalConversationDeliveryCorrelation{Kind: externalAnswerStreamCorrelationKind, ID: item.ID, Phase: phase},
		IdempotencyKey: externalAnswerStreamKey(item.ID, phase),
	})
	if err != nil {
		return nil, err
	}
	return result.Delivery, nil
}

// deleteAnswerStream removes a streamed message the committed reply will not
// finish. Without a declared delete it reports false and leaves the message.
func deleteAnswerStream(
	ctx context.Context,
	transport *ExternalConversationTransportService,
	item *ExternalConversationInboxItem,
	endpoint *ExternalConversationEndpoint,
	operations []capability.ConversationDeliveryOperation,
	stream *externalAnswerStream,
) (bool, error) {
	if !containsConversationDeliveryOperation(operations, capability.ConversationDeliveryMessageDelete) {
		return false, nil
	}
	_, err := transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
		Scope: item.Scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageDelete,
		ConversationID: item.ConversationID, ChannelMessageID: item.ChannelMessageID,
		ExternalConversationID: item.Event.ExternalConversationID, ExternalThreadID: stream.opened.ExternalThreadID,
		Parameters:     map[string]interface{}{"providerMessageId": stream.opened.ProviderMessageID},
		Correlation:    &ExternalConversationDeliveryCorrelation{Kind: externalAnswerStreamCorrelationKind, ID: item.ID, Phase: externalAnswerStreamDeletePhase},
		IdempotencyKey: externalAnswerStreamKey(item.ID, externalAnswerStreamDeletePhase),
	})
	return err == nil, err
}

// retireAnswerStream deletes the stream of an item that gets no reply (a
// canceled Run or a superseded failure).
func (w *ExternalConversationReplyWorker) retireAnswerStream(ctx context.Context, item *ExternalConversationInboxItem) error {
	stream, err := loadExternalAnswerStream(ctx, w.store, item)
	if err != nil || stream == nil {
		return err
	}
	if !stream.settled() {
		return errExternalReplyPending
	}
	if !stream.live() {
		return nil
	}
	endpoint, adapter, err := w.transport.resolveActiveEndpoint(ctx, item.Scope, item.EndpointID)
	if err != nil {
		// A paused or changed endpoint cancels outstanding responses.
		return nil
	}
	_, err = deleteAnswerStream(ctx, w.transport, item, endpoint, adapter.Adapter.Delivery.Operations, stream)
	return err
}
