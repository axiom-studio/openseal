package conversationrun

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

// ErrRunTerminalReportUndeliverable marks a failure report whose Run and
// conversation disagree for good. Retrying cannot change that, so the outbox
// acknowledges the report instead of retrying it forever.
var ErrRunTerminalReportUndeliverable = errors.New("terminal Run report is undeliverable")

// undeliverableReport still matches ErrInvalidAgentRun for callers that check it.
type undeliverableReport struct{ reason string }

func (e undeliverableReport) Error() string { return "invalid agent run: " + e.reason }

func (e undeliverableReport) Is(target error) bool {
	return target == ErrRunTerminalReportUndeliverable || target == models.ErrInvalidAgentRun
}

func undeliverable(reason string) error { return undeliverableReport{reason: reason} }

// TerminalConversationFailureReplyKey is the idempotency key of the failure
// reply to a terminal Run at its current revision.
func TerminalConversationFailureReplyKey(run *models.AgentRun) string {
	return "conversation-failure:" + run.ID + ":" + strconv.FormatInt(run.Revision, 10)
}

// ProjectFailedConversationReply first checks the normal Agent response key.
// Otherwise outbox recovery and external delivery share one immutable failure
// key per terminal attempt rather than creating additional status messages.
func ProjectFailedConversationReply(ctx context.Context, store models.ConversationStore, run *models.AgentRun) error {
	conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
	triggerID, _ := run.Context[models.ConversationRunTriggerIDContextKey].(string)
	conversation, err := store.GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return err
	}
	if conversation == nil || conversation.Status != models.ConversationStatusActive {
		// A deleted or archived chat has nobody left to tell.
		return nil
	}
	if conversation.Scope != run.Scope || conversation.Owner != run.Owner {
		return undeliverable("conversation owner differs from the Run owner")
	}
	// An application chat explains a credit stop beside the saved message and
	// retries this attempt once credits are added. A provider channel has no
	// such surface, so it still receives the failure answer.
	if run.CreditError != nil && !models.IsExternalChannelConversation(conversation) {
		return nil
	}
	trigger, err := store.GetChannelMessage(ctx, run.Scope, conversationID, triggerID)
	if err != nil {
		return err
	}
	if trigger == nil || trigger.Scope != run.Scope || trigger.ConversationID != conversationID || trigger.ID != triggerID || !conversationMessageStartsRun(conversation, trigger) {
		return undeliverable("trigger message is missing or does not start a Run")
	}
	initiator := trigger.Sender
	if models.VoiceCallStartedMessage(trigger) && trigger.Initiator != nil {
		initiator = *trigger.Initiator
	}
	if initiator.Type != models.ConversationParticipantUser {
		// Service wakes (connection setup, for example) and Agent handoffs
		// have no person waiting on this attempt's answer.
		return nil
	}
	if initiator.Validate() != nil {
		return undeliverable("trigger initiator is invalid")
	}
	expectedRoot := models.ExternalConversationThreadRoot(conversation, trigger)
	if root, _ := run.Context["threadRootMessageId"].(string); root != expectedRoot {
		return undeliverable("Run thread differs from the trigger thread")
	}
	agentID := runtimeconversation.ConversationRunAgentID(conversation, run)
	if !models.IsValidOpaqueIdentifier(agentID, 256) {
		return undeliverable("Run has no Agent to answer")
	}
	references := []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: run.ID}}
	if run.Context[models.ConversationTaskContextKey] != nil {
		task, source, taskErr := runtimeconversation.ConversationTaskForReport(ctx, store, run)
		if taskErr != nil {
			return taskErr
		}
		if task == nil || source || task.Mode != models.ConversationTaskModeContinuation {
			return models.ErrInvalidConversationTask
		}
		if saved, findErr := FindConversationTaskResultMessage(ctx, store, task, run); findErr != nil || saved != nil {
			return findErr
		}
		references = append(references, models.ConversationReference{Kind: models.ConversationReferenceTask, ID: task.ID})
	}
	// A model answer committed before terminalization already closes this
	// attempt. A failure answer from an older manual attempt does not.
	if saved, findErr := store.FindChannelMessageByIdempotencyKey(ctx, run.Scope, conversationID, models.ConversationTaskFinalResponseKey(run)); findErr != nil && !errors.Is(findErr, models.ErrChannelMessageNotFound) {
		return findErr
	} else if saved != nil {
		if !terminalConversationReplyMatches(saved, run, triggerID, agentID) {
			return undeliverable("saved answer does not belong to this Run")
		}
		return nil
	}
	key := TerminalConversationFailureReplyKey(run)
	if saved, findErr := store.FindChannelMessageByIdempotencyKey(ctx, run.Scope, conversationID, key); findErr != nil && !errors.Is(findErr, models.ErrChannelMessageNotFound) {
		return findErr
	} else if saved != nil {
		if !terminalConversationReplyMatches(saved, run, triggerID, agentID) {
			return undeliverable("saved answer does not belong to this Run")
		}
		// A crash after the post may have left the note unwritten.
		return appendRunFailedNote(ctx, store, run, saved)
	}
	request := runtimeconversation.PostChannelMessageRequest{
		Scope: run.Scope, ConversationID: conversationID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: agentID}, Intent: models.MessageIntentAnswer,
		Content:  runtimeconversation.TerminalRunFailureReply(ctx, store, store, run),
		Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, ReplyToMessageID: triggerID, ResolvesMessageID: triggerID,
		BroadcastToChannel: trigger.ThreadRootID == "" || trigger.BroadcastToChannel, ResponseMode: trigger.ResponseMode,
		References: references, IdempotencyKey: key,
	}
	service := runtimeconversation.NewConversationService(store)
	posted, err := service.PostChannelMessage(ctx, request)
	if errors.Is(err, models.ErrRevisionConflict) {
		current, getErr := store.GetConversation(ctx, run.Scope, conversationID)
		if getErr != nil {
			return getErr
		}
		request.ExpectedRevision = current.Revision
		posted, err = service.PostChannelMessage(ctx, request)
	}
	if err != nil {
		return err
	}
	return appendRunFailedNote(ctx, store, run, posted.Message)
}

// runFailedNoteSourceKey is per terminal attempt, like the reply key, so a
// retried Run that fails again records its own note.
func runFailedNoteSourceKey(run *models.AgentRun) string {
	return "run-failed:" + run.ID + ":" + strconv.FormatInt(run.Revision, 10)
}

// runFailedNote tells the Run's session what the user was told when the Run
// stopped, so the next Run in that session knows the attempt ended.
func runFailedNote(run *models.AgentRun, message *models.ChannelMessage) *models.SessionEntry {
	key := runFailedNoteSourceKey(run)
	data, _ := json.Marshal(map[string]string{"messageId": message.ID})
	text := "Your previous attempt (run " + run.ID + ") stopped and the user was told: " + strconv.Quote(runtimeconversation.BoundedTurnFailureText(message.Content))
	return &models.SessionEntry{
		ID: models.SessionEntryID(run.SessionID, key), SessionID: run.SessionID, RunID: run.ID,
		Type: models.SessionEntryNote, SourceKey: key,
		Note: &models.NoteEntry{Kind: models.NoteRunFailed, Text: text, Data: data},
	}
}

// appendRunFailedNote appends the run_failed note without a lease: the Run is
// terminal and the store accepts lease-less notes for it. The SourceKey makes
// a repeated report a no-op.
func appendRunFailedNote(ctx context.Context, sessions models.AgentSessionStore, run *models.AgentRun, message *models.ChannelMessage) error {
	if run.SessionID == "" || message == nil {
		return nil
	}
	_, err := sessions.AppendAgentSessionEntries(ctx, models.AppendAgentSessionEntriesRequest{
		Scope: run.Scope, SessionID: run.SessionID, RunID: run.ID,
		Entries: []*models.SessionEntry{runFailedNote(run, message)}, Now: time.Now().UTC(),
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, models.ErrAgentSessionNotFound):
		// The Run failed before its first step opened the session. A session
		// created later is seeded from the channel, failure reply included.
		return nil
	case runFailedNoteRejected(err):
		// The reply is already posted, and retrying cannot change a refusal;
		// the next Run still sees the reply in the channel.
		slog.Warn("run_failed note was refused; the failure reply is posted without it",
			"scope", run.Scope.ID, "run", run.ID, "session", run.SessionID, "error", err)
		return nil
	}
	return err
}

// runFailedNoteRejected reports a store refusal of the note that a retry
// cannot change, as opposed to a store that could not be reached.
func runFailedNoteRejected(err error) bool {
	for _, permanent := range []error{
		models.ErrInvalidSessionEntry, models.ErrSessionEntryTooLarge, models.ErrInvalidAgentSession, models.ErrAgentSessionConflict,
		models.ErrSessionMoved, models.ErrLeaseLost, models.ErrRunNotFound, models.ErrAgentSessionPathTooLarge,
	} {
		if errors.Is(err, permanent) {
			return true
		}
	}
	return false
}

func terminalConversationReplyMatches(message *models.ChannelMessage, run *models.AgentRun, triggerID, agentID string) bool {
	conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
	if message == nil || message.Scope != run.Scope || message.ConversationID != conversationID || message.ReplyToMessageID != triggerID || message.Sender.Type != models.ConversationParticipantAgent || message.Sender.ID != agentID || message.Intent != models.MessageIntentAnswer || message.Historical || strings.TrimSpace(message.Content) == "" {
		return false
	}
	for _, reference := range message.References {
		if reference.Kind == models.ConversationReferenceRun && reference.ID == run.ID {
			return true
		}
	}
	return false
}
