package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
)

// ErrRunTerminalReportUndeliverable marks a failure report whose Run and
// conversation disagree for good. Retrying cannot change that, so the outbox
// acknowledges the report instead of retrying it forever.
var ErrRunTerminalReportUndeliverable = errors.New("terminal Run report is undeliverable")

// undeliverableReport still matches ErrInvalidAgentRun for callers that check it.
type undeliverableReport struct{ reason string }

func (e undeliverableReport) Error() string { return "invalid agent run: " + e.reason }

func (e undeliverableReport) Is(target error) bool {
	return target == ErrRunTerminalReportUndeliverable || target == ErrInvalidAgentRun
}

func undeliverable(reason string) error { return undeliverableReport{reason: reason} }

// failedConversationReportCandidate is a failed chat Run with an Agent to
// speak for it: an Agent's own Run or a Team member's. A Team's routing Run
// has no speaker, so it reports nothing.
func failedConversationReportCandidate(run *AgentRun) bool {
	if run == nil || run.Status != AgentRunStatusFailed || run.Kind != RunKindConversation || run.ParentRunID != "" {
		return false
	}
	if run.Owner.Type != OwnerTypeAgent && !isTeamMemberRun(run) {
		return false
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
	return validOpaqueIdentifier(conversationID, 128) && validOpaqueIdentifier(triggerID, 128)
}

func terminalConversationFailureReplyKey(run *AgentRun) string {
	return "conversation-failure:" + run.ID + ":" + strconv.FormatInt(run.Revision, 10)
}

// projectFailedConversationReply first checks the normal Agent response key.
// Otherwise outbox recovery and external delivery share one immutable failure
// key per terminal attempt rather than creating additional status messages.
func projectFailedConversationReply(ctx context.Context, store ConversationStore, run *AgentRun) error {
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
	conversation, err := store.GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return err
	}
	if conversation == nil || conversation.Status != ConversationStatusActive {
		// A deleted or archived chat has nobody left to tell.
		return nil
	}
	if conversation.Scope != run.Scope || conversation.Owner != run.Owner {
		return undeliverable("conversation owner differs from the Run owner")
	}
	// An application chat explains a credit stop beside the saved message and
	// retries this attempt once credits are added. A provider channel has no
	// such surface, so it still receives the failure answer.
	if run.CreditError != nil && !externalChannelContext(conversation) {
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
	if voiceCallStartedMessage(trigger) && trigger.Initiator != nil {
		initiator = *trigger.Initiator
	}
	if initiator.Type != ConversationParticipantUser {
		// Service wakes (connection setup, for example) and Agent handoffs
		// have no person waiting on this attempt's answer.
		return nil
	}
	if initiator.Validate() != nil {
		return undeliverable("trigger initiator is invalid")
	}
	expectedRoot := externalConversationThreadRoot(conversation, trigger)
	if root, _ := run.Context["threadRootMessageId"].(string); root != expectedRoot {
		return undeliverable("Run thread differs from the trigger thread")
	}
	agentID := ConversationRunAgentID(conversation, run)
	if !validOpaqueIdentifier(agentID, 256) {
		return undeliverable("Run has no Agent to answer")
	}
	references := []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}}
	if run.Context[ConversationTaskContextKey] != nil {
		task, source, taskErr := conversationTaskForReport(ctx, store, run)
		if taskErr != nil {
			return taskErr
		}
		if task == nil || source || task.Mode != ConversationTaskModeContinuation {
			return ErrInvalidConversationTask
		}
		if saved, findErr := FindConversationTaskResultMessage(ctx, store, task, run); findErr != nil || saved != nil {
			return findErr
		}
		references = append(references, ConversationReference{Kind: ConversationReferenceTask, ID: task.ID})
	}
	// A model answer committed before terminalization already closes this
	// attempt. A failure answer from an older manual attempt does not.
	if saved, findErr := store.FindChannelMessageByIdempotencyKey(ctx, run.Scope, conversationID, conversationTaskFinalResponseKey(run)); findErr != nil && !errors.Is(findErr, ErrChannelMessageNotFound) {
		return findErr
	} else if saved != nil {
		if !terminalConversationReplyMatches(saved, run, triggerID, agentID) {
			return undeliverable("saved answer does not belong to this Run")
		}
		return nil
	}
	sessions, actions := terminalFailureReplyStores(store)
	notes, _ := store.(AgentSessionStore)
	key := terminalConversationFailureReplyKey(run)
	if saved, findErr := store.FindChannelMessageByIdempotencyKey(ctx, run.Scope, conversationID, key); findErr != nil && !errors.Is(findErr, ErrChannelMessageNotFound) {
		return findErr
	} else if saved != nil {
		if !terminalConversationReplyMatches(saved, run, triggerID, agentID) {
			return undeliverable("saved answer does not belong to this Run")
		}
		// A crash after the post may have left the note unwritten.
		return appendRunFailedNote(ctx, notes, run, saved)
	}
	request := PostChannelMessageRequest{
		Scope: run.Scope, ConversationID: conversationID, ExpectedRevision: conversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: agentID}, Intent: MessageIntentAnswer,
		Content:  terminalRunFailureReply(ctx, sessions, actions, run),
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: triggerID, ResolvesMessageID: triggerID,
		BroadcastToChannel: trigger.ThreadRootID == "" || trigger.BroadcastToChannel, ResponseMode: trigger.ResponseMode,
		References: references, IdempotencyKey: key,
	}
	service := NewConversationService(store)
	posted, err := service.PostChannelMessage(ctx, request)
	if errors.Is(err, ErrRevisionConflict) {
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
	return appendRunFailedNote(ctx, notes, run, posted.Message)
}

// runFailedNoteSourceKey is per terminal attempt, like the reply key, so a
// retried Run that fails again records its own note.
func runFailedNoteSourceKey(run *AgentRun) string {
	return "run-failed:" + run.ID + ":" + strconv.FormatInt(run.Revision, 10)
}

// runFailedNote tells the Run's session what the user was told when the Run
// stopped, so the next Run in that session knows the attempt ended.
func runFailedNote(run *AgentRun, message *ChannelMessage) *SessionEntry {
	key := runFailedNoteSourceKey(run)
	data, _ := json.Marshal(map[string]string{"messageId": message.ID})
	text := "Your previous attempt (run " + run.ID + ") stopped and the user was told: " + strconv.Quote(boundedTurnFailureText(message.Content))
	return &SessionEntry{
		ID: SessionEntryID(run.SessionID, key), SessionID: run.SessionID, RunID: run.ID,
		Type: SessionEntryNote, SourceKey: key,
		Note: &NoteEntry{Kind: NoteRunFailed, Text: text, Data: data},
	}
}

// appendRunFailedNote appends the run_failed note without a lease: the Run is
// terminal and the store accepts lease-less notes for it. The SourceKey makes
// a repeated report a no-op.
func appendRunFailedNote(ctx context.Context, sessions AgentSessionStore, run *AgentRun, message *ChannelMessage) error {
	if sessions == nil || run.SessionID == "" || message == nil {
		return nil
	}
	_, err := sessions.AppendAgentSessionEntries(ctx, AppendAgentSessionEntriesRequest{
		Scope: run.Scope, SessionID: run.SessionID, RunID: run.ID,
		Entries: []*SessionEntry{runFailedNote(run, message)}, Now: time.Now().UTC(),
	})
	if errors.Is(err, ErrAgentSessionNotFound) {
		// The Run failed before its first step opened the session. A session
		// created later is seeded from the channel, failure reply included.
		return nil
	}
	return err
}

func terminalConversationReplyMatches(message *ChannelMessage, run *AgentRun, triggerID, agentID string) bool {
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	if message == nil || message.Scope != run.Scope || message.ConversationID != conversationID || message.ReplyToMessageID != triggerID || message.Sender.Type != ConversationParticipantAgent || message.Sender.ID != agentID || message.Intent != MessageIntentAnswer || message.Historical || strings.TrimSpace(message.Content) == "" {
		return false
	}
	for _, reference := range message.References {
		if reference.Kind == ConversationReferenceRun && reference.ID == run.ID {
			return true
		}
	}
	return false
}
