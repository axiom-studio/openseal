package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const conversationAnswerEvent = "run.conversation_answer_received"

// Resolve the channel through kernel-owned lineage, never model-supplied context
// on a delegated child. Restrict this handoff to the owning Agent's own work.
func conversationWorkOrigin(ctx context.Context, runs PortfolioStore, conversations ConversationStore, run *AgentRun) (*AgentRun, *Conversation, *ChannelMessage, error) {
	return conversationWorkOriginForState(ctx, runs, conversations, run, false)
}

// Accepted-answer receipts may outlive a completed or paused parent. Resolve
// their immutable initiating identity without treating it as a fresh Run grant.
func conversationWorkOriginForState(ctx context.Context, runs PortfolioStore, conversations ConversationStore, run *AgentRun, includeInactiveParents bool) (*AgentRun, *Conversation, *ChannelMessage, error) {
	if run != nil && run.ParentRunID == "" && run.Context[ConversationTaskContextKey] != nil {
		return conversationTaskWorkOrigin(ctx, runs, conversations, run)
	}
	if run != nil && run.ParentRunID == "" && run.Context[deferredWorkflowContextKey] == true {
		return deferredWorkflowConversationOrigin(ctx, runs, conversations, run)
	}
	if run != nil && run.Kind == RunKindConversation && run.ParentRunID == "" {
		return foregroundConversationWorkOrigin(ctx, runs, conversations, run)
	}
	if run == nil || run.Kind != RunKindAgentWork || run.Owner.Type != OwnerTypeAgent || run.ParentRunID == "" {
		return nil, nil, nil, nil
	}
	current := run
	seen := map[string]bool{run.ID: true}
	for depth := 0; depth < 64 && current.ParentRunID != ""; depth++ {
		parent, err := runs.GetAgentRun(ctx, run.Scope, current.ParentRunID)
		if err != nil {
			return nil, nil, nil, err
		}
		if parent == nil || seen[parent.ID] || parent.Owner != run.Owner || !includeInactiveParents && (isTerminalAgentRunStatus(parent.Status) || parent.Status == AgentRunStatusPaused) {
			return nil, nil, nil, nil
		}
		seen[parent.ID] = true
		current = parent
		if current.Kind != RunKindConversation {
			continue
		}
		id, _ := current.Context[conversationRunContextConversationID].(string)
		triggerID, _ := current.Context[conversationRunContextTriggerID].(string)
		if id == "" || triggerID == "" {
			return nil, nil, nil, nil
		}
		conversation, err := conversations.GetConversation(ctx, run.Scope, id)
		if errors.Is(err, ErrConversationNotFound) {
			return nil, nil, nil, nil
		}
		if err != nil {
			return nil, nil, nil, err
		}
		if conversation == nil || conversation.Status != ConversationStatusActive || conversation.Owner != run.Owner {
			return nil, nil, nil, nil
		}
		trigger, err := conversations.GetChannelMessage(ctx, run.Scope, id, triggerID)
		return current, conversation, trigger, err
	}
	return nil, nil, nil, nil
}

func clarificationQuestionKey(run *AgentRun) string {
	return fmt.Sprintf("conversation-clarification:%s:%d", run.ID, run.LastAppliedTurn)
}
func isConversationQuestionWait(run *AgentRun) bool {
	return run != nil && run.Status == AgentRunStatusWaitingForEvent && run.WakeCondition != nil && run.WakeCondition.Type == "user_message" && run.LastAppliedTurn > 0
}

// Reconciliation also repairs questions produced before this projection existed.
func (s *ConversationRunScheduler) reconcileConversationQuestions(ctx context.Context, scope Scope, result *ConversationRunReconcileResult) error {
	for _, kind := range []RunKind{RunKindAgentWork, RunKindConversation} {
		if err := s.reconcileConversationQuestionsForKind(ctx, scope, result, kind); err != nil {
			return err
		}
	}
	return nil
}

func (s *ConversationRunScheduler) reconcileConversationQuestionsForKind(ctx context.Context, scope Scope, result *ConversationRunReconcileResult, kind RunKind) error {
	const pageSize = 100
	for offset := 0; ; offset += pageSize {
		runs, err := s.runs.store.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, Kind: kind, Statuses: []AgentRunStatus{AgentRunStatusWaitingForEvent}, Limit: pageSize, Offset: offset})
		if err != nil {
			return err
		}
		for _, run := range runs {
			if !isConversationQuestionWait(run) {
				continue
			}
			_, conversation, trigger, err := conversationWorkOrigin(ctx, s.runs.store, s.conversations.store, run)
			if err != nil {
				return err
			}
			if conversation == nil || trigger == nil {
				continue
			}
			if s.config.RequireParticipationOptIn && !conversationParticipationAllows(conversation, trigger) {
				continue
			}
			question, _ := run.Output["summary"].(string)
			if strings.TrimSpace(question) == "" {
				continue
			}
			posted, postErr := s.conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
				Scope: scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
				Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: conversation.Owner.ID},
				Intent: MessageIntentQuestion, Content: question, Audience: ConversationAudience{Kind: ConversationAudienceChannel},
				ReplyToMessageID: trigger.ID, RequiresResponse: true, References: conversationClarificationReferences(run),
				IdempotencyKey: clarificationQuestionKey(run),
			})
			if errors.Is(postErr, ErrRevisionConflict) {
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
	message *ChannelMessage
	run     *AgentRun
	source  *AgentRun
	trigger *ChannelMessage
}

// Answers target an explicit question (or its unambiguous thread). An unthreaded
// answer may address the single immediately preceding pending question. Never
// broadcast an answer across concurrent tasks.
func (s *ConversationRunScheduler) resumeConversationAnswer(ctx context.Context, conversation *Conversation, answer *ChannelMessage) (*AgentRunCommandResult, bool, error) {
	taskTargeted, err := s.targetsRootClarificationQuestion(ctx, conversation, answer)
	if err != nil {
		return nil, false, err
	}
	if conversation.Owner.Type != OwnerTypeAgent || answer.Sender.Type != ConversationParticipantUser {
		return nil, taskTargeted, nil
	}
	actor, valid := conversationMessageInitiatingUser(conversation, answer)
	if !valid {
		return nil, taskTargeted, nil
	}
	if err := answer.Validate(); err != nil {
		return nil, taskTargeted, nil
	}
	var pending []conversationQuestion
	const pageSize = 100
	for after := int64(0); ; {
		messages, err := s.conversations.ListChannelMessages(ctx, ChannelMessageFilter{Scope: conversation.Scope, ConversationID: conversation.ID, ThreadRootID: externalConversationThreadRoot(conversation, answer), AfterSequence: after, BeforeSequence: answer.Sequence, Limit: pageSize})
		if err != nil {
			return nil, false, err
		}
		for _, question := range messages {
			if question.Sender.Type != ConversationParticipantAgent || question.Sender.ID != conversation.Owner.ID || question.Intent != MessageIntentQuestion || !strings.HasPrefix(question.IdempotencyKey, "conversation-clarification:") {
				continue
			}
			for _, ref := range question.References {
				if ref.Kind != ConversationReferenceRun {
					continue
				}
				run, err := s.runs.store.GetAgentRun(ctx, conversation.Scope, ref.ID)
				if err != nil {
					return nil, false, err
				}
				if run == nil {
					continue
				}
				source, origin, trigger, err := conversationWorkOriginForState(ctx, s.runs.store, s.conversations.store, run, true)
				if err != nil {
					return nil, false, err
				}
				originalActor, valid := conversationMessageInitiatingUser(conversation, trigger)
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
				_, origin, _, err = conversationWorkOrigin(ctx, s.runs.store, s.conversations.store, run)
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
	questionTurnID, err := conversationClarificationTurnID(ctx, s.runs.store, q.run, q.message)
	if err != nil {
		return nil, false, err
	}
	receipt := &ConversationAnswerReceipt{
		Scope: conversation.Scope, RunID: q.run.ID, AgentID: q.run.AssignedAgentID,
		ConversationID: conversation.ID, SourceRunID: q.source.ID, SourceMessageID: q.trigger.ID,
		ThreadRootMessageID: q.message.ThreadRootID, QuestionMessageID: q.message.ID,
		QuestionTurnID: questionTurnID, QuestionTurnSequence: q.run.LastAppliedTurn, QuestionContent: q.message.Content,
		AnswerMessageID: answer.ID, AnswerContent: answer.Content, AuthenticatedActor: actor,
	}
	payload := map[string]interface{}{"conversationId": conversation.ID, "questionMessageId": q.message.ID, "answerMessageId": answer.ID}
	if q.run.Kind == RunKindAgentWork && q.run.ParentRunID == "" {
		receipt.ConversationTaskID, _ = q.run.Context[ConversationTaskContextKey].(string)
	}
	if receipt.ConversationTaskID != "" {
		payload["conversationTaskId"] = receipt.ConversationTaskID
		payload["sourceRunId"] = receipt.SourceRunID
		payload["sourceMessageId"] = receipt.SourceMessageID
		payload["threadRootMessageId"] = receipt.ThreadRootMessageID
		payload["authenticatedActorId"] = actor.ID
		payload["agentId"] = receipt.AgentID
		payload["questionTurnSequence"] = receipt.QuestionTurnSequence
		if receipt.QuestionTurnID != "" {
			payload["questionTurnId"] = receipt.QuestionTurnID
		}
	}
	activity := NewRunActivityService(s.runs.store, s.runs.store)
	run, event, err := activity.TransitionRun(ctx, conversation.Scope, q.run.ID, RunTransitionRequest{
		ExpectedRevision: q.run.Revision, Status: AgentRunStatusQueued,
		Summary: "Received the user's answer; continuing the task", EventType: conversationAnswerEvent,
		Actor: ActivityActor{Type: "user", ID: answer.Sender.ID}, CorrelationID: answer.ID, CausationID: q.message.ID,
		Payload:      payload,
		Intervention: &AgentRunIntervention{ID: stableConversationID(conversation.Scope, "conversation-answer:"+answer.ID, "intervention"), Actor: ActivityActor{Type: "user", ID: answer.Sender.ID}, Instruction: "Answer to your clarification:\n" + answer.Content, CreatedAt: answer.CreatedAt, ConversationAnswer: receipt},
	})
	if err != nil {
		return nil, false, err
	}
	result, _, err := s.acknowledgeConversationAnswer(ctx, conversation, answer, run)
	if result != nil {
		result.Event = event
	}
	return result, true, err
}

func (s *ConversationRunScheduler) conversationAnswerAccepted(ctx context.Context, run *AgentRun, answerID string) (bool, error) {
	const pageSize = 100
	for after := int64(0); ; {
		events, err := s.runs.store.ListActivity(ctx, ActivityFilter{Scope: run.Scope, RunID: run.ID, EventTypes: []string{conversationAnswerEvent}, AfterSequence: after, Limit: pageSize})
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
func (s *ConversationRunScheduler) acknowledgeConversationAnswer(ctx context.Context, conversation *Conversation, answer *ChannelMessage, run *AgentRun) (*AgentRunCommandResult, bool, error) {
	current, err := s.conversations.GetConversation(ctx, conversation.Scope, conversation.ID)
	if err != nil {
		return nil, false, err
	}
	_, err = s.conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: conversation.Scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: conversation.Owner.ID}, Intent: MessageIntentAcknowledgment,
		Content: "Your answer has been passed to the task.", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		ReplyToMessageID: answer.ID, ResolvesMessageID: answer.ID, References: conversationClarificationReferences(run),
		IdempotencyKey: "conversation-answer-received:" + answer.ID,
	})
	return &AgentRunCommandResult{Run: run}, true, err
}

// Supply the original user wording on every turn, including old delegated Runs.
// Only this same-Agent lineage may read the source message; unrelated Agents do
// not acquire access to conversation history through a fabricated context field.
type conversationWorkTurnRunner struct {
	inner         TurnRunner
	runs          PortfolioStore
	conversations ConversationStore
}

func (r conversationWorkTurnRunner) input(ctx context.Context, input TurnExecutionContext) (TurnExecutionContext, error) {
	input.canAskConversationQuestion = false
	input.sourceAccessChallenge = nil
	_, approvalMode, err := RunConversationApprovalMode(ctx, r.runs, r.conversations, input.Run)
	if err != nil {
		return input, err
	}
	input.approvalMode = approvalMode
	_, conversation, trigger, err := conversationWorkOrigin(ctx, r.runs, r.conversations, input.Run)
	if err != nil || conversation == nil || trigger == nil {
		return input, err
	}
	input.canAskConversationQuestion = conversation.Owner.Type == OwnerTypeAgent && input.Run.AssignedAgentID == conversation.Owner.ID
	input.sourceAccessChallenge, err = resolveSourceAccessChallengeInteraction(ctx, r.runs, input.Run)
	if err != nil {
		return input, err
	}
	input.Run = cloneAgentRun(input.Run)
	if input.Run.Context == nil {
		input.Run.Context = map[string]interface{}{}
	}
	input.Run.Context["conversationRequest"] = map[string]interface{}{"content": trigger.Content, "messageId": trigger.ID, "conversationId": conversation.ID}
	return input, nil
}
func (r conversationWorkTurnRunner) PlanTurnBudget(ctx context.Context, input TurnExecutionContext) (BudgetUsage, error) {
	input, err := r.input(ctx, input)
	if err != nil {
		return BudgetUsage{}, err
	}
	if planner, ok := r.inner.(TurnBudgetPlanner); ok {
		return planner.PlanTurnBudget(ctx, input)
	}
	return BudgetUsage{}, nil
}
func (r conversationWorkTurnRunner) RunTurn(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
	input, err := r.input(ctx, input)
	if err != nil {
		return nil, err
	}
	return r.inner.RunTurn(ctx, input)
}
