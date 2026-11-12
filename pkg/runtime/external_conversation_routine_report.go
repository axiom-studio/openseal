package runtime

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
)

// routineReportDeliveryKey is the outbox identity of one routine occurrence
// report. The report message itself is unique per Run and terminal status.
func routineReportDeliveryKey(messageID string) string {
	return "routine-report-delivery:" + messageID
}

// routineReportHeaderParameter carries an event occurrence's header line on
// its mirror delivery. The external chat has no trail of what started the
// run, so the mirrored report starts with it; the HelloSeal message itself
// is unchanged (the UI reads the header from the Run).
const routineReportHeaderParameter = "routineEventHeader"

// maxRoutineEventHeaderBytes is the longest header the mirror sends.
const maxRoutineEventHeaderBytes = 200

// routineReportHeader is the run's one-line event header, or "". The text
// comes from outside (an email subject, a summary), so mention syntax is
// neutralized before it reaches a chat, and a long header is cut to fit.
func routineReportHeader(run *AgentRun) string {
	header, _ := run.Context[RoutineEventHeaderContextKey].(string)
	header = neutralizeChatMentions(strings.Join(strings.Fields(header), " "))
	if len(header) <= maxRoutineEventHeaderBytes {
		return header
	}
	const ellipsis = "…"
	cut := maxRoutineEventHeaderBytes - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(header[cut]) {
		cut--
	}
	return strings.TrimSpace(header[:cut]) + ellipsis
}

// chatMentionNeutralizer stops outside text from pinging people in a mirrored
// chat: Slack reads <!channel>, <!here> and <@U…> as mentions, Slack and
// Telegram read @name. Angle brackets become look-alike quotes and a
// zero-width space follows every @.
var chatMentionNeutralizer = strings.NewReplacer("<", "‹", ">", "›", "@", "@\u200b")

func neutralizeChatMentions(text string) string {
	return chatMentionNeutralizer.Replace(text)
}

// withRoutineReportHeader returns message with the delivery's header line
// before its content, or message unchanged when there is none.
func withRoutineReportHeader(delivery *ExternalConversationDelivery, message *ChannelMessage) *ChannelMessage {
	if delivery.Operation != capability.ConversationDeliveryMessageSend {
		return message
	}
	header, _ := delivery.Parameters[routineReportHeaderParameter].(string)
	if header = strings.TrimSpace(header); header == "" {
		return message
	}
	headed := cloneChannelMessage(message)
	headed.Content = header + "\n" + message.Content
	return headed
}

// projectRoutineReportDelivery sends a routine occurrence's terminal report to
// the external conversation the routine was created from. The reply worker
// answers only inbound messages; an occurrence answers none, so its report
// would otherwise stay in the mirrored Conversation. A routine occurrence's
// report is a new top-level message in that conversation, as it is in the
// mirrored Conversation, whatever the endpoint's reply mode.
//
// The destination is proven only from kernel records: the Conversation's
// endpoint origin, the creating message's provider mapping and the active
// endpoint's owner and Agent. Anything else is a no-op, as for the reply
// worker; only store failures are returned for the reporting worker to retry.
func projectRoutineReportDelivery(ctx context.Context, store ExternalConversationStore, transport *ExternalConversationTransportService, run *AgentRun) (*ExternalConversationDelivery, error) {
	if store == nil || transport == nil || run == nil || !isTerminalAgentRunStatus(run.Status) ||
		run.Context[scheduledTaskContextKey] != true || run.Context[ConversationTaskContextKey] != nil || !runNeedsTerminalReporting(run) ||
		conversationrun.SilentRoutineOccurrence(run) {
		return nil, nil
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	sourceID, _ := run.Context[conversationRunContextTriggerID].(string)
	conversation, err := store.GetConversation(ctx, run.Scope, conversationID)
	if err != nil || conversation == nil {
		return nil, ignoreMissingConversation(err)
	}
	if conversation.Owner != run.Owner || conversation.Origin == nil || conversation.Origin.Kind != ConversationReferenceExternalSource {
		return nil, nil
	}
	report, err := store.FindChannelMessageByIdempotencyKey(ctx, run.Scope, conversation.ID, "run-reporting-terminal:"+run.ID+":"+string(run.Status))
	if err != nil || report == nil {
		return nil, ignoreMissingConversation(err)
	}
	if !routineReportMessage(report, run) {
		return nil, nil
	}
	endpoint, err := store.GetExternalConversationEndpoint(ctx, run.Scope, conversation.Origin.ID)
	if err != nil || endpoint == nil {
		return nil, ignoreMissingConversation(err)
	}
	if endpoint.Status != ExternalConversationEndpointActive || endpoint.Owner != run.Owner ||
		(endpoint.Handler.Kind == ExternalConversationHandlerAgent && endpoint.DeploymentID != run.AssignedAgentID) {
		return nil, nil
	}
	key := routineReportDeliveryKey(report.ID)
	existing, err := store.GetExternalConversationDelivery(ctx, run.Scope, stableExternalConversationID(run.Scope, endpoint.ID, "delivery", key))
	if err != nil || existing != nil {
		return existing, err
	}
	source, err := store.GetChannelMessage(ctx, run.Scope, conversation.ID, sourceID)
	if err != nil || source == nil {
		return nil, ignoreMissingConversation(err)
	}
	externalConversationID, err := routineReportDestination(ctx, store, endpoint, conversation, source)
	if err != nil || externalConversationID == "" {
		return nil, err
	}
	_, adapter, err := transport.resolveActiveEndpoint(ctx, run.Scope, endpoint.ID)
	if err != nil {
		return nil, ignoreUndeliverable(err)
	}
	if !containsConversationDeliveryOperation(adapter.Adapter.Delivery.Operations, capability.ConversationDeliveryMessageSend) {
		return nil, nil
	}
	request := EnqueueExternalConversationDeliveryRequest{
		Scope: run.Scope, EndpointID: endpoint.ID, Operation: capability.ConversationDeliveryMessageSend,
		ConversationID: conversation.ID, ChannelMessageID: report.ID,
		ExternalConversationID: externalConversationID,
		IdempotencyKey:         key,
	}
	if header := routineReportHeader(run); header != "" {
		request.Parameters = map[string]interface{}{routineReportHeaderParameter: header}
	}
	result, err := transport.Enqueue(ctx, request)
	if err != nil {
		return nil, ignoreUndeliverable(err)
	}
	return result.Delivery, nil
}

// routineReportMessage is the Agent's top-level terminal report for this Run.
func routineReportMessage(message *ChannelMessage, run *AgentRun) bool {
	if message == nil || message.Historical || message.Sender.Type != ConversationParticipantAgent || message.Sender.ID != run.AssignedAgentID ||
		message.ReplyToMessageID != "" || message.ThreadRootID != "" ||
		message.Audience.Kind != ConversationAudienceChannel || strings.TrimSpace(message.Content) == "" {
		return false
	}
	for _, reference := range message.References {
		if reference.Kind == ConversationReferenceRun && reference.ID == run.ID {
			return true
		}
	}
	return false
}

// routineReportDestination resolves the provider conversation of the
// routine's creating message. The report goes there top level. The mappings
// written when that message arrived must agree.
func routineReportDestination(ctx context.Context, store ExternalConversationStore, endpoint *ExternalConversationEndpoint, conversation *Conversation, source *ChannelMessage) (string, error) {
	origin := source.ExternalSource
	if source.Historical || source.Sender.Type != ConversationParticipantUser || origin == nil || !validExternalConversationReference(origin.ChannelID, 1024) {
		return "", nil
	}
	base, err := store.GetExternalConversationMapping(ctx, endpoint.Scope, endpoint.ID, origin.ChannelID, "")
	if err != nil || base == nil || base.ConversationID != conversation.ID {
		return "", err
	}
	// The creating message must have arrived through this endpoint.
	mapping, err := store.GetExternalMessageMapping(ctx, endpoint.Scope, endpoint.ID, ExternalMessageInbound, origin.MessageID)
	if err != nil || mapping == nil || mapping.ConversationID != conversation.ID || mapping.ChannelMessageID != source.ID {
		return "", err
	}
	return origin.ChannelID, nil
}

func ignoreMissingConversation(err error) error {
	if errors.Is(err, ErrConversationNotFound) || errors.Is(err, ErrChannelMessageNotFound) || errors.Is(err, ErrExternalConversationEndpointNotFound) {
		return nil
	}
	return err
}

// An endpoint whose adapter binding changed, or that can no longer take this
// message, keeps the report in the Conversation; retrying cannot fix it.
func ignoreUndeliverable(err error) error {
	if errors.Is(err, ErrExternalConversationConflict) || errors.Is(err, ErrExternalConversationEndpointNotFound) || errors.Is(err, ErrInvalidExternalConversation) {
		return nil
	}
	return err
}
