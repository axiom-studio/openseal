package conversationrun

import (
	"context"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

// This is a reporting affordance only. The projector retrieves the canonical
// ledger and source message before publishing any conversation task result.
func conversationTaskTerminalReportingCandidate(run *models.AgentRun) bool {
	return run != nil && models.IsTerminalAgentRunStatus(run.Status) &&
		(run.Kind == models.RunKindAgentWork || run.Kind == models.RunKindConversation && run.ParentRunID == "" && run.Context[models.ConversationTaskContextKey] != nil)
}

// FindConversationTaskResultMessage resolves the exact saved answer of a
// continued conversation rather than requiring a second terminal message.
// Independent tasks retain their original reporting key and Task reference.
func FindConversationTaskResultMessage(ctx context.Context, store models.ConversationStore, task *models.ConversationTask, run *models.AgentRun) (*models.ChannelMessage, error) {
	if store == nil || !models.ConversationTaskMatchesWorkRun(task, run) || !models.IsTerminalAgentRunStatus(run.Status) {
		return nil, models.ErrInvalidConversationTask
	}
	if task.Mode != models.ConversationTaskModeContinuation {
		message, err := store.FindChannelMessageByIdempotencyKey(ctx, task.Scope, task.ConversationID, "run-reporting-terminal:"+run.ID+":"+string(run.Status))
		if err != nil || message == nil {
			return message, err
		}
		if !conversationTaskResultMessageMatches(task, run, message, true) {
			return nil, models.ErrInvalidConversationTask
		}
		return message, nil
	}
	verified, sourceReport, err := conversation.TaskForReport(ctx, store, run)
	if err != nil {
		return nil, err
	}
	if verified == nil || sourceReport || !models.SameConversationTask(task, verified) {
		return nil, models.ErrInvalidConversationTask
	}
	keys := []string{
		TerminalConversationFailureReplyKey(run),
		models.ConversationTaskFinalResponseKey(run),
		"team-action-outcome:" + models.SHA256DigestString(run.Scope.Kind+"\x00"+run.Scope.ID+"\x00"+run.ID+"\x00"+task.SourceMessageID),
		"conversation-run-canceled-result:" + run.ID,
		"run-reporting-terminal:" + run.ID + ":" + string(run.Status),
	}
	for _, key := range keys {
		message, err := store.FindChannelMessageByIdempotencyKey(ctx, task.Scope, task.ConversationID, key)
		if errors.Is(err, models.ErrChannelMessageNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if message == nil {
			continue
		}
		if !conversationTaskResultMessageMatches(task, run, message, false) {
			return nil, models.ErrInvalidConversationTask
		}
		return message, nil
	}
	// Team coordination can save several answers in one canonical round. This
	// Run's own Agent's answer is the stable task result link; the round still
	// owns the complete answer set.
	key := "participation-round:" + models.SHA256DigestString(run.Scope.Kind+"\x00"+run.Scope.ID+"\x00"+task.ConversationID+"\x00"+task.SourceMessageID)
	round, err := store.FindParticipationRoundByIdempotencyKey(ctx, task.Scope, task.ConversationID, key)
	if err != nil {
		return nil, err
	}
	if run.Status == models.AgentRunStatusCompleted && round != nil && round.Round != nil && round.Round.TriggerMessageID == task.SourceMessageID {
		for _, message := range round.Messages {
			if conversationTaskResultMessageMatches(task, run, message, false) {
				return message, nil
			}
		}
		message, err := store.FindChannelMessageByIdempotencyKey(ctx, task.Scope, task.ConversationID, "team-participation-unanswered:"+round.Round.ID)
		if err != nil {
			return nil, err
		}
		if message != nil && conversationTaskResultMessageMatches(task, run, message, false) {
			return message, nil
		}
	}
	return nil, nil
}

func conversationTaskResultMessageMatches(task *models.ConversationTask, run *models.AgentRun, message *models.ChannelMessage, requireTaskReference bool) bool {
	if message == nil || message.Scope != task.Scope || message.ConversationID != task.ConversationID ||
		message.ReplyToMessageID != task.SourceMessageID || message.ThreadRootID != task.ThreadRootID ||
		message.Intent != models.MessageIntentAnswer && message.Intent != models.MessageIntentSystem || strings.TrimSpace(message.Content) == "" {
		return false
	}
	if message.Sender.Type != models.ConversationParticipantAgent && (message.Sender.Type != models.ConversationParticipantService || message.Sender.ID != "openseal.conversation") {
		return false
	}
	// A result is the expected speaker's own answer, never another member's.
	want := task.TargetAgentID
	if want == "" {
		want = strings.TrimSpace(run.AssignedAgentID)
	}
	if want == "" {
		want = task.Owner.ID
	}
	if message.Sender.Type == models.ConversationParticipantAgent && message.Sender.ID != want {
		return false
	}
	runReference, taskReference := false, false
	for _, reference := range message.References {
		if reference.Kind == models.ConversationReferenceRun && reference.ID == run.ID {
			runReference = true
		}
		if reference.Kind == models.ConversationReferenceTask {
			if reference.ID != task.ID {
				return false
			}
			taskReference = true
		}
	}
	return runReference && (!requireTaskReference || taskReference)
}

func projectConversationTaskContinuationResult(ctx context.Context, store models.ConversationStore, task *models.ConversationTask, run *models.AgentRun) error {
	if message, err := FindConversationTaskResultMessage(ctx, store, task, run); err != nil || message != nil {
		return err
	}
	source, err := store.GetChannelMessage(ctx, task.Scope, task.ConversationID, task.SourceMessageID)
	if err != nil {
		return err
	}
	if source == nil {
		return models.ErrChannelMessageNotFound
	}
	service := conversation.NewService(store)
	channel, err := service.GetConversation(ctx, task.Scope, task.ConversationID)
	if err != nil {
		return err
	}
	content := terminalRunStatusLabel(run.Status) + ": " + strings.TrimSpace(task.Goal)
	content = conversationTaskReportContent(ctx, store, task, false, run, content)
	sender := models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: run.AssignedAgentID}
	if sender.ID == "" {
		sender = models.ConversationParticipant{Type: models.ConversationParticipantService, ID: "openseal.conversation"}
	}
	request := conversation.PostChannelMessageRequest{
		Scope: task.Scope, ConversationID: task.ConversationID, ExpectedRevision: channel.Revision,
		Sender: sender, SenderDisplayName: "Agent", Intent: models.MessageIntentAnswer, Content: content,
		Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, ReplyToMessageID: source.ID, ResolvesMessageID: source.ID,
		BroadcastToChannel: source.ThreadRootID == "" || source.BroadcastToChannel, ResponseMode: source.ResponseMode,
		References:     []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: run.ID}, {Kind: models.ConversationReferenceTask, ID: task.ID}},
		IdempotencyKey: "run-reporting-terminal:" + run.ID + ":" + string(run.Status),
	}
	_, err = service.PostChannelMessage(ctx, request)
	if errors.Is(err, models.ErrRevisionConflict) {
		channel, getErr := service.GetConversation(ctx, task.Scope, task.ConversationID)
		if getErr != nil {
			return getErr
		}
		request.ExpectedRevision = channel.Revision
		_, err = service.PostChannelMessage(ctx, request)
	}
	return err
}

func conversationTaskReportContent(ctx context.Context, store models.ConversationStore, task *models.ConversationTask, source bool, run *models.AgentRun, fallback string) string {
	if source {
		return task.Acknowledgment
	}
	if run.Status == models.AgentRunStatusFailed {
		return conversation.TerminalRunFailureReply(ctx, store, store, run)
	}
	if run.Status == models.AgentRunStatusCompleted {
		for _, key := range []string{"report", "summary"} {
			if answer, ok := run.Output[key].(string); ok && strings.TrimSpace(answer) != "" {
				return strings.TrimSpace(answer)
			}
		}
	}
	return fallback
}
