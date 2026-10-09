package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
		now:       time.Now,
	}, nil
}

// ProcessScope projects up to limit applied inbound messages. Message and
// delivery idempotency keys make a repeated projection a no-op. When the
// store records reply progress, a resolved item is marked terminal and a
// failing one backs off, so idle scopes stop re-projecting every applied
// item on every pass; other stores keep the original revisit behaviour.
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
	progress, tracked := w.store.(ExternalConversationReplyStateStore)
	now := w.now().UTC()
	filter := ExternalConversationInboxFilter{
		Scope: scope, Statuses: []ExternalConversationInboxStatus{ExternalConversationInboxApplied}, Limit: limit,
	}
	if tracked {
		filter.ReplyDueAt = &now
	}
	items, err := w.store.ListExternalConversationInbox(ctx, filter)
	if err != nil {
		return nil, err
	}
	deliveries := make([]*ExternalConversationDelivery, 0, len(items))
	var projectErrors []error
	for _, item := range items {
		delivery, resolved, projectErr := w.project(ctx, item)
		if projectErr != nil {
			projectErrors = append(projectErrors, fmt.Errorf("project external conversation inbox item %s: %w", item.ID, projectErr))
			if tracked {
				w.recordFailure(ctx, progress, item, now, projectErr)
			}
			continue
		}
		if delivery != nil {
			deliveries = append(deliveries, delivery)
		}
		if tracked && resolved {
			// A lost race only means another worker recorded it first.
			_ = progress.SaveExternalConversationReplyProgress(ctx, item.Scope, item.ID, item.Revision, ExternalConversationReplyProgress{
				State: ExternalConversationReplyProjected, Attempts: item.ReplyAttempts, At: now,
			})
		}
	}
	return deliveries, errors.Join(projectErrors...)
}

func (w *ExternalConversationReplyWorker) recordFailure(ctx context.Context, progress ExternalConversationReplyStateStore, item *ExternalConversationInboxItem, now time.Time, cause error) {
	attempts := item.ReplyAttempts + 1
	next := ExternalConversationReplyProgress{Attempts: attempts, Error: cause.Error(), At: now}
	if attempts >= externalConversationReplyMaximumAttempts {
		next.State = ExternalConversationReplyFailed
	} else {
		retryAt := now.Add(externalConversationReplyRetryDelay(attempts))
		next.AvailableAt = &retryAt
	}
	_ = progress.SaveExternalConversationReplyProgress(ctx, item.Scope, item.ID, item.Revision, next)
}

// project returns resolved=true when no further projection can ever be
// needed for the item: its reply exists or was delivered, or no reply is
// due (no Run, a canceled Run, a paused endpoint, a superseded failure).
func (w *ExternalConversationReplyWorker) project(
	ctx context.Context,
	item *ExternalConversationInboxItem,
) (*ExternalConversationDelivery, bool, error) {
	delivery, err := w.projectItem(ctx, item)
	if errors.Is(err, errExternalReplyPending) {
		return nil, false, nil
	}
	return delivery, err == nil, err
}

// errExternalReplyPending marks an item whose Run has not finished yet.
var errExternalReplyPending = errors.New("external conversation reply pending")

func (w *ExternalConversationReplyWorker) projectItem(
	ctx context.Context,
	item *ExternalConversationInboxItem,
) (*ExternalConversationDelivery, error) {
	if item == nil {
		return nil, nil
	}
	if item.Status != ExternalConversationInboxApplied ||
		strings.TrimSpace(item.RunID) == "" || strings.TrimSpace(item.ConversationID) == "" ||
		strings.TrimSpace(item.ChannelMessageID) == "" {
		return nil, nil
	}
	run, err := w.store.GetAgentRun(ctx, item.Scope, item.RunID)
	if err != nil {
		return nil, err
	}
	if run == nil || run.Status == AgentRunStatusCanceled {
		return nil, nil
	}
	if externalInboxItemIsFollowUp(item, run) {
		return nil, nil // The Run's own trigger item delivers its single reply.
	}
	if run.Status != AgentRunStatusCompleted && run.Status != AgentRunStatusFailed {
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
	if failedConversationReportCandidate(run) {
		// Metadata-only updates are not new attempts. Resolve the captured
		// terminal identity instead of minting a reply for a mutable revision.
		if reports, ok := w.store.(interface {
			GetRunTerminalReport(context.Context, Scope, string, AgentRunStatus) (*RunTerminalReport, error)
		}); ok {
			run, err = externalConversationFailureReportRun(ctx, reports, run)
			if err != nil {
				return nil, err
			}
		}
		conversationID, conversationOK := run.Context[conversationRunContextConversationID].(string)
		triggerID, triggerOK := run.Context[conversationRunContextTriggerID].(string)
		if !conversationOK || !triggerOK || conversationID != item.ConversationID || triggerID != item.ChannelMessageID {
			// An acknowledged report releases its snapshot. Its immutable
			// inbox source still fences projection from mutable Run context.
			return nil, ErrExternalConversationConflict
		}
		// Both workers publish through the same revision-specific failure key.
		// The provider outbox then delivers that saved Agent answer once.
		if err := projectFailedConversationReply(ctx, w.store, run); err != nil {
			return nil, err
		}
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
	// The canonical projector also prefers a committed normal answer. In
	// particular, an older failed manual attempt must not win a history scan.
	message, err := w.store.FindChannelMessageByIdempotencyKey(ctx, item.Scope, item.ConversationID, conversationTaskFinalResponseKey(run))
	if err != nil && !errors.Is(err, ErrChannelMessageNotFound) {
		return nil, err
	}
	if message != nil {
		if !canonicalExternalRunReply(message, item, run) {
			return nil, ErrExternalConversationConflict
		}
		return message, nil
	}
	if run.Status == AgentRunStatusFailed {
		message, err := w.store.FindChannelMessageByIdempotencyKey(ctx, item.Scope, item.ConversationID, terminalConversationFailureReplyKey(run))
		if err != nil && !errors.Is(err, ErrChannelMessageNotFound) {
			return nil, err
		}
		if message != nil {
			if !canonicalExternalRunReply(message, item, run) {
				return nil, ErrExternalConversationConflict
			}
			return message, nil
		}
	}
	if messageID, _ := run.Output["messageId"].(string); strings.TrimSpace(messageID) != "" {
		message, err := w.store.GetChannelMessage(ctx, item.Scope, item.ConversationID, messageID)
		if err != nil && !errors.Is(err, ErrChannelMessageNotFound) {
			return nil, err
		}
		if message != nil && message.ID == messageID && canonicalExternalRunReply(message, item, run) {
			return message, nil
		}
	}
	// Older outputs may not carry messageId. Stores cap page sizes, so scan
	// actual pages rather than assuming one large limit includes the answer.
	filter := ChannelMessageFilter{Scope: item.Scope, ConversationID: item.ConversationID, Limit: 100}
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
			return nil, ErrExternalConversationConflict
		}
		filter.AfterSequence = last.Sequence
	}
}

// Only the external mutable-Run read needs normalization. A reporting worker
// must project its exact claimed snapshot even when a newer attempt exists.
func externalConversationFailureReportRun(ctx context.Context, reports interface {
	GetRunTerminalReport(context.Context, Scope, string, AgentRunStatus) (*RunTerminalReport, error)
}, run *AgentRun) (*AgentRun, error) {
	report, err := reports.GetRunTerminalReport(ctx, run.Scope, run.ID, run.Status)
	if errors.Is(err, ErrRunTerminalReportNotFound) {
		// Historical pre-59 failures may have no reporting intent.
		return run, nil
	}
	if err != nil {
		return nil, err
	}
	if report == nil || report.Scope != run.Scope || report.RunID != run.ID || report.Status != run.Status || report.TerminalRevision < 0 || report.TerminalRevision > run.Revision {
		return nil, ErrInvalidRunTerminalReport
	}
	if report.TerminalRevision == 0 && report.DeliveredAt != nil && report.Run == nil {
		// Acknowledged schema58 rows released their snapshots before the
		// terminal revision was recorded. Do not invent historical identity.
		return run, nil
	}
	if report.Run != nil {
		snapshot := report.Run
		if snapshot.Scope != run.Scope || snapshot.ID != run.ID || snapshot.Status != run.Status || snapshot.Revision != report.TerminalRevision || snapshot.Owner != run.Owner || snapshot.AssignedAgentID != run.AssignedAgentID || snapshot.Kind != run.Kind || snapshot.ParentRunID != run.ParentRunID || snapshot.Source != run.Source {
			return nil, ErrInvalidRunTerminalReport
		}
		for _, key := range []string{conversationRunContextConversationID, conversationRunContextTriggerID, "threadRootMessageId"} {
			left, leftString := snapshot.Context[key].(string)
			right, rightString := run.Context[key].(string)
			if key == "threadRootMessageId" && snapshot.Context[key] == nil && run.Context[key] == nil {
				continue
			}
			if !leftString || !rightString || left != right {
				return nil, ErrInvalidRunTerminalReport
			}
		}
		return cloneAgentRun(snapshot), nil
	}
	if report.DeliveredAt == nil {
		return nil, ErrInvalidRunTerminalReport
	}
	// Acknowledgment releases only the duplicated snapshot, never its key.
	result := cloneAgentRun(run)
	result.Revision = report.TerminalRevision
	return result, nil
}

func canonicalExternalRunReply(message *ChannelMessage, item *ExternalConversationInboxItem, run *AgentRun) bool {
	if message == nil || message.Scope != item.Scope || message.ConversationID != item.ConversationID ||
		message.ReplyToMessageID != item.ChannelMessageID || message.Intent != MessageIntentAnswer ||
		message.Sender.Type == ConversationParticipantUser || message.Historical || strings.TrimSpace(message.Content) == "" {
		return false
	}
	for _, reference := range message.References {
		if reference.Kind == ConversationReferenceRun && reference.ID == run.ID {
			return true
		}
	}
	return false
}

func externalConversationRunReply(run *AgentRun) (string, bool) {
	if run == nil {
		return "", false
	}
	if run.Status == AgentRunStatusFailed {
		if requiresFinalFailureExplanation(run.Checkpoint) {
			failure := run.Checkpoint[FinalFailureExplanationCheckpointKey].(map[string]interface{})
			explained, _ := failure["explained"].(bool)
			if summary := strings.TrimSpace(conversationResultString(run.Output, "summary")); explained && summary != "" && len(summary) <= 65536 {
				return summary, true
			}
		}
		// Failures may follow partially executed actions. Report only the reply
		// interruption, without exposing internal errors or guessing whether
		// those actions had external effects.
		return TerminalFailureReply(terminalFailureCodeFromCheckpoint(run.Checkpoint)), true
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
