package runtime

import (
	"context"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
)

// externalItemReply is one Run whose answer an applied inbox item delivers.
// id keys that answer's deliveries: the item ID for the item's own Run, the
// item and member Run IDs for each Team member's answer.
type externalItemReply struct {
	run *models.AgentRun
	id  string
}

type externalItemReplyStore interface {
	GetConversation(context.Context, models.Scope, string) (*models.Conversation, error)
	GetChannelMessage(context.Context, models.Scope, string, string) (*models.ChannelMessage, error)
	GetAgentRun(context.Context, models.Scope, string) (*models.AgentRun, error)
}

// externalItemReplies returns the Runs answering an applied inbox item: the
// item's own Run, or, once a Team's routing Run has completed, the member
// Runs it started (Output.responderRunIds), since routing writes no answer.
// Each member Run must verify against the routing lineage, or be an earlier
// member Run that took the message as a follow-up; callers skip those like
// any follow-up.
func externalItemReplies(ctx context.Context, store externalItemReplyStore, item *models.ExternalConversationInboxItem) ([]externalItemReply, error) {
	if item == nil || strings.TrimSpace(item.RunID) == "" {
		return nil, nil
	}
	run, err := store.GetAgentRun(ctx, item.Scope, item.RunID)
	if err != nil || run == nil {
		return nil, err
	}
	if !runtimeconversation.IsTeamRoutingRun(run) || run.Status != models.AgentRunStatusCompleted {
		return []externalItemReply{{run: run, id: item.ID}}, nil
	}
	conversation, err := store.GetConversation(ctx, item.Scope, item.ConversationID)
	if err != nil {
		return nil, err
	}
	trigger, err := store.GetChannelMessage(ctx, item.Scope, item.ConversationID, item.ChannelMessageID)
	if err != nil {
		return nil, err
	}
	ids := teamResponderRunIDs(run)
	replies := make([]externalItemReply, 0, len(ids))
	for _, id := range ids {
		member, err := store.GetAgentRun(ctx, item.Scope, id)
		if err != nil {
			return nil, err
		}
		verified, err := runtimeconversation.VerifyTeamMemberRun(ctx, store, conversation, trigger, member)
		if err != nil {
			return nil, err
		}
		if verified {
			verified = member.Context[runtimeconversation.TeamRoutingRunContextKey] == run.ID
		} else {
			verified = models.IsTeamMemberRun(member) && member.Owner == run.Owner && member.Scope == run.Scope &&
				member.Context[models.ConversationRunConversationIDContextKey] == item.ConversationID && conversationrun.ExternalInboxItemIsFollowUp(item, member)
		}
		if !verified {
			return nil, models.ErrExternalConversationConflict
		}
		replies = append(replies, externalItemReply{run: member, id: item.ID + ":" + member.ID})
	}
	return replies, nil
}

// teamResponderRunIDs is the member Run IDs a completed routing Run started.
func teamResponderRunIDs(routing *models.AgentRun) []string {
	var ids []string
	switch values := routing.Output["responderRunIds"].(type) {
	case []string:
		ids = append(ids, values...)
	case []interface{}:
		for _, value := range values {
			if id, ok := value.(string); ok {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// externalItemReplyAgents is the Agents answering an item, whose lanes keep
// its thread working.
func externalItemReplyAgents(replies []externalItemReply) []string {
	agents := make([]string, 0, len(replies))
	for _, reply := range replies {
		if id := strings.TrimSpace(reply.run.AssignedAgentID); id != "" {
			agents = append(agents, id)
		}
	}
	return agents
}
