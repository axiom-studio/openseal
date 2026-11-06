package runtime

import (
	"context"
	"errors"
	"fmt"
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
// Turn; a pending approval is
// replaced by the message (supersedeApprovalForMessage); Runs waiting for an
// event (credential, setup, browser handoff), another Agent, a dependency, or
// paused by the user keep waiting and see the message when they resume. Terminal Runs, Runs promoted to a
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
	switch conversation.Owner.Type {
	case OwnerTypeAgent:
		return s.deliverToConversationLane(ctx, conversation, message, conversationAgentRunRequest(conversation, message).ConcurrencyKey)
	case OwnerTypeTeam:
		return s.deliverToWorkingMembers(ctx, conversation, message)
	}
	return nil, nil
}

// deliverToWorkingMembers is a Team chat's steering: a message that addresses
// nobody (no @mention, @everyone, quote or reply) while members are still
// answering the same person goes to each of them as a follow-up, exactly like
// a 1:1 chat's active Run, instead of being routed as a new question. An
// addressed message is routed; a member it names who is working takes it as
// a follow-up there (routeTeamMessage).
func (s *ConversationRunScheduler) deliverToWorkingMembers(ctx context.Context, conversation *Conversation, message *ChannelMessage) (*AgentRunCommandResult, error) {
	root := externalConversationThreadRoot(conversation, message)
	if message.Sender.Type != ConversationParticipantUser || len(message.Mentions) > 0 || quotedMessageID(message.Quote) != "" ||
		(message.Audience.Kind != "" && message.Audience.Kind != ConversationAudienceChannel) ||
		(message.ReplyToMessageID != "" && message.ReplyToMessageID != root) {
		return nil, nil
	}
	runs, err := s.runs.store.ListAgentRuns(ctx, AgentRunFilter{
		Scope: conversation.Scope, Owner: &conversation.Owner, Kind: RunKindConversation, ConversationID: conversation.ID,
		Order: AgentRunOrderCreatedDesc, Limit: 50,
	})
	if err != nil {
		return nil, err
	}
	var first *AgentRunCommandResult
	lanes := map[string]bool{}
	for _, run := range runs {
		if !isTeamMemberRun(run) || isTerminalAgentRunStatus(run.Status) {
			continue
		}
		lane := conversationRunLane(conversation.ID, root, run.AssignedAgentID)
		if run.ConcurrencyKey != lane || lanes[lane] {
			continue
		}
		lanes[lane] = true
		delivered, err := s.deliverToConversationLane(ctx, conversation, message, lane)
		if err != nil {
			return nil, err
		}
		if first == nil && delivered != nil {
			first = delivered
		}
	}
	return first, nil
}

// deliverToConversationLane is deliverToActiveConversationRun for one lane:
// an Agent channel's, or a Team member's lane in a Team channel, so a member
// already answering the same person takes the new message as a follow-up.
func (s *ConversationRunScheduler) deliverToConversationLane(ctx context.Context, conversation *Conversation, message *ChannelMessage, lane string) (*AgentRunCommandResult, error) {
	if message.Sender.Type != ConversationParticipantUser {
		return nil, nil
	}
	actor, valid := conversationMessageInitiatingUser(conversation, message)
	if !valid {
		return nil, nil
	}
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
					// Replay finishes a supersede interrupted after delivery.
					superseded, err := s.supersedeApprovalForMessage(ctx, run, message)
					if err != nil {
						return nil, err
					}
					if superseded != nil {
						run = superseded.Run
					}
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
		// A message that answers a pending approval replaces it: the action is
		// not carried out and the Run resumes with the message.
		superseded, err := s.supersedeApprovalForMessage(ctx, run, message)
		if err != nil {
			return nil, err
		}
		if superseded != nil {
			run = superseded.Run
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

// supersedeApprovalForMessage closes the pending approval that the Run is
// waiting on when the person answers it with a chat message, which is already
// delivered to the Run as a follow-up. Any approval of this Run, still pending
// and created before the message, qualifies; the action is not carried out and
// the Run resumes with the message. A decision that won the race (the
// person pressed Accept or Decline) is kept: the message then reaches the Run
// as an ordinary follow-up when it resumes. It returns the resolution, or nil
// when nothing was superseded.
func (s *ConversationRunScheduler) supersedeApprovalForMessage(ctx context.Context, run *AgentRun, message *ChannelMessage) (*ApprovalResolutionResult, error) {
	if run == nil || message == nil || !conversationRunHasFollowUp(run, message.ID) {
		return nil, nil
	}
	return s.replaceApprovalWithMessage(ctx, run, message)
}

// replaceApprovalWithMessage closes the Run's pending approval as replaced by
// the message, whether or not the message was delivered to the Run.
func (s *ConversationRunScheduler) replaceApprovalWithMessage(ctx context.Context, run *AgentRun, message *ChannelMessage) (*ApprovalResolutionResult, error) {
	if run == nil || message == nil || run.Status != AgentRunStatusWaitingForApproval || run.WakeCondition == nil ||
		run.WakeCondition.Type != "approval" || run.WakeCondition.Reference == "" {
		return nil, nil
	}
	approval, err := s.runs.store.GetApproval(ctx, run.Scope, run.WakeCondition.Reference)
	if errors.Is(err, ErrApprovalNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if approval == nil || approval.Status != ApprovalStatusPending || approval.RunID != run.ID || !approval.CreatedAt.Before(message.CreatedAt) {
		return nil, nil
	}
	call, err := s.runs.store.GetActionCall(ctx, run.Scope, approval.ActionCallID)
	if errors.Is(err, ErrActionNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if call == nil || call.Status != ActionCallStatusWaitingApproval || call.ApprovalID != approval.ID {
		return nil, nil
	}
	result, err := s.approvals.ResolveSupersededByMessage(ctx, run.Scope, approval.ID, approval.Revision, message.ID)
	if errors.Is(err, ErrApprovalResolved) || errors.Is(err, ErrRevisionConflict) || errors.Is(err, ErrInvalidRunTransition) || errors.Is(err, ErrApprovalExpired) {
		return nil, nil
	}
	if err != nil || result == nil || !result.Resolved {
		return nil, err
	}
	return result, nil
}

// replaceOtherMemberApprovals applies a Team message to the members it was not
// routed to: the person's pending approvals in their lanes of this chat are
// replaced by the message, like a 1:1 chat's. Those Runs then stop without a
// reply (replacedTeamApprovalOutcome); the routed member answers the message.
func (s *ConversationRunScheduler) replaceOtherMemberApprovals(ctx context.Context, conversation *Conversation, message *ChannelMessage, root string, routed map[string]bool) error {
	if conversation.Owner.Type != OwnerTypeTeam || message.Sender.Type != ConversationParticipantUser {
		return nil
	}
	actor, valid := conversationMessageInitiatingUser(conversation, message)
	if !valid {
		return nil
	}
	runs, err := s.runs.store.ListAgentRuns(ctx, AgentRunFilter{
		Scope: conversation.Scope, Owner: &conversation.Owner, Kind: RunKindConversation,
		Statuses: []AgentRunStatus{AgentRunStatusWaitingForApproval}, Order: AgentRunOrderCreatedDesc, Limit: 50,
	})
	if err != nil {
		return err
	}
	for _, run := range runs {
		if !isTeamMemberRun(run) || routed[run.AssignedAgentID] || run.ConcurrencyKey != conversationRunLane(conversation.ID, root, run.AssignedAgentID) {
			continue
		}
		if conversationID, _ := run.Context[conversationRunContextConversationID].(string); conversationID != conversation.ID {
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
			return err
		}
		if triggerActor, ok := conversationMessageInitiatingUser(conversation, trigger); !ok || triggerActor != actor {
			continue
		}
		if _, err := s.replaceApprovalWithMessage(ctx, run, message); err != nil {
			return err
		}
	}
	return nil
}

// replacedTeamApprovalOutcome stops a Team member Run whose approval a message
// routed to another member replaced: the action was not carried out and the
// chat already shows that, so the member posts nothing.
func replacedTeamApprovalOutcome(run *AgentRun) *TurnOutcome {
	if !isTeamMemberRun(run) {
		return nil
	}
	last, _ := run.Checkpoint["lastAction"].(map[string]interface{})
	messageID, _ := last["supersededByMessageId"].(string)
	if fmt.Sprint(last["approvalStatus"]) != string(ApprovalStatusSuperseded) || messageID == "" || conversationRunHasFollowUp(run, messageID) {
		return nil
	}
	return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Replaced by a newer message for another member",
		RunOutput: map[string]interface{}{"approvalReplaced": true, "replacedByMessageId": messageID}}
}
