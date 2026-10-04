package runtime

import (
	"context"
	"errors"
	"strings"
)

// Independent tasks keep their original conversation authority after the source
// chat has ended. Only the canonical task ledger can establish that authority;
// copied context fields on an unrelated root Run never grant conversation access.
func conversationTaskWorkOrigin(ctx context.Context, runs PortfolioStore, conversations ConversationStore, run *AgentRun) (*AgentRun, *Conversation, *ChannelMessage, error) {
	if run == nil || run.Kind != RunKindAgentWork || run.Owner.Type != OwnerTypeAgent || run.ParentRunID != "" || conversations == nil {
		return nil, nil, nil, nil
	}
	proof, ok := runs.(ConversationTaskProofStore)
	if !ok {
		return nil, nil, nil, nil
	}
	task, err := proof.FindConversationTaskByWorkRunID(ctx, run.Scope, run.ID)
	if errors.Is(err, ErrConversationTaskNotFound) {
		return nil, nil, nil, nil
	}
	if err != nil || task == nil {
		return nil, nil, nil, err
	}
	if task.Mode != ConversationTaskModeIndependent || task.TargetAgentID != run.Owner.ID {
		return nil, nil, nil, nil
	}
	valid, err := VerifyConversationTaskWorkRun(ctx, proof, task, run)
	if err != nil || !valid {
		return nil, nil, nil, err
	}
	source, err := runs.GetAgentRun(ctx, task.Scope, task.SourceRunID)
	if err != nil {
		return nil, nil, nil, err
	}
	conversation, err := conversations.GetConversation(ctx, task.Scope, task.ConversationID)
	if err != nil {
		return nil, nil, nil, err
	}
	if conversation == nil || conversation.Status != ConversationStatusActive {
		return nil, nil, nil, nil
	}
	trigger, err := conversations.GetChannelMessage(ctx, task.Scope, task.ConversationID, task.SourceMessageID)
	if err != nil {
		return nil, nil, nil, err
	}
	actor, valid := conversationMessageInitiatingUser(conversation, trigger)
	if !valid || actor != task.AuthenticatedActor || !validConversationTaskAdmissionSource(source, conversation, trigger, task) {
		return nil, nil, nil, nil
	}
	return source, conversation, trigger, nil
}

// Use only after conversationWorkOrigin has verified the canonical origin.
func conversationClarificationReferences(run *AgentRun) []ConversationReference {
	references := []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}}
	if run.Kind == RunKindAgentWork && run.ParentRunID == "" {
		if taskID, ok := run.Context[ConversationTaskContextKey].(string); ok && taskID != "" {
			references = append(references, ConversationReference{Kind: ConversationReferenceTask, ID: taskID})
		}
	}
	return references
}

// An explicit reply to a root Run's question is a clarification attempt, including
// when its actor, thread or current Run state prevents acceptance. Do not turn
// such rejected answers into unrelated foreground work.
func (s *ConversationRunScheduler) targetsRootClarificationQuestion(ctx context.Context, conversation *Conversation, answer *ChannelMessage) (bool, error) {
	if conversation == nil || answer == nil || answer.Scope != conversation.Scope || answer.ConversationID != conversation.ID {
		return false, nil
	}
	target := answer.ResolvesMessageID
	if target == "" {
		target = answer.ReplyToMessageID
	}
	if target == "" {
		return false, nil
	}
	question, err := s.conversations.store.GetChannelMessage(ctx, conversation.Scope, conversation.ID, target)
	if errors.Is(err, ErrChannelMessageNotFound) {
		return false, nil
	}
	if err != nil || question == nil {
		return false, err
	}
	if question.Sender != (ConversationParticipant{Type: ConversationParticipantAgent, ID: conversation.Owner.ID}) || question.Intent != MessageIntentQuestion || !question.RequiresResponse || !strings.HasPrefix(question.IdempotencyKey, "conversation-clarification:") {
		return false, nil
	}
	for _, reference := range question.References {
		if reference.Kind != ConversationReferenceRun {
			continue
		}
		run, err := s.runs.store.GetAgentRun(ctx, conversation.Scope, reference.ID)
		if err != nil {
			return false, err
		}
		if run != nil && run.Kind == RunKindConversation && run.ParentRunID == "" {
			_, origin, _, err := foregroundConversationWorkOrigin(ctx, s.runs.store, s.conversations.store, run)
			if err != nil {
				return false, err
			}
			if origin != nil && origin.ID == conversation.ID {
				return true, nil
			}
		}
		tasks, ok := s.runs.store.(ConversationTaskStore)
		if !ok {
			continue
		}
		task, err := tasks.FindConversationTaskByWorkRunID(ctx, conversation.Scope, reference.ID)
		if err != nil && !errors.Is(err, ErrConversationTaskNotFound) {
			return false, err
		}
		if task != nil && task.Validate() == nil && task.Mode == ConversationTaskModeIndependent && task.ConversationID == conversation.ID && task.Owner == conversation.Owner {
			return true, nil
		}
	}
	return false, nil
}

func conversationClarificationMatches(run *AgentRun, trigger, question, answer *ChannelMessage) bool {
	if question.Validate() != nil {
		return false
	}
	if run.ParentRunID != "" {
		return true
	}
	thread := trigger.ThreadRootID
	if thread == "" {
		thread = trigger.ID
	}
	if question.ReplyToMessageID != trigger.ID || question.ThreadRootID != thread || !question.RequiresResponse ||
		question.ID != stableConversationID(run.Scope, question.IdempotencyKey, "message") ||
		!strings.HasPrefix(question.IdempotencyKey, "conversation-clarification:"+run.ID+":") ||
		answer.ThreadRootID != "" && answer.ThreadRootID != thread {
		return false
	}
	if run.Context[ConversationTaskContextKey] == nil {
		return true
	}
	for _, reference := range question.References {
		if reference.Kind == ConversationReferenceTask && reference.ID == run.Context[ConversationTaskContextKey] {
			return true
		}
	}
	return false
}

// Bind host-owned pending purposes to the exact question-producing turn. Legacy
// Runs may have only an applied sequence, so omit the ID when no matching turn
// is retained; consumers requiring purpose-specific consent must fail closed.
func conversationClarificationTurnID(ctx context.Context, runs PortfolioStore, run *AgentRun, question *ChannelMessage) (string, error) {
	turns, ok := runs.(AgentTurnStore)
	if !ok || run.LastAppliedTurn < 1 {
		return "", nil
	}
	items, err := turns.ListAgentTurns(ctx, AgentTurnFilter{Scope: run.Scope, RunID: run.ID, AfterSequence: run.LastAppliedTurn - 1, Limit: 1})
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "", nil
	}
	turn := items[0]
	if turn == nil || turn.Scope != run.Scope || turn.RunID != run.ID || turn.Sequence != run.LastAppliedTurn ||
		turn.Status != AgentTurnStatusCompleted || turn.NextRunStatus != AgentRunStatusWaitingForEvent ||
		turn.WakeCondition == nil || turn.WakeCondition.Type != "user_message" {
		return "", nil
	}
	content, _ := turn.RunOutput["summary"].(string)
	if strings.TrimSpace(content) != question.Content {
		return "", nil
	}
	return turn.ID, nil
}
