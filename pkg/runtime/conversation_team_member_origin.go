package runtime

import (
	"context"
	"errors"
	"strings"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// ConversationRunAgentID is the Agent a chat Run speaks as in conversation:
// the owner of an Agent's chat, or the assigned member of a Team's. It is ""
// when the Run cannot speak there. A Team member is the same Agent its 1:1
// chat has, so everything keyed by "the Agent in this chat" uses this.
//
// It names the acting Agent only and is not an authorization proof: Team
// membership is proved when the member Run is created (verifyTeamMemberRun),
// and the current roster is checked by AgentOnChatRoster.
func ConversationRunAgentID(conversation *Conversation, run *AgentRun) string {
	if conversation == nil || run == nil || run.Owner != conversation.Owner {
		return ""
	}
	switch run.Owner.Type {
	case OwnerTypeAgent:
		if run.AssignedAgentID == run.Owner.ID {
			return run.Owner.ID
		}
	case OwnerTypeTeam:
		return strings.TrimSpace(run.AssignedAgentID)
	}
	return ""
}

// teamMemberRunKey is the idempotency key the routing Run creates a member's
// answer Run with, so the member Run's ID is derived from its routing Run.
func teamMemberRunKey(routing *AgentRun, memberID string) string {
	return "team-member-run:" + hashString(routing.Scope.Kind+"\x00"+routing.Scope.ID+"\x00"+routing.ID+"\x00"+memberID)
}

type teamMemberRunReader interface {
	GetAgentRun(context.Context, Scope, string) (*AgentRun, error)
}

// verifyTeamMemberRun checks a member's answer Run against kernel-owned
// lineage: the routing Run scheduled for trigger created it under the derived
// ID, in the member's lane. Copied context fields alone never pass.
func verifyTeamMemberRun(ctx context.Context, runs teamMemberRunReader, conversation *Conversation, trigger *ChannelMessage, run *AgentRun) (bool, error) {
	if runs == nil || conversation == nil || trigger == nil || !isTeamMemberRun(run) || conversation.Owner.Type != OwnerTypeTeam ||
		run.Owner != conversation.Owner || run.Scope != conversation.Scope || trigger.ConversationID != conversation.ID {
		return false, nil
	}
	routingID, _ := run.Context[teamRoutingRunContextKey].(string)
	expected := conversationAgentRunRequest(conversation, trigger)
	if routingID == "" || routingID != agentrun.RunIDForIdempotencyKey(conversation.Scope, expected.IdempotencyKey) {
		return false, nil
	}
	routing, err := runs.GetAgentRun(ctx, conversation.Scope, routingID)
	if err != nil {
		return false, err
	}
	root, _ := expected.Context["threadRootMessageId"].(string)
	if routing == nil || routing.ID != routingID || routing.Scope != run.Scope || routing.Owner != run.Owner || routing.AssignedAgentID != "" ||
		routing.Kind != RunKindConversation || routing.Context[conversationRunContextConversationID] != conversation.ID ||
		routing.Context[conversationRunContextTriggerID] != trigger.ID {
		return false, nil
	}
	return run.ID == agentrun.RunIDForIdempotencyKey(run.Scope, teamMemberRunKey(routing, run.AssignedAgentID)) &&
		run.ConcurrencyKey == ConversationRunLane(conversation.ID, root, run.AssignedAgentID) &&
		run.Context[conversationRunContextConversationID] == conversation.ID && run.Context[conversationRunContextTriggerID] == trigger.ID, nil
}

// ResolveTeamHandoffRequest returns the person's message behind a Team
// member Run that a teammate's hand-off started: the run's trigger is that
// teammate's answer @mentioning this member, and the answer replies to the
// person. A hand-off is one hop, so the replied-to message must be a person's.
// It returns nil, nil when run is not a hand-off; a hand-off whose lineage
// does not verify is an error.
func ResolveTeamHandoffRequest(ctx context.Context, reader TeamHandoffMessageReader, run *AgentRun, handoff *ChannelMessage) (*ChannelMessage, error) {
	if !isTeamMemberRun(run) || handoff == nil || handoff.Sender.Type != ConversationParticipantAgent {
		return nil, nil
	}
	denied := errors.New("Team hand-off provenance unavailable")
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
	if reader == nil || handoff.ID != triggerID || handoff.ConversationID != conversationID || handoff.Scope != run.Scope ||
		handoff.Sender.ID == run.AssignedAgentID || handoff.Intent != MessageIntentAnswer || handoff.ReplyToMessageID == "" ||
		!strings.HasPrefix(handoff.IdempotencyKey, "agent-channel-response:") ||
		handoff.ID != stableConversationID(run.Scope, handoff.IdempotencyKey, "message") {
		return nil, denied
	}
	mentioned := false
	for _, mention := range handoff.Mentions {
		mentioned = mentioned || mention == (ConversationParticipant{Type: ConversationParticipantAgent, ID: run.AssignedAgentID})
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
	if voiceCallStartedMessage(request) && request.Initiator != nil {
		actor = *request.Initiator
	}
	if actor.Type != ConversationParticipantUser || actor.Validate() != nil {
		return nil, denied
	}
	return request, nil
}
