package runtime

import (
	"context"
	"strings"
)

// externalItemReply is one Run whose answer an applied inbox item delivers.
// id keys that answer's deliveries: the item ID for the item's own Run, the
// item and member Run IDs for each Team member's answer.
type externalItemReply struct {
	run *AgentRun
	id  string
}

type externalItemReplyStore interface {
	GetConversation(context.Context, Scope, string) (*Conversation, error)
	GetChannelMessage(context.Context, Scope, string, string) (*ChannelMessage, error)
	GetAgentRun(context.Context, Scope, string) (*AgentRun, error)
}

// externalItemReplies returns the Runs answering an applied inbox item: the
// item's own Run, or, once a Team's routing Run has completed, the member
// Runs it started (Output.responderRunIds), since routing writes no answer.
// Each member Run must verify against the routing lineage, or be an earlier
// member Run that took the message as a follow-up; callers skip those like
// any follow-up.
func externalItemReplies(ctx context.Context, store externalItemReplyStore, item *ExternalConversationInboxItem) ([]externalItemReply, error) {
	if item == nil || strings.TrimSpace(item.RunID) == "" {
		return nil, nil
	}
	run, err := store.GetAgentRun(ctx, item.Scope, item.RunID)
	if err != nil || run == nil {
		return nil, err
	}
	if !isTeamRoutingRun(run) || run.Status != AgentRunStatusCompleted {
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
		verified, err := verifyTeamMemberRun(ctx, store, conversation, trigger, member)
		if err != nil {
			return nil, err
		}
		if verified {
			verified = member.Context[teamRoutingRunContextKey] == run.ID
		} else {
			verified = isTeamMemberRun(member) && member.Owner == run.Owner && member.Scope == run.Scope &&
				member.Context[conversationRunContextConversationID] == item.ConversationID && externalInboxItemIsFollowUp(item, member)
		}
		if !verified {
			return nil, ErrExternalConversationConflict
		}
		replies = append(replies, externalItemReply{run: member, id: item.ID + ":" + member.ID})
	}
	return replies, nil
}

// teamResponderRunIDs is the member Run IDs a completed routing Run started.
func teamResponderRunIDs(routing *AgentRun) []string {
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
