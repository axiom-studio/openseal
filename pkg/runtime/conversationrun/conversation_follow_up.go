package conversationrun

import (
	"context"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

const maximumConversationFollowUpInstructionBytes = 16000

func conversationRunHasFollowUp(run *models.AgentRun, messageID string) bool {
	for _, delivery := range models.ConversationRunFollowUps(run) {
		if delivery.MessageID == messageID {
			return true
		}
	}
	return false
}

// ExternalInboxItemIsFollowUp reports whether an inbox item's message was
// delivered into another message's Run. That Run's own inbox item projects
// its progress and reply; the follow-up item must not duplicate them.
func ExternalInboxItemIsFollowUp(item *models.ExternalConversationInboxItem, run *models.AgentRun) bool {
	if item == nil || run == nil {
		return false
	}
	trigger, _ := run.Context[models.ConversationRunTriggerIDContextKey].(string)
	return trigger != item.ChannelMessageID && conversationRunHasFollowUp(run, item.ChannelMessageID)
}

// conversationFollowUpRunEligible reports whether a foreground conversation
// Run can still consume new human input. Any non-terminal state qualifies:
// queued, planning, running and sleeping Runs see the message at their next
// step; a pending approval is replaced by the message
// (supersedeApprovalForMessage); Runs waiting for an event (credential,
// setup, browser handoff), another Agent, a dependency, or paused by the user
// keep waiting and see the message when they resume. Terminal Runs, Runs
// promoted to a background conversation task, Runs explaining their failure
// (FailureExplanation set) and pinned Runbook Runs never absorb new messages.
func conversationFollowUpRunEligible(ctx context.Context, store models.PortfolioStore, conversation *models.Conversation, run *models.AgentRun) (bool, error) {
	if run == nil || run.Kind != models.RunKindConversation || run.Scope != conversation.Scope || run.Owner != conversation.Owner ||
		models.IsTerminalAgentRunStatus(run.Status) || run.FailureExplanation != nil || (run.Plan != nil && run.Plan["runbook"] != nil) {
		return false, nil
	}
	if conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string); conversationID != conversation.ID {
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
// thread of it) when that Run was started by the same person. The message is
// recorded as a message-backed intervention, which bumps the Run's revision:
// a final answer drafted before it is not posted, and the Run's next step
// ingests the message into its session through catch-up (a user entry, once).
// One Run handles the whole exchange instead of parallel Runs competing for
// the lane. A nil result means no Run absorbed the message.
func (s *Scheduler) deliverToActiveConversationRun(ctx context.Context, conversation *models.Conversation, message *models.ChannelMessage) (*agentrun.CommandResult, error) {
	switch conversation.Owner.Type {
	case models.OwnerTypeAgent:
		return s.deliverToConversationLane(ctx, conversation, message, runtimeconversation.AgentRunRequest(conversation, message).ConcurrencyKey)
	case models.OwnerTypeTeam:
		return s.deliverToWorkingMembers(ctx, conversation, message)
	}
	return nil, nil
}

// deliverToWorkingMembers is a Team chat's steering: a message that addresses
// nobody (no @mention, @everyone, quote or reply) while members are still
// answering the same person goes to each of them as a follow-up, exactly like
// a 1:1 chat's active Run, instead of being routed as a new question. Each
// member has its own lane session, so every member's next step appends the
// message to its own session. An
// addressed message is routed; a member it names who is working takes it as
// a follow-up there (routeTeamMessage).
func (s *Scheduler) deliverToWorkingMembers(ctx context.Context, conversation *models.Conversation, message *models.ChannelMessage) (*agentrun.CommandResult, error) {
	root := models.ExternalConversationThreadRoot(conversation, message)
	if message.Sender.Type != models.ConversationParticipantUser || len(message.Mentions) > 0 || runtimeconversation.QuotedMessageID(message.Quote) != "" ||
		(message.Audience.Kind != "" && message.Audience.Kind != models.ConversationAudienceChannel) ||
		(message.ReplyToMessageID != "" && message.ReplyToMessageID != root) {
		return nil, nil
	}
	runs, err := s.runStore.ListAgentRuns(ctx, models.AgentRunFilter{
		Scope: conversation.Scope, Owner: &conversation.Owner, Kind: models.RunKindConversation, ConversationID: conversation.ID,
		Order: models.AgentRunOrderCreatedDesc, Limit: 50,
	})
	if err != nil {
		return nil, err
	}
	var first *agentrun.CommandResult
	lanes := map[string]bool{}
	for _, run := range runs {
		if !models.IsTeamMemberRun(run) || models.IsTerminalAgentRunStatus(run.Status) {
			continue
		}
		lane := models.ConversationRunLane(conversation.ID, root, run.AssignedAgentID)
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
func (s *Scheduler) deliverToConversationLane(ctx context.Context, conversation *models.Conversation, message *models.ChannelMessage, lane string) (*agentrun.CommandResult, error) {
	if message.Sender.Type != models.ConversationParticipantUser {
		return nil, nil
	}
	actor, valid := runtimeconversation.MessageInitiatingUser(conversation, message)
	if !valid {
		return nil, nil
	}
	interventionID := models.ConversationFollowUpInterventionID(conversation.Scope, message.ID)
	for attempt := 0; attempt < 3; attempt++ {
		runs, err := s.runStore.ListAgentRuns(ctx, models.AgentRunFilter{
			Scope: conversation.Scope, Owner: &conversation.Owner, Kind: models.RunKindConversation,
			ConcurrencyKey: lane, Order: models.AgentRunOrderCreatedDesc, Limit: 50,
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
					return &agentrun.CommandResult{Run: run}, nil
				}
			}
		}
		var target *models.AgentRun
		for _, run := range runs {
			eligible, err := conversationFollowUpRunEligible(ctx, s.runStore, conversation, run)
			if err != nil {
				return nil, err
			}
			if !eligible {
				continue
			}
			triggerID, _ := run.Context[models.ConversationRunTriggerIDContextKey].(string)
			if triggerID == "" || triggerID == message.ID {
				continue
			}
			trigger, err := s.conversations.GetChannelMessage(ctx, conversation.Scope, conversation.ID, triggerID)
			if errors.Is(err, models.ErrChannelMessageNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if triggerActor, ok := runtimeconversation.MessageInitiatingUser(conversation, trigger); ok && triggerActor == actor {
				target = run
				break
			}
		}
		if target == nil {
			return nil, nil
		}
		status, wake := target.Status, target.WakeCondition
		if status == models.AgentRunStatusSleeping {
			// A retry or schedule timer must not delay fresh human input.
			status, wake = models.AgentRunStatusQueued, nil
		}
		run, event, err := s.runs.TransitionRun(ctx, conversation.Scope, target.ID, activity.RunTransitionRequest{
			ExpectedRevision: target.Revision, Status: status, WakeCondition: wake,
			Summary: "Received a new message for this request", EventType: "run.intervened",
			Actor:         models.ActivityActor{Type: "user", ID: actor.ID},
			CorrelationID: message.ID, Visibility: models.ActivityVisibilityPrivate,
			Payload: map[string]interface{}{
				"interventionId": interventionID, "conversationId": conversation.ID, "messageId": message.ID,
			},
			Intervention: &models.AgentRunIntervention{
				ID: interventionID, Actor: models.ActivityActor{Type: "user", ID: actor.ID},
				Instruction: conversationFollowUpInstruction(message), CreatedAt: message.CreatedAt,
				ConversationMessage: &models.ConversationMessageDelivery{
					ConversationID: conversation.ID, MessageID: message.ID, Sequence: message.Sequence,
					ThreadRootMessageID: models.ExternalConversationThreadRoot(conversation, message), Sender: message.Sender,
				},
			},
		})
		if errors.Is(err, models.ErrRevisionConflict) || errors.Is(err, models.ErrInvalidRunTransition) {
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
		return &agentrun.CommandResult{Run: run, Event: event}, nil
	}
	return nil, models.ErrRevisionConflict
}

func runOrEmpty(run *models.AgentRun) *models.AgentRun {
	if run == nil {
		return &models.AgentRun{}
	}
	return run
}

func conversationFollowUpInstruction(message *models.ChannelMessage) string {
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
// Agent, preferring the already loaded recent history, in channel order. It
// serves only the old goal-building path in conversation_run.go and goes with
// it; loop Runs read follow-ups from their session (catch-up).
func (r *TurnRunner) conversationFollowUpMessages(ctx context.Context, run *models.AgentRun, conversation *models.Conversation, viewer models.ConversationViewer, recent []*models.ChannelMessage) ([]*models.ChannelMessage, error) {
	deliveries := models.ConversationRunFollowUps(run)
	if len(deliveries) == 0 {
		return nil, nil
	}
	loaded := make(map[string]*models.ChannelMessage, len(recent))
	for _, message := range recent {
		if message != nil {
			loaded[message.ID] = message
		}
	}
	result := make([]*models.ChannelMessage, 0, len(deliveries))
	for _, delivery := range deliveries {
		message := loaded[delivery.MessageID]
		if message == nil {
			visible, err := r.conversations.GetVisibleChannelMessage(ctx, conversation.Scope, conversation.ID, delivery.MessageID, viewer)
			if errors.Is(err, models.ErrChannelMessageNotFound) {
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
func (s *Scheduler) supersedeApprovalForMessage(ctx context.Context, run *models.AgentRun, message *models.ChannelMessage) (*models.ApprovalResolutionResult, error) {
	if run == nil || message == nil || !conversationRunHasFollowUp(run, message.ID) {
		return nil, nil
	}
	return s.replaceApprovalWithMessage(ctx, run, message)
}

// replaceApprovalWithMessage closes the Run's pending approval as replaced by
// the message, whether or not the message was delivered to the Run.
func (s *Scheduler) replaceApprovalWithMessage(ctx context.Context, run *models.AgentRun, message *models.ChannelMessage) (*models.ApprovalResolutionResult, error) {
	if run == nil || message == nil || run.Status != models.AgentRunStatusWaitingForApproval || run.WakeCondition == nil ||
		run.WakeCondition.Type != "approval" || run.WakeCondition.Reference == "" {
		return nil, nil
	}
	approval, err := s.runStore.GetApproval(ctx, run.Scope, run.WakeCondition.Reference)
	if errors.Is(err, models.ErrApprovalNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if approval == nil || approval.Status != models.ApprovalStatusPending || approval.RunID != run.ID || !approval.CreatedAt.Before(message.CreatedAt) {
		return nil, nil
	}
	call, err := s.runStore.GetActionCall(ctx, run.Scope, approval.ActionCallID)
	if errors.Is(err, models.ErrActionNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if call == nil || call.Status != models.ActionCallStatusWaitingApproval || call.ApprovalID != approval.ID {
		return nil, nil
	}
	result, err := s.approvals.ResolveSupersededByMessage(ctx, run.Scope, approval.ID, approval.Revision, message.ID)
	if errors.Is(err, models.ErrApprovalResolved) || errors.Is(err, models.ErrRevisionConflict) || errors.Is(err, models.ErrInvalidRunTransition) || errors.Is(err, action.ErrApprovalExpired) {
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
// reply (conversationLoop.replacedTeamApproval); the routed member answers
// the message.
func (s *Scheduler) replaceOtherMemberApprovals(ctx context.Context, conversation *models.Conversation, message *models.ChannelMessage, root string, routed map[string]bool) error {
	if conversation.Owner.Type != models.OwnerTypeTeam || message.Sender.Type != models.ConversationParticipantUser {
		return nil
	}
	actor, valid := runtimeconversation.MessageInitiatingUser(conversation, message)
	if !valid {
		return nil
	}
	runs, err := s.runStore.ListAgentRuns(ctx, models.AgentRunFilter{
		Scope: conversation.Scope, Owner: &conversation.Owner, Kind: models.RunKindConversation,
		Statuses: []models.AgentRunStatus{models.AgentRunStatusWaitingForApproval}, Order: models.AgentRunOrderCreatedDesc, Limit: 50,
	})
	if err != nil {
		return err
	}
	for _, run := range runs {
		if !models.IsTeamMemberRun(run) || routed[run.AssignedAgentID] || run.ConcurrencyKey != models.ConversationRunLane(conversation.ID, root, run.AssignedAgentID) {
			continue
		}
		if conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string); conversationID != conversation.ID {
			continue
		}
		triggerID, _ := run.Context[models.ConversationRunTriggerIDContextKey].(string)
		if triggerID == "" || triggerID == message.ID {
			continue
		}
		trigger, err := s.conversations.GetChannelMessage(ctx, conversation.Scope, conversation.ID, triggerID)
		if errors.Is(err, models.ErrChannelMessageNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if triggerActor, ok := runtimeconversation.MessageInitiatingUser(conversation, trigger); !ok || triggerActor != actor {
			continue
		}
		if _, err := s.replaceApprovalWithMessage(ctx, run, message); err != nil {
			return err
		}
	}
	return nil
}
