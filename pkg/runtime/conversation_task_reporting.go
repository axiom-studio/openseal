package runtime

import (
	"context"
	"errors"
	"strings"
)

func conversationTaskForReport(ctx context.Context, store ConversationStore, run *AgentRun) (*ConversationTask, bool, error) {
	if run == nil {
		return nil, false, nil
	}
	taskID, _ := run.Context[ConversationTaskContextKey].(string)
	sourceTaskID := ""
	switch ids := run.Output["conversationTaskIds"].(type) {
	case []string:
		if len(ids) == 1 {
			sourceTaskID = ids[0]
		}
	case []interface{}:
		if len(ids) == 1 {
			sourceTaskID, _ = ids[0].(string)
		}
	}
	if taskID == "" && sourceTaskID == "" {
		return nil, false, nil
	}
	tasks, ok := store.(ConversationTaskStore)
	if !ok {
		return nil, false, ErrInvalidConversationTask
	}
	var task *ConversationTask
	var err error
	sourceReport := taskID == "" && sourceTaskID != ""
	if sourceReport {
		task, err = tasks.GetConversationTask(ctx, run.Scope, sourceTaskID)
	} else {
		task, err = tasks.FindConversationTaskByWorkRunID(ctx, run.Scope, run.ID)
	}
	if err != nil {
		return nil, false, err
	}
	if task == nil || task.Validate() != nil || task.Scope != run.Scope || task.Owner != run.Owner || task.Mode != ConversationTaskModeContinuation && task.TargetAgentID != run.AssignedAgentID ||
		run.Context[conversationRunContextConversationID] != task.ConversationID || run.Context[conversationRunContextTriggerID] != task.SourceMessageID {
		return nil, false, ErrInvalidConversationTask
	}
	if task.Mode == ConversationTaskModeContinuation {
		proof, ok := store.(ConversationTaskProofStore)
		if !ok {
			return nil, false, ErrInvalidConversationTask
		}
		valid, err := VerifyConversationTaskWorkRun(ctx, proof, task, run)
		if err != nil {
			return nil, false, err
		}
		if !valid {
			return nil, false, ErrInvalidConversationTask
		}
		return task, false, nil
	}
	if sourceReport {
		if run.ID != task.SourceRunID || run.Kind != RunKindConversation || run.Source != RunSourceChat || run.Output["summary"] != task.Acknowledgment || run.Output["conversationTaskWorkRunId"] != task.WorkRunID {
			return nil, false, ErrInvalidConversationTask
		}
	} else if taskID != task.ID || !ConversationTaskMatchesWorkRun(task, run) {
		return nil, false, ErrInvalidConversationTask
	}
	return task, sourceReport, nil
}

// This is a reporting affordance only. The projector retrieves the canonical
// ledger and source message before publishing any conversation task result.
func conversationTaskTerminalReportingCandidate(run *AgentRun) bool {
	return run != nil && isTerminalAgentRunStatus(run.Status) &&
		(run.Kind == RunKindAgentWork || run.Kind == RunKindConversation && run.ParentRunID == "" && run.Context[ConversationTaskContextKey] != nil)
}

// FindConversationTaskResultMessage resolves the exact saved answer of a
// continued conversation rather than requiring a second terminal message.
// Independent tasks retain their original reporting key and Task reference.
func FindConversationTaskResultMessage(ctx context.Context, store ConversationStore, task *ConversationTask, run *AgentRun) (*ChannelMessage, error) {
	if store == nil || !ConversationTaskMatchesWorkRun(task, run) || !isTerminalAgentRunStatus(run.Status) {
		return nil, ErrInvalidConversationTask
	}
	if task.Mode != ConversationTaskModeContinuation {
		message, err := store.FindChannelMessageByIdempotencyKey(ctx, task.Scope, task.ConversationID, "run-reporting-terminal:"+run.ID+":"+string(run.Status))
		if err != nil || message == nil {
			return message, err
		}
		if !conversationTaskResultMessageMatches(task, run, message, true) {
			return nil, ErrInvalidConversationTask
		}
		return message, nil
	}
	verified, sourceReport, err := conversationTaskForReport(ctx, store, run)
	if err != nil {
		return nil, err
	}
	if verified == nil || sourceReport || !sameConversationTask(task, verified) {
		return nil, ErrInvalidConversationTask
	}
	keys := []string{
		conversationTaskFinalResponseKey(run),
		"team-action-outcome:" + hashString(run.Scope.Kind+"\x00"+run.Scope.ID+"\x00"+run.ID+"\x00"+task.SourceMessageID),
		"conversation-run-canceled-result:" + run.ID,
		"run-reporting-terminal:" + run.ID + ":" + string(run.Status),
	}
	for _, key := range keys {
		message, err := store.FindChannelMessageByIdempotencyKey(ctx, task.Scope, task.ConversationID, key)
		if errors.Is(err, ErrChannelMessageNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if message == nil {
			continue
		}
		if !conversationTaskResultMessageMatches(task, run, message, false) {
			return nil, ErrInvalidConversationTask
		}
		return message, nil
	}
	// Team coordination can save several answers in one canonical round. The
	// first visible result is the stable task result link; the round still owns
	// the complete answer set.
	key := "participation-round:" + hashString(run.Scope.Kind+"\x00"+run.Scope.ID+"\x00"+task.ConversationID+"\x00"+task.SourceMessageID)
	round, err := store.FindParticipationRoundByIdempotencyKey(ctx, task.Scope, task.ConversationID, key)
	if err != nil {
		return nil, err
	}
	if run.Status == AgentRunStatusCompleted && round != nil && round.Round != nil && round.Round.TriggerMessageID == task.SourceMessageID {
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

func conversationTaskResultMessageMatches(task *ConversationTask, run *AgentRun, message *ChannelMessage, requireTaskReference bool) bool {
	if message == nil || message.Scope != task.Scope || message.ConversationID != task.ConversationID ||
		message.ReplyToMessageID != task.SourceMessageID || message.ThreadRootID != task.ThreadRootID ||
		message.Intent != MessageIntentAnswer && message.Intent != MessageIntentSystem || strings.TrimSpace(message.Content) == "" {
		return false
	}
	if message.Sender.Type != ConversationParticipantAgent && (message.Sender.Type != ConversationParticipantService || message.Sender.ID != "openseal.conversation") {
		return false
	}
	if task.TargetAgentID != "" && message.Sender.Type == ConversationParticipantAgent && message.Sender.ID != task.TargetAgentID && task.Owner.Type != OwnerTypeTeam {
		return false
	}
	runReference, taskReference := false, false
	for _, reference := range message.References {
		if reference.Kind == ConversationReferenceRun && reference.ID == run.ID {
			runReference = true
		}
		if reference.Kind == ConversationReferenceTask {
			if reference.ID != task.ID {
				return false
			}
			taskReference = true
		}
	}
	return runReference && (!requireTaskReference || taskReference)
}

func projectConversationTaskContinuationResult(ctx context.Context, store ConversationStore, task *ConversationTask, run *AgentRun) error {
	if message, err := FindConversationTaskResultMessage(ctx, store, task, run); err != nil || message != nil {
		return err
	}
	source, err := store.GetChannelMessage(ctx, task.Scope, task.ConversationID, task.SourceMessageID)
	if err != nil {
		return err
	}
	if source == nil {
		return ErrChannelMessageNotFound
	}
	service := NewConversationService(store)
	channel, err := service.GetConversation(ctx, task.Scope, task.ConversationID)
	if err != nil {
		return err
	}
	content := terminalRunStatusLabel(run.Status) + ": " + strings.TrimSpace(task.Goal)
	content = conversationTaskReportContent(task, false, run, content)
	sender := ConversationParticipant{Type: ConversationParticipantAgent, ID: run.AssignedAgentID}
	if sender.ID == "" {
		sender = ConversationParticipant{Type: ConversationParticipantService, ID: "openseal.conversation"}
	}
	request := PostChannelMessageRequest{
		Scope: task.Scope, ConversationID: task.ConversationID, ExpectedRevision: channel.Revision,
		Sender: sender, SenderDisplayName: "Agent", Intent: MessageIntentAnswer, Content: content,
		Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: source.ID, ResolvesMessageID: source.ID,
		BroadcastToChannel: source.ThreadRootID == "" || source.BroadcastToChannel, ResponseMode: source.ResponseMode,
		References:     []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}, {Kind: ConversationReferenceTask, ID: task.ID}},
		IdempotencyKey: "run-reporting-terminal:" + run.ID + ":" + string(run.Status),
	}
	_, err = service.PostChannelMessage(ctx, request)
	if errors.Is(err, ErrRevisionConflict) {
		channel, getErr := service.GetConversation(ctx, task.Scope, task.ConversationID)
		if getErr != nil {
			return getErr
		}
		request.ExpectedRevision = channel.Revision
		_, err = service.PostChannelMessage(ctx, request)
	}
	return err
}

func conversationTaskReportContent(task *ConversationTask, source bool, run *AgentRun, fallback string) string {
	if source {
		return task.Acknowledgment
	}
	if run.Status == AgentRunStatusFailed {
		return terminalFailureReplyFromCheckpoint(run.Checkpoint)
	}
	if run.Status == AgentRunStatusCompleted {
		for _, key := range []string{"report", "summary"} {
			if answer, ok := run.Output[key].(string); ok && strings.TrimSpace(answer) != "" {
				return strings.TrimSpace(answer)
			}
		}
	}
	return fallback
}
