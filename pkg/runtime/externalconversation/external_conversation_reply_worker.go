package externalconversation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
)

// ReplyWorker projects applied inbox work after its
// canonical Run finishes. Direct Agent/Team handlers normally already wrote
// the reply; event Runbooks expose the reply as their typed "reply" output.
// Both paths converge on the same idempotent Conversation message and outbox.
type ReplyWorker struct {
	store         models.ExternalConversationReplyStore
	conversations *conversation.Service
	transport     *TransportService
	now           func() time.Time
}

const (
	// A failed projection is retried with exponential backoff and abandoned
	// (ReplyState failed) after this many attempts.
	externalConversationReplyMaximumAttempts = 8
	externalConversationReplyBaseRetry       = 2 * time.Second
	externalConversationReplyMaximumRetry    = 5 * time.Minute
)

func externalConversationReplyRetryDelay(attempt int) time.Duration {
	delay := externalConversationReplyBaseRetry
	for i := 1; i < attempt && delay < externalConversationReplyMaximumRetry; i++ {
		delay *= 2
	}
	return min(delay, externalConversationReplyMaximumRetry)
}

func NewReplyWorker(
	store models.ExternalConversationReplyStore,
	resolver AdapterResolver,
	opts ...models.ServiceOption,
) (*ReplyWorker, error) {
	if store == nil || resolver == nil {
		return nil, errors.New("external conversation reply store and adapter resolver are required")
	}
	return &ReplyWorker{
		store: store, conversations: conversation.NewService(store, opts...),
		transport: NewTransportService(store, resolver, opts...),
		now:       models.ClockFrom(opts),
	}, nil
}

// ProcessScope projects up to limit applied inbound messages. Message and
// delivery idempotency keys make a repeated projection a no-op. When the
// store records reply progress, a resolved item is marked terminal and a
// failing one backs off, so idle scopes stop re-projecting every applied
// item on every pass; other stores keep the original revisit behaviour.
func (w *ReplyWorker) ProcessScope(
	ctx context.Context,
	scope models.Scope,
	limit int,
) ([]*models.ExternalConversationDelivery, error) {
	if w == nil || w.store == nil || w.conversations == nil || w.transport == nil {
		return nil, errors.New("external conversation reply worker is not configured")
	}
	if limit <= 0 {
		limit = 100
	}
	progress, tracked := w.store.(models.ExternalConversationReplyStateStore)
	now := w.now().UTC()
	filter := models.ExternalConversationInboxFilter{
		Scope: scope, Statuses: []models.ExternalConversationInboxStatus{models.ExternalConversationInboxApplied}, Limit: limit,
	}
	if tracked {
		filter.ReplyDueAt = &now
	}
	items, err := w.store.ListExternalConversationInbox(ctx, filter)
	if err != nil {
		return nil, err
	}
	deliveries := make([]*models.ExternalConversationDelivery, 0, len(items))
	var projectErrors []error
	for _, item := range items {
		projected, resolved, projectErr := w.project(ctx, item)
		if projectErr != nil {
			projectErrors = append(projectErrors, fmt.Errorf("project external conversation inbox item %s: %w", item.ID, projectErr))
			if tracked {
				w.recordFailure(ctx, progress, item, now, projectErr)
			}
			continue
		}
		deliveries = append(deliveries, projected...)
		if tracked && resolved {
			// A lost race only means another worker recorded it first.
			_ = progress.SaveExternalConversationReplyProgress(ctx, item.Scope, item.ID, item.Revision, models.ExternalConversationReplyProgress{
				State: models.ExternalConversationReplyProjected, Attempts: item.ReplyAttempts, At: now,
			})
		}
	}
	return deliveries, errors.Join(projectErrors...)
}

func (w *ReplyWorker) recordFailure(ctx context.Context, progress models.ExternalConversationReplyStateStore, item *models.ExternalConversationInboxItem, now time.Time, cause error) {
	attempts := item.ReplyAttempts + 1
	next := models.ExternalConversationReplyProgress{Attempts: attempts, Error: cause.Error(), At: now}
	if attempts >= externalConversationReplyMaximumAttempts {
		next.State = models.ExternalConversationReplyFailed
	} else {
		retryAt := now.Add(externalConversationReplyRetryDelay(attempts))
		next.AvailableAt = &retryAt
	}
	_ = progress.SaveExternalConversationReplyProgress(ctx, item.Scope, item.ID, item.Revision, next)
}

// project returns resolved=true when no further projection can ever be
// needed for the item: its replies exist or were delivered, or no reply is
// due (no Run, a canceled Run, a paused endpoint, a superseded failure).
func (w *ReplyWorker) project(
	ctx context.Context,
	item *models.ExternalConversationInboxItem,
) ([]*models.ExternalConversationDelivery, bool, error) {
	deliveries, err := w.projectItem(ctx, item)
	if errors.Is(err, errExternalReplyPending) {
		return deliveries, false, nil
	}
	return deliveries, err == nil, err
}

// errExternalReplyPending marks an item whose Run has not finished yet.
var errExternalReplyPending = errors.New("external conversation reply pending")

// projectItem delivers each answer the item has: one for an Agent's Run,
// one per member for a Team's. Each member answer has its own keys.
func (w *ReplyWorker) projectItem(
	ctx context.Context,
	item *models.ExternalConversationInboxItem,
) ([]*models.ExternalConversationDelivery, error) {
	if item == nil {
		return nil, nil
	}
	if item.Status != models.ExternalConversationInboxApplied ||
		strings.TrimSpace(item.RunID) == "" || strings.TrimSpace(item.ConversationID) == "" ||
		strings.TrimSpace(item.ChannelMessageID) == "" {
		return nil, nil
	}
	replies, err := externalItemReplies(ctx, w.store, item)
	if err != nil {
		return nil, err
	}
	agents := externalItemReplyAgents(replies)
	var deliveries []*models.ExternalConversationDelivery
	var failures []error
	pending := false
	for _, reply := range replies {
		delivery, err := w.projectReply(ctx, item, reply, agents)
		switch {
		case errors.Is(err, errExternalReplyPending):
			pending = true
		case err != nil:
			failures = append(failures, err)
		case delivery != nil:
			deliveries = append(deliveries, delivery)
		}
	}
	if err := errors.Join(failures...); err != nil {
		return nil, err
	}
	if pending {
		return deliveries, errExternalReplyPending
	}
	return deliveries, nil
}

func (w *ReplyWorker) projectReply(
	ctx context.Context,
	item *models.ExternalConversationInboxItem,
	reply externalItemReply,
	agents []string,
) (*models.ExternalConversationDelivery, error) {
	run := reply.run
	if run.Status == models.AgentRunStatusCanceled {
		return nil, w.retireAnswerStream(ctx, item, reply.id)
	}
	if conversationrun.ExternalInboxItemIsFollowUp(item, run) {
		return nil, nil // The Run's own trigger item delivers its single reply.
	}
	if run.Status != models.AgentRunStatusCompleted && run.Status != models.AgentRunStatusFailed {
		return nil, errExternalReplyPending
	}
	endpoint, err := w.store.GetExternalConversationEndpoint(ctx, item.Scope, item.EndpointID)
	if err != nil {
		return nil, err
	}
	// Pausing or retiring an endpoint intentionally cancels any response that
	// has not yet crossed the external delivery boundary. Applied inbox items
	// remain immutable audit records and are revisited by this projector, so an
	// inactive endpoint is a successful no-op rather than a permanent warning.
	if endpoint != nil && endpoint.Status != models.ExternalConversationEndpointActive {
		return nil, nil
	}
	if !externalConversationSnapshotMatchesEndpoint(endpoint, item.Scope, item.EndpointID, item.EndpointRevision, item.Adapter, item.Event.ExternalConversationID) {
		return nil, models.ErrExternalConversationConflict
	}
	// Projection is already durable. Do not reinterpret an old item against a
	// newer Skill version or enqueue another status clear while new work runs.
	key := externalConversationReplyKey(reply.id)
	existing, err := w.store.GetExternalConversationDelivery(ctx, item.Scope, stableExternalConversationID(item.Scope, item.EndpointID, "delivery", key))
	if err != nil {
		return nil, err
	}
	resend := false
	if existing != nil && existing.Operation == capability.ConversationDeliveryMessageUpdate {
		// A reply that finishes a streamed message stays pending until the edit
		// settles. If the edit failed (the chat message was deleted, say), the
		// reply is posted as a new message instead.
		if existing.Status == models.ExternalConversationDeliveryDelivered {
			return existing, nil
		}
		if !externalConversationDeliveryTerminal(existing.Status) {
			return nil, errExternalReplyPending
		}
		key, resend = key+":resend", true
		existing, err = w.store.GetExternalConversationDelivery(ctx, item.Scope, stableExternalConversationID(item.Scope, item.EndpointID, "delivery", key))
		if err != nil {
			return nil, err
		}
	}
	if existing != nil {
		if existing.Scope != item.Scope || existing.EndpointID != item.EndpointID || existing.ConversationID != item.ConversationID ||
			existing.Operation != capability.ConversationDeliveryMessageSend || existing.IdempotencyKey != key ||
			(existing.ExternalConversationID != "" && existing.ExternalConversationID != item.Event.ExternalConversationID) ||
			(item.Event.ExternalThreadID != "" && existing.ExternalThreadID != item.Event.ExternalThreadID) ||
			existing.EndpointRevision < item.EndpointRevision || existing.Adapter.BindingRevision < item.Adapter.BindingRevision ||
			existing.Adapter.SkillID != item.Adapter.SkillID || existing.Adapter.SourceIdentity != item.Adapter.SourceIdentity ||
			!models.ExternalConversationAdapterBelongsToEndpoint(existing.Adapter, item.Adapter) {
			return nil, models.ErrExternalConversationConflict
		}
		return existing, nil
	}
	_, adapter, err := w.transport.resolveActiveEndpoint(ctx, item.Scope, item.EndpointID)
	if err != nil {
		return nil, err
	}
	if !externalConversationSnapshotMatchesBinding(endpoint, item.Adapter, adapter) || endpoint.Owner != run.Owner ||
		(endpoint.Handler.Kind == models.ExternalConversationHandlerAgent && endpoint.DeploymentID != run.AssignedAgentID) {
		return nil, models.ErrExternalConversationConflict
	}
	if models.FailedConversationReportCandidate(run) {
		// Metadata-only updates are not new attempts. Resolve the captured
		// terminal identity instead of minting a reply for a mutable revision.
		if reports, ok := w.store.(interface {
			GetRunTerminalReport(context.Context, models.Scope, string, models.AgentRunStatus) (*models.RunTerminalReport, error)
		}); ok {
			run, err = externalConversationFailureReportRun(ctx, reports, run)
			if err != nil {
				return nil, err
			}
		}
		conversationID, conversationOK := run.Context[models.ConversationRunConversationIDContextKey].(string)
		triggerID, triggerOK := run.Context[models.ConversationRunTriggerIDContextKey].(string)
		if !conversationOK || !triggerOK || conversationID != item.ConversationID || triggerID != item.ChannelMessageID {
			// An acknowledged report releases its snapshot. Its immutable
			// inbox source still fences projection from mutable Run context.
			return nil, models.ErrExternalConversationConflict
		}
		// Both workers publish through the same revision-specific failure key.
		// The provider outbox then delivers that saved Agent answer once.
		if err := conversationrun.ProjectFailedConversationReply(ctx, w.store, run); err != nil {
			return nil, err
		}
	}
	message, err := w.findCanonicalReply(ctx, item, run)
	if err != nil {
		return nil, err
	}
	if message == nil {
		if run.Status == models.AgentRunStatusFailed {
			member := ""
			if models.IsTeamMemberRun(run) {
				member = run.AssignedAgentID
			}
			superseded, checkErr := w.failureSuperseded(ctx, item, endpoint, member)
			if checkErr != nil {
				return nil, checkErr
			}
			if superseded {
				return nil, w.retireAnswerStream(ctx, item, reply.id)
			}
		}
		content, ok := externalConversationRunReply(ctx, w.store, w.store, run)
		if !ok {
			return nil, fmt.Errorf("%w: completed conversation Run has no canonical reply output", models.ErrInvalidExternalConversation)
		}
		saved, refErr := conversation.RunActionArtifactReferences(ctx, w.store, w.store, run)
		if refErr != nil {
			return nil, refErr
		}
		artifacts, _ := conversation.HandedOverArtifacts(saved, run.Output)
		if catalog, ok := any(w.store).(models.ArtifactStore); ok {
			_ = models.MarkRunFiles(ctx, catalog, run.Scope, saved, artifacts)
		}
		current, getErr := w.conversations.GetConversation(ctx, item.Scope, item.ConversationID)
		if getErr != nil {
			return nil, getErr
		}
		senderID := strings.TrimSpace(run.AssignedAgentID)
		if senderID == "" {
			senderID = endpoint.Owner.ID
		}
		posted, postErr := w.conversations.PostChannelMessage(ctx, conversation.PostChannelMessageRequest{
			Scope: item.Scope, ConversationID: item.ConversationID, ExpectedRevision: current.Revision,
			Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: senderID},
			Intent: models.MessageIntentAnswer, Content: content,
			Audience:         models.ConversationAudience{Kind: models.ConversationAudienceChannel},
			ReplyToMessageID: item.ChannelMessageID, ResolvesMessageID: item.ChannelMessageID,
			References:     append([]models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: run.ID}}, artifacts...),
			IdempotencyKey: "external-conversation-reply:" + reply.id,
		})
		if postErr != nil {
			return nil, postErr
		}
		message = posted.Message
	}
	externalThreadID := externalConversationReplyThread(item, endpoint)
	request := EnqueueExternalConversationDeliveryRequest{
		Scope: item.Scope, EndpointID: endpoint.ID,
		Operation:      capability.ConversationDeliveryMessageSend,
		ConversationID: item.ConversationID, ChannelMessageID: message.ID,
		ExternalConversationID: item.Event.ExternalConversationID,
		ExternalThreadID:       externalThreadID,
		IdempotencyKey:         key,
	}
	if !resend {
		if err := w.finishAnswerStream(ctx, item, reply.id, endpoint, adapter.Adapter, run, message, &request); err != nil {
			return nil, err
		}
	}
	result, err := w.transport.Enqueue(ctx, request)
	if err != nil {
		return nil, err
	}
	// Thread status is advisory and provider-capability dependent. Enqueueing it
	// after the reply preserves thread ordering so providers clear "Thinking…"
	// only after the durable answer has been accepted.
	busy, busyErr := externalThreadHasActiveWork(ctx, w.store, run, agents...)
	if busyErr == nil && !busy {
		_, _ = w.transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
			Scope: item.Scope, EndpointID: endpoint.ID,
			Operation:      capability.ConversationDeliveryTypingIndicator,
			ConversationID: item.ConversationID, ChannelMessageID: message.ID,
			ExternalConversationID: item.Event.ExternalConversationID,
			ExternalThreadID:       externalThreadID,
			Parameters:             map[string]interface{}{"status": ""},
			IdempotencyKey:         "external-conversation-reply-status:" + reply.id,
		})
	}
	if request.Operation == capability.ConversationDeliveryMessageUpdate {
		return nil, errExternalReplyPending
	}
	return result.Delivery, nil
}

// finishAnswerStream turns the committed reply into an edit of the message
// its preview streamed into. A failed turn's partial preview, or a reply
// with files an edit cannot carry, is deleted when the adapter can; the reply
// is then posted as usual.
func (w *ReplyWorker) finishAnswerStream(
	ctx context.Context,
	item *models.ExternalConversationInboxItem,
	replyID string,
	endpoint *models.ExternalConversationEndpoint,
	adapter capability.ConversationAdapter,
	run *models.AgentRun,
	message *models.ChannelMessage,
	request *EnqueueExternalConversationDeliveryRequest,
) error {
	stream, err := loadExternalAnswerStream(ctx, w.store, item, replyID)
	if err != nil || stream == nil {
		return err
	}
	if !stream.settled() {
		return errExternalReplyPending
	}
	if !stream.live() || !containsConversationDeliveryOperation(adapter.Delivery.Operations, capability.ConversationDeliveryMessageUpdate) {
		return nil
	}
	files := false
	if containsConversationFeature(adapter.Features, capability.ConversationFeatureAttachments) {
		for _, reference := range message.References {
			files = files || reference.Kind == models.ConversationReferenceArtifact
		}
	}
	if run.Status == models.AgentRunStatusFailed || files {
		deleted, err := deleteAnswerStream(ctx, w.transport, item, replyID, endpoint, adapter.Delivery.Operations, stream)
		if err != nil || deleted || files {
			return err
		}
	}
	request.Operation = capability.ConversationDeliveryMessageUpdate
	request.Parameters = map[string]interface{}{"providerMessageId": stream.opened.ProviderMessageID}
	request.ExternalThreadID = stream.opened.ExternalThreadID
	return nil
}

func (w *ReplyWorker) findCanonicalReply(
	ctx context.Context,
	item *models.ExternalConversationInboxItem,
	run *models.AgentRun,
) (*models.ChannelMessage, error) {
	// The canonical projector also prefers a committed normal answer. In
	// particular, an older failed manual attempt must not win a history scan.
	message, err := w.store.FindChannelMessageByIdempotencyKey(ctx, item.Scope, item.ConversationID, models.ConversationTaskFinalResponseKey(run))
	if err != nil && !errors.Is(err, models.ErrChannelMessageNotFound) {
		return nil, err
	}
	if message != nil {
		if !canonicalExternalRunReply(message, item, run) {
			return nil, models.ErrExternalConversationConflict
		}
		return message, nil
	}
	if run.Status == models.AgentRunStatusFailed {
		message, err := w.store.FindChannelMessageByIdempotencyKey(ctx, item.Scope, item.ConversationID, conversationrun.TerminalConversationFailureReplyKey(run))
		if err != nil && !errors.Is(err, models.ErrChannelMessageNotFound) {
			return nil, err
		}
		if message != nil {
			if !canonicalExternalRunReply(message, item, run) {
				return nil, models.ErrExternalConversationConflict
			}
			return message, nil
		}
	}
	if messageID, _ := run.Output["messageId"].(string); strings.TrimSpace(messageID) != "" {
		message, err := w.store.GetChannelMessage(ctx, item.Scope, item.ConversationID, messageID)
		if err != nil && !errors.Is(err, models.ErrChannelMessageNotFound) {
			return nil, err
		}
		if message != nil && message.ID == messageID && canonicalExternalRunReply(message, item, run) {
			return message, nil
		}
	}
	// Older outputs may not carry messageId. Stores cap page sizes, so scan
	// actual pages rather than assuming one large limit includes the answer.
	filter := models.ChannelMessageFilter{Scope: item.Scope, ConversationID: item.ConversationID, Limit: 100}
	for {
		messages, err := w.store.ListChannelMessages(ctx, filter)
		if err != nil {
			return nil, err
		}
		for _, message := range messages {
			if canonicalExternalRunReply(message, item, run) {
				return message, nil
			}
		}
		if len(messages) < filter.Limit {
			return nil, nil
		}
		last := messages[len(messages)-1]
		if last == nil || last.Sequence <= filter.AfterSequence {
			return nil, models.ErrExternalConversationConflict
		}
		filter.AfterSequence = last.Sequence
	}
}

// Only the external mutable-Run read needs normalization. A reporting worker
// must project its exact claimed snapshot even when a newer attempt exists.
func externalConversationFailureReportRun(ctx context.Context, reports interface {
	GetRunTerminalReport(context.Context, models.Scope, string, models.AgentRunStatus) (*models.RunTerminalReport, error)
}, run *models.AgentRun) (*models.AgentRun, error) {
	report, err := reports.GetRunTerminalReport(ctx, run.Scope, run.ID, run.Status)
	if errors.Is(err, models.ErrRunTerminalReportNotFound) {
		// Historical pre-59 failures may have no reporting intent.
		return run, nil
	}
	if err != nil {
		return nil, err
	}
	if report == nil || report.Scope != run.Scope || report.RunID != run.ID || report.Status != run.Status || report.TerminalRevision < 0 || report.TerminalRevision > run.Revision {
		return nil, models.ErrInvalidRunTerminalReport
	}
	if report.TerminalRevision == 0 && report.DeliveredAt != nil && report.Run == nil {
		// Acknowledged schema58 rows released their snapshots before the
		// terminal revision was recorded. Do not invent historical identity.
		return run, nil
	}
	if report.Run != nil {
		snapshot := report.Run
		if snapshot.Scope != run.Scope || snapshot.ID != run.ID || snapshot.Status != run.Status || snapshot.Revision != report.TerminalRevision || snapshot.Owner != run.Owner || snapshot.AssignedAgentID != run.AssignedAgentID || snapshot.Kind != run.Kind || snapshot.ParentRunID != run.ParentRunID || snapshot.Source != run.Source {
			return nil, models.ErrInvalidRunTerminalReport
		}
		for _, key := range []string{models.ConversationRunConversationIDContextKey, models.ConversationRunTriggerIDContextKey, "threadRootMessageId"} {
			left, leftString := snapshot.Context[key].(string)
			right, rightString := run.Context[key].(string)
			if key == "threadRootMessageId" && snapshot.Context[key] == nil && run.Context[key] == nil {
				continue
			}
			if !leftString || !rightString || left != right {
				return nil, models.ErrInvalidRunTerminalReport
			}
		}
		return models.CloneAgentRun(snapshot), nil
	}
	if report.DeliveredAt == nil {
		return nil, models.ErrInvalidRunTerminalReport
	}
	// Acknowledgment releases only the duplicated snapshot, never its key.
	result := models.CloneAgentRun(run)
	result.Revision = report.TerminalRevision
	return result, nil
}

func canonicalExternalRunReply(message *models.ChannelMessage, item *models.ExternalConversationInboxItem, run *models.AgentRun) bool {
	if message == nil || message.Scope != item.Scope || message.ConversationID != item.ConversationID ||
		message.ReplyToMessageID != item.ChannelMessageID || message.Intent != models.MessageIntentAnswer ||
		message.Sender.Type == models.ConversationParticipantUser || message.Historical || strings.TrimSpace(message.Content) == "" {
		return false
	}
	for _, reference := range message.References {
		if reference.Kind == models.ConversationReferenceRun && reference.ID == run.ID {
			return true
		}
	}
	return false
}

func externalConversationRunReply(ctx context.Context, sessions conversation.LoopSessionEntryLister, actions models.ActionCallReader, run *models.AgentRun) (string, bool) {
	if run == nil {
		return "", false
	}
	if run.Status == models.AgentRunStatusFailed {
		// Only a posted model explanation is the failed attempt's reply.
		if f := run.FailureExplanation; f != nil && f.Explained {
			if summary := strings.TrimSpace(conversationrun.ConversationResultString(run.Output, "summary")); summary != "" && len(summary) <= 65536 {
				return summary, true
			}
		}
		// Failures may follow partially executed actions. Report only the reply
		// interruption, without exposing internal errors or guessing whether
		// those actions had external effects.
		return conversation.TerminalRunFailureReply(ctx, sessions, actions, run), true
	}
	if run.Status != models.AgentRunStatusCompleted {
		return "", false
	}
	reply, ok := run.Output["reply"].(string)
	reply = strings.TrimSpace(reply)
	return reply, ok && reply != "" && len(reply) <= 65536
}

// A later question or answer in the same thread makes an old failure notice
// stale. Other threads in the channel must not suppress this reply. Canceled
// runs are deliberately excluded by project: replacement turns handle them.
// For a Team member (member != ""), only that member's own later answer
// supersedes its failure; a teammate's answer does not.
func (w *ReplyWorker) failureSuperseded(
	ctx context.Context,
	item *models.ExternalConversationInboxItem,
	endpoint *models.ExternalConversationEndpoint,
	member string,
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
	if threadRoot == "" && (item.Event.ExternalThreadID != "" || endpoint.Policy.ReplyMode == models.ExternalConversationReplyThread) {
		threadRoot = trigger.ID
	}
	filter := models.ChannelMessageFilter{
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
			if message.Sender.Type == models.ConversationParticipantUser ||
				(message.Intent == models.MessageIntentAnswer && (member == "" || message.Sender.ID == member)) {
				return true, nil
			}
		}
		if len(messages) < filter.Limit {
			return false, nil
		}
		filter.AfterSequence = messages[len(messages)-1].Sequence
	}
}
