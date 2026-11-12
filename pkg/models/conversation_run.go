package models

import (
	"context"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
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

func ConversationFollowUpInterventionID(scope Scope, messageID string) string {
	return StableConversationID(scope, "conversation-follow-up:"+messageID, "intervention")
}

// ConversationRunFollowUps returns the messages delivered to this foreground
// Run after its trigger, in delivery order.
func ConversationRunFollowUps(run *AgentRun) []ConversationMessageDelivery {
	if run == nil {
		return nil
	}
	conversationID, _ := run.Context[ConversationRunConversationIDContextKey].(string)
	var result []ConversationMessageDelivery
	for _, intervention := range run.PendingInterventions {
		delivery := intervention.ConversationMessage
		if delivery != nil && delivery.ConversationID == conversationID && delivery.MessageID != "" &&
			intervention.ID == ConversationFollowUpInterventionID(run.Scope, delivery.MessageID) {
			result = append(result, *delivery)
		}
	}
	return result
}

const (
	ConversationRunConversationIDContextKey = "conversationId"
	ConversationRunTriggerIDContextKey      = "triggerMessageId"
	ConversationRunSchedulerParticipant     = "conversation-run-scheduler"
)

func ValidateConversationRun(run *AgentRun) error {
	if run == nil || run.Kind != RunKindConversation || (run.Owner.Type != OwnerTypeTeam && run.Owner.Type != OwnerTypeAgent) {
		return fmt.Errorf("%w: Team- or Agent-owned conversation Run is required", ErrInvalidAgentRun)
	}
	if run.Owner.Type == OwnerTypeAgent && run.AssignedAgentID != run.Owner.ID {
		return fmt.Errorf("%w: Agent-owned conversation Run must be assigned to its owner", ErrInvalidAgentRun)
	}
	conversationID, conversationOK := run.Context[ConversationRunConversationIDContextKey].(string)
	triggerID, triggerOK := run.Context[ConversationRunTriggerIDContextKey].(string)
	root, _ := run.Context["threadRootMessageId"].(string)
	if root != "" && !IsValidOpaqueIdentifier(root, 128) {
		return ErrInvalidAgentRun
	}
	reason, _ := run.Context[RoutingReasonContextKey].(string)
	if !conversationOK || !triggerOK || !IsValidOpaqueIdentifier(conversationID, 128) || !IsValidOpaqueIdentifier(triggerID, 128) ||
		run.ConcurrencyKey != ConversationRunLane(conversationID, root, run.AssignedAgentID) || len(reason) > MaximumRoutingReasonBytes {
		return fmt.Errorf("%w: conversation Run context or concurrency key is invalid", ErrInvalidAgentRun)
	}
	return ValidateConversationRunSession(run)
}

// External provider messages never inherit the agent’s other private work.
func IsExternalChannelConversation(conversation *Conversation) bool {
	return conversation != nil && conversation.Origin != nil && conversation.Origin.Kind == ConversationReferenceExternalSource
}

// An explicit thread remains inside its conversation. Its root scopes model
// context and execution; unthreaded application chats have no such scope.
func ExternalConversationThreadRoot(conversation *Conversation, trigger *ChannelMessage) string {
	if trigger == nil {
		return ""
	}
	// Telegram DM reply IDs identify quoted messages, not separate conversations.
	// Keep real Telegram topics isolated, and retain Slack's thread semantics.
	if source := trigger.ExternalSource; IsExternalChannelConversation(conversation) && source != nil &&
		source.Provider == "telegram" && source.ChannelType == "private" &&
		!strings.HasPrefix(source.ThreadID, "topic:") && !strings.HasPrefix(source.ThreadID, "direct-topic:") {
		return ""
	}
	if trigger.ThreadRootID != "" {
		return trigger.ThreadRootID
	}
	if trigger.StartsThread {
		return trigger.ID
	}
	if IsExternalChannelConversation(conversation) {
		return trigger.ID
	}
	return ""
}

// TeamRosterReader reads the current Team deployment for roster checks.
type TeamRosterReader interface {
	GetTeamDeployment(context.Context, capability.ScopeReference, string) (*kernelteam.Deployment, error)
}

type TeamHandoffMessageReader interface {
	GetChannelMessage(context.Context, Scope, string, string) (*ChannelMessage, error)
}

const (
	// RoutingReasonContextKey carries the router's reason onto the answer.
	RoutingReasonContextKey = "routingReason"
)

// IsTeamMemberRun reports whether run is a Team member's answer to a Team
// channel message: an ordinary conversation Run, assigned to that member.
func IsTeamMemberRun(run *AgentRun) bool {
	return run != nil && run.Kind == RunKindConversation && run.Owner.Type == OwnerTypeTeam &&
		strings.TrimSpace(run.AssignedAgentID) != ""
}

// ConversationRunLane is a conversation Run's concurrency key: the channel or
// one thread of it, then the Agent working in it. It is the same rule for an
// Agent's own chat and a Team member's, so once a member is assigned its Run
// is an Agent Run like any other. Only a Team's unassigned routing Run keeps
// the bare channel lane. Hosts that verify a Run's chat origin compare against
// it.
func ConversationRunLane(conversationID, threadRoot, assignedAgentID string) string {
	lane := conversationID
	if threadRoot != "" {
		lane += ":thread:" + threadRoot
	}
	if assignedAgentID != "" {
		lane += ":agent:" + assignedAgentID
	}
	return lane
}

// ReplyRunGuard fences a final reply: the reply commits only while the Run is
// running under WorkerID's live lease with InterventionCount interventions.
type ReplyRunGuard struct {
	Scope             Scope
	RunID             string
	WorkerID          string
	ConcurrencyKey    string
	InterventionCount int
}
