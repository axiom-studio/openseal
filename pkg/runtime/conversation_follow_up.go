package runtime

import (
	"context"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"
)

// ConversationMessageDelivery is the durable receipt for a channel message
// that the scheduler delivered to an already-active foreground conversation
// Run as pending human input.
type ConversationMessageDelivery struct {
	ConversationID      string                  `json:"conversationId"`
	MessageID           string                  `json:"messageId"`
	Sequence            int64                   `json:"sequence"`
	ThreadRootMessageID string                  `json:"threadRootMessageId,omitempty"`
	Sender              ConversationParticipant `json:"sender"`
}

const maximumConversationFollowUpInstructionBytes = 16000

func conversationFollowUpInterventionID(scope Scope, messageID string) string {
	return stableConversationID(scope, "conversation-follow-up:"+messageID, "intervention")
}

// conversationRunFollowUps returns the messages delivered to this foreground
// Run after its trigger, in delivery order.
func conversationRunFollowUps(run *AgentRun) []ConversationMessageDelivery {
	if run == nil {
		return nil
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	var result []ConversationMessageDelivery
	for _, intervention := range run.PendingInterventions {
		delivery := intervention.ConversationMessage
		if delivery != nil && delivery.ConversationID == conversationID && delivery.MessageID != "" &&
			intervention.ID == conversationFollowUpInterventionID(run.Scope, delivery.MessageID) {
			result = append(result, *delivery)
		}
	}
	return result
}

func conversationRunHasFollowUp(run *AgentRun, messageID string) bool {
	for _, delivery := range conversationRunFollowUps(run) {
		if delivery.MessageID == messageID {
			return true
		}
	}
	return false
}

// externalInboxItemIsFollowUp reports whether an inbox item's message was
// delivered into another message's Run. That Run's own inbox item projects
// its progress and reply; the follow-up item must not duplicate them.
func externalInboxItemIsFollowUp(item *ExternalConversationInboxItem, run *AgentRun) bool {
	if item == nil || run == nil {
		return false
	}
	trigger, _ := run.Context[conversationRunContextTriggerID].(string)
	return trigger != item.ChannelMessageID && conversationRunHasFollowUp(run, item.ChannelMessageID)
}

// conversationFollowUpRunEligible reports whether a foreground conversation
// Run can still consume new human input. Any non-terminal state qualifies:
// queued, planning, running and sleeping Runs see the message on their next
// Turn; Runs waiting for approval, an event (credential, setup, browser
// handoff), another Agent, a dependency, or paused by the user keep waiting
// and see the message when they resume. Terminal Runs, Runs promoted to a
// background conversation task, Runs explaining their final failure and
// pinned Runbook Runs never absorb new messages.
func conversationFollowUpRunEligible(ctx context.Context, store PortfolioStore, conversation *Conversation, run *AgentRun) (bool, error) {
	if run == nil || run.Kind != RunKindConversation || run.Scope != conversation.Scope || run.Owner != conversation.Owner ||
		isTerminalAgentRunStatus(run.Status) || requiresFinalFailureExplanation(run.Checkpoint) || (run.Plan != nil && run.Plan["runbook"] != nil) {
		return false, nil
	}
	if conversationID, _ := run.Context[conversationRunContextConversationID].(string); conversationID != conversation.ID {
		return false, nil
	}
	task, err := canonicalConversationTaskContinuation(ctx, store, run)
	if err != nil {
		return false, err
	}
	return task == nil, nil
}

// deliverToActiveConversationRun hands a new human message to the active
// foreground Run of the same Agent channel lane (the conversation, or one
// thread of it) when that Run was started by the same person. The Run sees
// the message as pending human input on its next Turn, exactly like operator
// guidance, so one Run handles the whole exchange instead of parallel Runs
// competing for the lane. A nil result means no Run absorbed the message.
func (s *ConversationRunScheduler) deliverToActiveConversationRun(ctx context.Context, conversation *Conversation, message *ChannelMessage) (*AgentRunCommandResult, error) {
	if conversation.Owner.Type != OwnerTypeAgent || message.Sender.Type != ConversationParticipantUser {
		return nil, nil
	}
	actor, valid := conversationMessageInitiatingUser(conversation, message)
	if !valid {
		return nil, nil
	}
	lane := conversationAgentRunRequest(conversation, message).ConcurrencyKey
	interventionID := conversationFollowUpInterventionID(conversation.Scope, message.ID)
	for attempt := 0; attempt < 3; attempt++ {
		runs, err := s.runs.store.ListAgentRuns(ctx, AgentRunFilter{
			Scope: conversation.Scope, Owner: &conversation.Owner, Kind: RunKindConversation,
			ConcurrencyKey: lane, Order: AgentRunOrderCreatedDesc, Limit: 50,
		})
		if err != nil {
			return nil, err
		}
		for _, run := range runs {
			for _, intervention := range runOrEmpty(run).PendingInterventions {
				if intervention.ID == interventionID {
					return &AgentRunCommandResult{Run: run}, nil
				}
			}
		}
		var target *AgentRun
		for _, run := range runs {
			eligible, err := conversationFollowUpRunEligible(ctx, s.runs.store, conversation, run)
			if err != nil {
				return nil, err
			}
			if !eligible {
				continue
			}
			triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
			if triggerID == "" || triggerID == message.ID {
				continue
			}
			trigger, err := s.conversations.GetChannelMessage(ctx, conversation.Scope, conversation.ID, triggerID)
			if errors.Is(err, ErrChannelMessageNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if triggerActor, ok := conversationMessageInitiatingUser(conversation, trigger); ok && triggerActor == actor {
				target = run
				break
			}
		}
		if target == nil {
			return nil, nil
		}
		status, wake := target.Status, target.WakeCondition
		if status == AgentRunStatusSleeping {
			// A retry or schedule timer must not delay fresh human input.
			status, wake = AgentRunStatusQueued, nil
		}
		activity := NewRunActivityService(s.runs.store, s.runs.store)
		run, event, err := activity.TransitionRun(ctx, conversation.Scope, target.ID, RunTransitionRequest{
			ExpectedRevision: target.Revision, Status: status, WakeCondition: wake,
			Summary: "Received a new message for this request", EventType: "run.intervened",
			Actor:         ActivityActor{Type: "user", ID: actor.ID},
			CorrelationID: message.ID, Visibility: ActivityVisibilityPrivate,
			Payload: map[string]interface{}{
				"interventionId": interventionID, "conversationId": conversation.ID, "messageId": message.ID,
			},
			Intervention: &AgentRunIntervention{
				ID: interventionID, Actor: ActivityActor{Type: "user", ID: actor.ID},
				Instruction: conversationFollowUpInstruction(message), CreatedAt: message.CreatedAt,
				ConversationMessage: &ConversationMessageDelivery{
					ConversationID: conversation.ID, MessageID: message.ID, Sequence: message.Sequence,
					ThreadRootMessageID: externalConversationThreadRoot(conversation, message), Sender: message.Sender,
				},
			},
		})
		if errors.Is(err, ErrRevisionConflict) || errors.Is(err, ErrInvalidRunTransition) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return &AgentRunCommandResult{Run: run, Event: event}, nil
	}
	return nil, ErrRevisionConflict
}

func runOrEmpty(run *AgentRun) *AgentRun {
	if run == nil {
		return &AgentRun{}
	}
	return run
}

func conversationFollowUpInstruction(message *ChannelMessage) string {
	content := strings.TrimSpace(message.Content)
	if content == "" {
		return "(The user sent a message without text; see its attachments.)"
	}
	if len(content) <= maximumConversationFollowUpInstructionBytes {
		return content
	}
	content = content[:maximumConversationFollowUpInstructionBytes]
	for len(content) > 0 && !utf8.ValidString(content) {
		content = content[:len(content)-1]
	}
	return content
}

// conversationFollowUpMessages loads the delivered follow-ups visible to the
// Agent, preferring the already loaded recent history, in channel order.
func (r *ConversationRunTurnRunner) conversationFollowUpMessages(ctx context.Context, run *AgentRun, conversation *Conversation, viewer ConversationViewer, recent []*ChannelMessage) ([]*ChannelMessage, error) {
	deliveries := conversationRunFollowUps(run)
	if len(deliveries) == 0 {
		return nil, nil
	}
	loaded := make(map[string]*ChannelMessage, len(recent))
	for _, message := range recent {
		if message != nil {
			loaded[message.ID] = message
		}
	}
	result := make([]*ChannelMessage, 0, len(deliveries))
	for _, delivery := range deliveries {
		message := loaded[delivery.MessageID]
		if message == nil {
			visible, err := r.conversations.GetVisibleChannelMessage(ctx, conversation.Scope, conversation.ID, delivery.MessageID, viewer)
			if errors.Is(err, ErrChannelMessageNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			message = visible
		}
		if message != nil {
			result = append(result, message)
		}
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].Sequence < result[j].Sequence })
	return result, nil
}
