package runtime

import (
	"context"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

const conversationAnswerEvent = "run.conversation_answer_received"

// clarificationQuestionKey is the idempotency key of the question a Run
// waiting on an ask_user entry posted; its wake reference names the entry.
func clarificationQuestionKey(run *models.AgentRun) string {
	if run == nil || run.WakeCondition == nil {
		return ""
	}
	return runtimeconversation.ConversationClarificationKey(run.ID, run.WakeCondition.Reference)
}
func isConversationQuestionWait(run *models.AgentRun) bool {
	return run != nil && run.Status == models.AgentRunStatusWaitingForEvent && run.WakeCondition != nil && run.WakeCondition.Type == "user_message" &&
		models.IsValidOpaqueIdentifier(run.WakeCondition.Reference, 128)
}

// Reconciliation re-posts the question of every Run parked on an ask_user
// entry, from the entry's question argument (idempotent with the settle post).
func (s *ConversationRunScheduler) reconcileConversationQuestions(ctx context.Context, scope models.Scope, result *ConversationRunReconcileResult) error {
	for _, kind := range []models.RunKind{models.RunKindAgentWork, models.RunKindConversation} {
		if err := s.reconcileConversationQuestionsForKind(ctx, scope, result, kind); err != nil {
			return err
		}
	}
	return nil
}

func (s *ConversationRunScheduler) reconcileConversationQuestionsForKind(ctx context.Context, scope models.Scope, result *ConversationRunReconcileResult, kind models.RunKind) error {
	const pageSize = 100
	for offset := 0; ; offset += pageSize {
		runs, err := s.runStore.ListAgentRuns(ctx, models.AgentRunFilter{Scope: scope, Kind: kind, Statuses: []models.AgentRunStatus{models.AgentRunStatusWaitingForEvent}, Limit: pageSize, Offset: offset})
		if err != nil {
			return err
		}
		for _, run := range runs {
			if !isConversationQuestionWait(run) {
				continue
			}
			_, conversation, trigger, err := runtimeconversation.ConversationWorkOrigin(ctx, s.runStore, s.conversationStore, run)
			if err != nil {
				return err
			}
			if conversation == nil || trigger == nil || runtimeconversation.ConversationRunAgentID(conversation, run) == "" {
				continue
			}
			if s.config.RequireParticipationOptIn && !conversationParticipationAllows(conversation, trigger) {
				continue
			}
			entry, err := conversationQuestionEntry(ctx, s.runStore, run)
			if err != nil {
				return err
			}
			if entry == nil {
				continue
			}
			posted, postErr := s.conversations.PostChannelMessage(ctx, runtimeconversation.ConversationQuestionPostRequest(run, conversation, trigger, models.AskUserEntryArgs(entry), clarificationQuestionKey(run)))
			if errors.Is(postErr, models.ErrRevisionConflict) {
				continue
			}
			if postErr != nil {
				return postErr
			}
			if !posted.Replayed {
				result.Results++
			}
		}
		if len(runs) < pageSize {
			return nil
		}
	}
}

type conversationQuestion struct {
	message *models.ChannelMessage
	run     *models.AgentRun
	source  *models.AgentRun
	trigger *models.ChannelMessage
}

// Answers target an explicit question (or its unambiguous thread). An unthreaded
// answer may address the single immediately preceding pending question. Never
// broadcast an answer across concurrent tasks.
func (s *ConversationRunScheduler) resumeConversationAnswer(ctx context.Context, conversation *models.Conversation, answer *models.ChannelMessage) (*agentrun.AgentRunCommandResult, bool, error) {
	taskTargeted, err := s.targetsRootClarificationQuestion(ctx, conversation, answer)
	if err != nil {
		return nil, false, err
	}
	if (conversation.Owner.Type != models.OwnerTypeAgent && conversation.Owner.Type != models.OwnerTypeTeam) || answer.Sender.Type != models.ConversationParticipantUser {
		return nil, taskTargeted, nil
	}
	actor, valid := runtimeconversation.ConversationMessageInitiatingUser(conversation, answer)
	if !valid {
		return nil, taskTargeted, nil
	}
	if err := answer.Validate(); err != nil {
		return nil, taskTargeted, nil
	}
	var pending []conversationQuestion
	const pageSize = 100
	for after := int64(0); ; {
		messages, err := s.conversations.ListChannelMessages(ctx, models.ChannelMessageFilter{Scope: conversation.Scope, ConversationID: conversation.ID, ThreadRootID: models.ExternalConversationThreadRoot(conversation, answer), AfterSequence: after, BeforeSequence: answer.Sequence, Limit: pageSize})
		if err != nil {
			return nil, false, err
		}
		for _, question := range messages {
			if question.Sender.Type != models.ConversationParticipantAgent || question.Intent != models.MessageIntentQuestion || !strings.HasPrefix(question.IdempotencyKey, "conversation-clarification:") {
				continue
			}
			for _, ref := range question.References {
				if ref.Kind != models.ConversationReferenceRun {
					continue
				}
				run, err := s.runStore.GetAgentRun(ctx, conversation.Scope, ref.ID)
				if err != nil {
					return nil, false, err
				}
				// A Team's question is its asking member's; another member
				// never receives an answer to it.
				if agentID := runtimeconversation.ConversationRunAgentID(conversation, run); run == nil || agentID == "" || agentID != question.Sender.ID {
					continue
				}
				source, origin, trigger, err := runtimeconversation.ConversationWorkOriginForState(ctx, s.runStore, s.conversationStore, run, true)
				if err != nil {
					return nil, false, err
				}
				originalActor, valid := runtimeconversation.ConversationMessageInitiatingUser(conversation, trigger)
				if origin == nil || origin.ID != conversation.ID || !valid || originalActor != actor {
					continue
				}
				if !conversationClarificationMatches(run, trigger, question, answer) {
					continue
				}
				// Read the atomic transition receipt before testing current status. A worker
				// may already have finished or asked another question after accepting this answer.
				accepted, err := s.conversationAnswerAccepted(ctx, run, answer.ID)
				if err != nil {
					return nil, false, err
				}
				if accepted {
					return s.acknowledgeConversationAnswer(ctx, conversation, answer, run)
				}
				if !isConversationQuestionWait(run) || clarificationQuestionKey(run) != question.IdempotencyKey {
					continue
				}
				_, origin, _, err = runtimeconversation.ConversationWorkOrigin(ctx, s.runStore, s.conversationStore, run)
				if err != nil {
					return nil, false, err
				}
				if origin == nil || origin.ID != conversation.ID {
					continue
				}
				pending = append(pending, conversationQuestion{message: question, run: run, source: source, trigger: trigger})
			}
		}
		if len(messages) < pageSize {
			break
		}
		after = messages[len(messages)-1].Sequence
	}
	target := answer.ResolvesMessageID
	if target == "" {
		target = answer.ReplyToMessageID
	}
	var matches []conversationQuestion
	for _, q := range pending {
		if target != "" {
			if q.message.ID == target {
				matches = []conversationQuestion{q}
				break
			}
			if q.message.ThreadRootID == target {
				matches = append(matches, q)
			}
		} else if len(pending) == 1 && q.message.Sequence == answer.Sequence-1 {
			matches = append(matches, q)
		}
	}
	if len(matches) != 1 {
		return nil, taskTargeted, nil
	}
	q := matches[0]
	entry, err := conversationQuestionEntry(ctx, s.runStore, q.run)
	if err != nil {
		return nil, false, err
	}
	if entry == nil || models.AskUserEntryQuestion(entry) != q.message.Content {
		return nil, taskTargeted, nil
	}
	receipt := &models.ConversationAnswerReceipt{
		Scope: conversation.Scope, RunID: q.run.ID, AgentID: q.run.AssignedAgentID,
		ConversationID: conversation.ID, SourceRunID: q.source.ID, SourceMessageID: q.trigger.ID,
		ThreadRootMessageID: q.message.ThreadRootID, QuestionMessageID: q.message.ID,
		QuestionTurnID: entry.StepID, QuestionTurnSequence: entry.Step, QuestionContent: q.message.Content,
		AnswerMessageID: answer.ID, AnswerContent: answer.Content, SelectedOptionIDs: append([]string(nil), answer.SelectedOptionIDs...),
		AuthenticatedActor: actor,
	}
	payload := map[string]interface{}{"conversationId": conversation.ID, "questionMessageId": q.message.ID, "answerMessageId": answer.ID}
	if q.run.Kind == models.RunKindAgentWork && q.run.ParentRunID == "" {
		receipt.ConversationTaskID, _ = q.run.Context[models.ConversationTaskContextKey].(string)
	}
	if receipt.ConversationTaskID != "" {
		payload["conversationTaskId"] = receipt.ConversationTaskID
		payload["sourceRunId"] = receipt.SourceRunID
		payload["sourceMessageId"] = receipt.SourceMessageID
		payload["threadRootMessageId"] = receipt.ThreadRootMessageID
		payload["authenticatedActorId"] = actor.ID
		payload["agentId"] = receipt.AgentID
		payload["questionTurnSequence"] = receipt.QuestionTurnSequence
		payload["questionTurnId"] = receipt.QuestionTurnID
	}
	activity := runtimeactivity.NewRunActivityService(s.runStore, s.runStore, models.WithClock(s.now))
	run, event, err := activity.TransitionRun(ctx, conversation.Scope, q.run.ID, runtimeactivity.RunTransitionRequest{
		ExpectedRevision: q.run.Revision, Status: models.AgentRunStatusQueued,
		Summary: "Received the user's answer; continuing the task", EventType: conversationAnswerEvent,
		Actor: models.ActivityActor{Type: "user", ID: answer.Sender.ID}, CorrelationID: answer.ID, CausationID: q.message.ID,
		Payload:      payload,
		Intervention: &models.AgentRunIntervention{ID: models.StableConversationID(conversation.Scope, "conversation-answer:"+answer.ID, "intervention"), Actor: models.ActivityActor{Type: "user", ID: answer.Sender.ID}, Instruction: "Answer to your clarification:\n" + answer.Content, CreatedAt: answer.CreatedAt, ConversationAnswer: receipt},
	})
	if err != nil {
		return nil, false, err
	}
	result, _, err := s.acknowledgeConversationAnswer(ctx, conversation, answer, q.run)
	if result != nil {
		result.Run = run
		result.Event = event
	}
	return result, true, err
}

func (s *ConversationRunScheduler) conversationAnswerAccepted(ctx context.Context, run *models.AgentRun, answerID string) (bool, error) {
	const pageSize = 100
	for after := int64(0); ; {
		events, err := s.runStore.ListActivity(ctx, models.ActivityFilter{Scope: run.Scope, RunID: run.ID, EventTypes: []string{conversationAnswerEvent}, AfterSequence: after, Limit: pageSize})
		if err != nil {
			return false, err
		}
		for _, event := range events {
			if event.EventType == conversationAnswerEvent && event.CorrelationID == answerID {
				return true, nil
			}
		}
		if len(events) < pageSize {
			return false, nil
		}
		after = events[len(events)-1].Sequence
	}
}
func (s *ConversationRunScheduler) acknowledgeConversationAnswer(ctx context.Context, conversation *models.Conversation, answer *models.ChannelMessage, run *models.AgentRun) (*agentrun.AgentRunCommandResult, bool, error) {
	current, err := s.conversations.GetConversation(ctx, conversation.Scope, conversation.ID)
	if err != nil {
		return nil, false, err
	}
	_, err = s.conversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: conversation.Scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: runtimeconversation.ConversationRunAgentID(conversation, run)}, Intent: models.MessageIntentAcknowledgment,
		Content: "Your answer has been passed to the task.", Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
		ReplyToMessageID: answer.ID, ResolvesMessageID: answer.ID, References: runtimeconversation.ConversationClarificationReferences(run),
		IdempotencyKey: "conversation-answer-received:" + answer.ID,
	})
	return &agentrun.AgentRunCommandResult{Run: run}, true, err
}
