package runtime

import (
	"context"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// routeTeamMessage is a Team channel's routing Run: it records who answers the
// message and starts one member conversation Run per responder, the same Run a
// 1:1 Agent chat gets. It writes no answer itself; the result is the Run's
// output, whose responderRunIds the chat client follows.
func (r *ConversationRunTurnRunner) routeTeamMessage(ctx context.Context, run *AgentRun, conversation *Conversation, triggerID string) (map[string]interface{}, error) {
	if r.runs == nil || r.coordinator == nil {
		return nil, ErrConversationCoordinationUnavailable
	}
	trigger, err := r.conversations.GetChannelMessage(ctx, run.Scope, conversation.ID, triggerID)
	if err != nil {
		return nil, err
	}
	childKey := func(participant ConversationParticipant) string { return teamMemberRunKey(run, participant.ID) }
	result, err := r.coordinator.coordinateWithChildRuns(ctx, ConversationRoutingRequest{
		Scope: run.Scope, ConversationID: conversation.ID, TriggerMessageID: triggerID,
		IdempotencyKey: conversationRoutingKey(run.Scope, conversation.ID, triggerID),
	}, childKey)
	if err != nil {
		if ctx.Err() != nil || permanentConversationRunError(err) {
			return nil, err
		}
		return nil, conversationRetryError(err)
	}
	// A member already answering this person takes the message as a follow-up,
	// like a 1:1 chat's active Run; nobody's work is interrupted.
	followUps := &ConversationRunScheduler{
		conversations: r.conversations, runs: r.runs, conversationStore: r.conversationStore, runStore: r.runStore,
		approvals: action.NewApprovalCoordinator(r.runStore, r.runStore, nil, models.WithClock(r.now)),
	}
	root, _ := run.Context["threadRootMessageId"].(string)
	runIDs := make([]interface{}, 0, len(result.Round.Responders))
	routed := map[string]bool{}
	for _, responder := range result.Round.Responders {
		routed[responder.Participant.ID] = true
	}
	if err := followUps.replaceOtherMemberApprovals(ctx, conversation, trigger, root, routed); err != nil {
		return nil, err
	}
	for _, responder := range result.Round.Responders {
		key := childKey(responder.Participant)
		existing, err := r.runStore.GetAgentRun(ctx, run.Scope, agentrun.RunIDForIdempotencyKey(run.Scope, key))
		if err != nil {
			return nil, err
		}
		if existing == nil {
			delivered, err := followUps.deliverToConversationLane(ctx, conversation, trigger, ConversationRunLane(conversation.ID, root, responder.Participant.ID))
			if err != nil {
				return nil, err
			}
			if delivered != nil {
				runIDs = append(runIDs, delivered.Run.ID)
				continue
			}
		}
		created, err := r.runs.CreateAgentRun(ctx, teamMemberRunRequest(run, conversation, trigger, responder, key))
		if err != nil {
			return nil, err
		}
		runIDs = append(runIDs, created.Run.ID)
	}
	return map[string]interface{}{
		"conversationId": conversation.ID, "triggerMessageId": triggerID,
		"participationRoundId": result.Round.ID, "responderRunIds": runIDs, "replayed": result.Replayed,
	}, nil
}

// teamMembers lists the members who may answer in the channel, with the
// names an @mention uses.
func (r *ConversationRunTurnRunner) teamMembers(ctx context.Context, conversation *Conversation, trigger *ChannelMessage) (conversationTeamRoster, []ConversationParticipantBinding, error) {
	roster := loadConversationTeamRoster(ctx, r.conversationStore, conversation)
	if r.coordinator == nil {
		return roster, nil, nil
	}
	bindings, err := r.coordinator.resolveParticipants(ctx, ConversationParticipantQuery{
		Conversation: cloneConversation(conversation), Trigger: cloneChannelMessage(trigger),
	})
	if errors.Is(err, ErrNoConversationParticipants) {
		return roster, nil, nil
	}
	return roster, bindings, err
}

func (r *ConversationRunTurnRunner) agentConversationTeamContext(ctx context.Context, run *AgentRun, conversation *Conversation, trigger *ChannelMessage) (*agentConversationTeam, error) {
	roster, bindings, err := r.teamMembers(ctx, conversation, trigger)
	if err != nil {
		return nil, err
	}
	return roster.agentTeam(bindings, run.AssignedAgentID), nil
}

// handoffMentions finds the teammates an answer @mentions, in the order they
// appear. The sender, unknown names and @everyone are ignored: an Agent's
// handoff never fans out to the whole Team.
func (r *ConversationRunTurnRunner) handoffMentions(ctx context.Context, conversation *Conversation, trigger *ChannelMessage, sender string, content string) ([]ConversationParticipant, error) {
	if !strings.Contains(content, "@") {
		return nil, nil
	}
	roster, bindings, err := r.teamMembers(ctx, conversation, trigger)
	if err != nil {
		return nil, err
	}
	return findHandoffMentions(content, sender, roster, bindings), nil
}
