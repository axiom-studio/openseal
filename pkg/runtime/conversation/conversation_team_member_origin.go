package conversation

import (
	"context"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// RunAgentID is the Agent a chat Run speaks as in conversation:
// the owner of an Agent's chat, or the assigned member of a Team's. It is ""
// when the Run cannot speak there. A Team member is the same Agent its 1:1
// chat has, so everything keyed by "the Agent in this chat" uses this.
//
// It names the acting Agent only and is not an authorization proof: Team
// membership is proved when the member Run is created (verifyTeamMemberRun),
// and the current roster is checked by AgentOnChatRoster.
func RunAgentID(conversation *models.Conversation, run *models.AgentRun) string {
	if conversation == nil || run == nil || run.Owner != conversation.Owner {
		return ""
	}
	switch run.Owner.Type {
	case models.OwnerTypeAgent:
		if run.AssignedAgentID == run.Owner.ID {
			return run.Owner.ID
		}
	case models.OwnerTypeTeam:
		return strings.TrimSpace(run.AssignedAgentID)
	}
	return ""
}

// TeamMemberRunKey is the idempotency key the routing Run creates a member's
// answer Run with, so the member Run's ID is derived from its routing Run.
func TeamMemberRunKey(routing *models.AgentRun, memberID string) string {
	return "team-member-run:" + models.SHA256DigestString(routing.Scope.Kind+"\x00"+routing.Scope.ID+"\x00"+routing.ID+"\x00"+memberID)
}

type teamMemberRunReader interface {
	GetAgentRun(context.Context, models.Scope, string) (*models.AgentRun, error)
}

// VerifyTeamMemberRun checks a member's answer Run against kernel-owned
// lineage: the routing Run scheduled for trigger created it under the derived
// ID, in the member's lane. Copied context fields alone never pass.
func VerifyTeamMemberRun(ctx context.Context, runs teamMemberRunReader, conversation *models.Conversation, trigger *models.ChannelMessage, run *models.AgentRun) (bool, error) {
	if runs == nil || conversation == nil || trigger == nil || !models.IsTeamMemberRun(run) || conversation.Owner.Type != models.OwnerTypeTeam ||
		run.Owner != conversation.Owner || run.Scope != conversation.Scope || trigger.ConversationID != conversation.ID {
		return false, nil
	}
	routingID, _ := run.Context[TeamRoutingRunContextKey].(string)
	expected := AgentRunRequest(conversation, trigger)
	if routingID == "" || routingID != agentrun.RunIDForIdempotencyKey(conversation.Scope, expected.IdempotencyKey) {
		return false, nil
	}
	routing, err := runs.GetAgentRun(ctx, conversation.Scope, routingID)
	if err != nil {
		return false, err
	}
	root, _ := expected.Context["threadRootMessageId"].(string)
	if routing == nil || routing.ID != routingID || routing.Scope != run.Scope || routing.Owner != run.Owner || routing.AssignedAgentID != "" ||
		routing.Kind != models.RunKindConversation || routing.Context[models.ConversationRunConversationIDContextKey] != conversation.ID ||
		routing.Context[models.ConversationRunTriggerIDContextKey] != trigger.ID {
		return false, nil
	}
	return run.ID == agentrun.RunIDForIdempotencyKey(run.Scope, TeamMemberRunKey(routing, run.AssignedAgentID)) &&
		run.ConcurrencyKey == models.ConversationRunLane(conversation.ID, root, run.AssignedAgentID) &&
		run.Context[models.ConversationRunConversationIDContextKey] == conversation.ID && run.Context[models.ConversationRunTriggerIDContextKey] == trigger.ID, nil
}

// ResolveTeamHandoffRequest returns the person's message behind a Team
// member Run that a teammate's hand-off started: the run's trigger is that
// teammate's answer @mentioning this member, and the answer replies to the
// person. A hand-off is one hop, so the replied-to message must be a person's.
// It returns nil, nil when run is not a hand-off; a hand-off whose lineage
// does not verify is an error.
func ResolveTeamHandoffRequest(ctx context.Context, reader models.TeamHandoffMessageReader, run *models.AgentRun, handoff *models.ChannelMessage) (*models.ChannelMessage, error) {
	if !models.IsTeamMemberRun(run) || handoff == nil || handoff.Sender.Type != models.ConversationParticipantAgent {
		return nil, nil
	}
	denied := errors.New("Team hand-off provenance unavailable")
	conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
	triggerID, _ := run.Context[models.ConversationRunTriggerIDContextKey].(string)
	if reader == nil || handoff.ID != triggerID || handoff.ConversationID != conversationID || handoff.Scope != run.Scope ||
		handoff.Sender.ID == run.AssignedAgentID || handoff.Intent != models.MessageIntentAnswer || handoff.ReplyToMessageID == "" ||
		!strings.HasPrefix(handoff.IdempotencyKey, "agent-channel-response:") ||
		handoff.ID != models.StableConversationID(run.Scope, handoff.IdempotencyKey, "message") {
		return nil, denied
	}
	mentioned := false
	for _, mention := range handoff.Mentions {
		mentioned = mentioned || mention == (models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: run.AssignedAgentID})
	}
	if !mentioned {
		return nil, denied
	}
	request, err := reader.GetChannelMessage(ctx, run.Scope, conversationID, handoff.ReplyToMessageID)
	if err != nil || request == nil || request.ID != handoff.ReplyToMessageID || request.Scope != run.Scope ||
		request.ConversationID != conversationID || request.Sequence >= handoff.Sequence {
		return nil, denied
	}
	actor := request.Sender
	if models.VoiceCallStartedMessage(request) && request.Initiator != nil {
		actor = *request.Initiator
	}
	if actor.Type != models.ConversationParticipantUser || actor.Validate() != nil {
		return nil, denied
	}
	return request, nil
}
