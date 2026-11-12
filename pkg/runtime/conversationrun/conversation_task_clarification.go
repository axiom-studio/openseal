package conversationrun

import (
	"context"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

// An explicit reply to a root Run's question is a clarification attempt, including
// when its actor, thread or current Run state prevents acceptance. Do not turn
// such rejected answers into unrelated foreground work.
func (s *Scheduler) targetsRootClarificationQuestion(ctx context.Context, conversation *models.Conversation, answer *models.ChannelMessage) (bool, error) {
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
	question, err := s.conversationStore.GetChannelMessage(ctx, conversation.Scope, conversation.ID, target)
	if errors.Is(err, models.ErrChannelMessageNotFound) {
		return false, nil
	}
	if err != nil || question == nil {
		return false, err
	}
	if question.Sender.Type != models.ConversationParticipantAgent || question.Intent != models.MessageIntentQuestion || !question.RequiresResponse || !strings.HasPrefix(question.IdempotencyKey, "conversation-clarification:") {
		return false, nil
	}
	for _, reference := range question.References {
		if reference.Kind != models.ConversationReferenceRun {
			continue
		}
		run, err := s.runStore.GetAgentRun(ctx, conversation.Scope, reference.ID)
		if err != nil {
			return false, err
		}
		if run != nil && run.Kind == models.RunKindConversation && run.ParentRunID == "" {
			_, origin, _, err := runtimeconversation.ForegroundConversationWorkOrigin(ctx, s.runStore, s.conversationStore, run)
			if err != nil {
				return false, err
			}
			if origin != nil && origin.ID == conversation.ID && runtimeconversation.RunAgentID(conversation, run) == question.Sender.ID {
				return true, nil
			}
		}
		tasks, ok := s.runStore.(models.ConversationTaskStore)
		if !ok {
			continue
		}
		task, err := tasks.FindConversationTaskByWorkRunID(ctx, conversation.Scope, reference.ID)
		if err != nil && !errors.Is(err, agentrun.ErrConversationTaskNotFound) {
			return false, err
		}
		if task != nil && task.Validate() == nil && task.Mode == models.ConversationTaskModeIndependent && task.ConversationID == conversation.ID && task.Owner == conversation.Owner &&
			task.TargetAgentID == question.Sender.ID {
			return true, nil
		}
	}
	return false, nil
}

func conversationClarificationMatches(run *models.AgentRun, trigger, question, answer *models.ChannelMessage) bool {
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
		question.ID != models.StableConversationID(run.Scope, question.IdempotencyKey, "message") ||
		!strings.HasPrefix(question.IdempotencyKey, "conversation-clarification:"+run.ID+":") ||
		answer.ThreadRootID != "" && answer.ThreadRootID != thread {
		return false
	}
	if run.Context[models.ConversationTaskContextKey] == nil {
		return true
	}
	for _, reference := range question.References {
		if reference.Kind == models.ConversationReferenceTask && reference.ID == run.Context[models.ConversationTaskContextKey] {
			return true
		}
	}
	return false
}

// conversationQuestionEntry returns the ask_user entry a question-waiting Run
// is parked on (run.WakeCondition.Reference), or nil when the reference does
// not name an open ask_user entry of this Run. The answer receipt binds to
// that entry's step (QuestionTurnID = step id, QuestionTurnSequence = step).
func conversationQuestionEntry(ctx context.Context, runs models.PortfolioStore, run *models.AgentRun) (*models.SessionEntry, error) {
	if !isConversationQuestionWait(run) {
		return nil, nil
	}
	sessions, ok := runs.(models.AgentSessionStore)
	if !ok {
		return nil, nil
	}
	entry, err := sessions.GetSessionEntry(ctx, run.Scope, run.WakeCondition.Reference)
	if errors.Is(err, models.ErrSessionEntryNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if entry == nil || entry.RunID != run.ID || entry.Type != models.SessionEntryTool || entry.State != models.ToolEntryAwaitingEvent ||
		entry.StepID == "" || entry.Step < 1 || models.AskUserEntryQuestion(entry) == "" {
		return nil, nil
	}
	return entry, nil
}
