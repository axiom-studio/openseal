package runtime

import (
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
)

const (
	conversationRunContextTriggerSeq = "triggerSequence"
)

// conversationMessageInitiatingUser resolves only the canonical scoped human
// identity. A host-owned voice-call start may name its authenticated initiator;
// arbitrary service metadata and copied Run context never grant that authority.
func conversationMessageInitiatingUser(conversation *models.Conversation, message *models.ChannelMessage) (models.ConversationParticipant, bool) {
	if conversation == nil || message == nil || message.Scope != conversation.Scope || message.ConversationID != conversation.ID {
		return models.ConversationParticipant{}, false
	}
	actor := message.Sender
	if models.VoiceCallStartedMessage(message) && message.Initiator != nil {
		actor = *message.Initiator
	}
	return actor, actor.Type == models.ConversationParticipantUser && actor.Validate() == nil
}

func conversationAgentRunRequest(conversation *models.Conversation, message *models.ChannelMessage) models.CreateAgentRunRequest {
	goal := teamRoutingRunGoal
	assignedAgentID := ""
	visibility := models.ActivityVisibilityTeam
	if conversation.Owner.Type == models.OwnerTypeAgent {
		goal = "Respond to an Agent channel message"
		assignedAgentID = conversation.Owner.ID
		visibility = models.ActivityVisibilityPrivate
	}
	request := models.CreateAgentRunRequest{
		Scope: conversation.Scope, Kind: models.RunKindConversation, Owner: conversation.Owner,
		AssignedAgentID: assignedAgentID, ActingUser: models.MessageActingUser(conversation, message),
		ConcurrencyKey: models.ConversationRunLane(conversation.ID, "", assignedAgentID), Goal: goal, Source: models.RunSourceChat,
		Context: map[string]interface{}{
			models.ConversationRunConversationIDContextKey: conversation.ID,
			models.ConversationRunTriggerIDContextKey:      message.ID,
			conversationRunContextTriggerSeq:               message.Sequence,
		},
		IdempotencyKey: conversationRunIdempotencyKey(conversation.Scope, conversation.ID, message.ID),
		Actor:          models.ActivityActor{Type: "service", ID: models.ConversationRunSchedulerParticipant},
		Visibility:     visibility,
	}
	if root := models.ExternalConversationThreadRoot(conversation, message); root != "" {
		request.Context["threadRootMessageId"] = root
		request.ConcurrencyKey = models.ConversationRunLane(conversation.ID, root, assignedAgentID)
	}
	if message.ResponseMode != "" {
		request.Context["responseMode"] = message.ResponseMode
	}
	if models.VoiceCallStartedMessage(message) {
		request.Context["voiceCallStarted"] = true
	}
	if assignedAgentID != "" {
		request.SessionID = models.ConversationSessionID(conversation.Scope, request.ConcurrencyKey)
	}
	return request
}

func conversationRunIdempotencyKey(scope models.Scope, conversationID, messageID string) string {
	return "conversation-run:" + models.SHA256DigestString(scope.Kind+"\x00"+scope.ID+"\x00"+conversationID+"\x00"+messageID)
}

// ConversationPlace is a place find_place found in a chat, as the Agent
// sees it in its turn context.
type ConversationPlace struct {
	Place string  `json:"place"`
	Name  string  `json:"name"`
	Lat   float64 `json:"lat"`
	Lon   float64 `json:"lon"`
}

// MaximumConversationPlaces bounds the places a turn context lists.
const MaximumConversationPlaces = 20

type agentConversationActiveRun struct {
	ID                string                `json:"id"`
	RootRunID         string                `json:"rootRunId"`
	Entrypoint        string                `json:"entrypoint,omitempty"`
	Status            models.AgentRunStatus `json:"status"`
	Goal              string                `json:"goal"`
	Source            models.RunSource      `json:"source"`
	Revision          int64                 `json:"revision"`
	AvailableControls []string              `json:"availableControls,omitempty"`
}

// agentConversationAssignment is the work a teammate handed over in the chat.
type agentConversationAssignment struct {
	From string `json:"fromAgentId,omitempty"`
	Kind string `json:"kind"`
	Work string `json:"work"`
}

func conversationRunAssignment(run *models.AgentRun) *agentConversationAssignment {
	if run == nil || run.Source != models.RunSourceRequest && run.Source != models.RunSourceHandoff && run.Source != models.RunSourceEscalation || strings.TrimSpace(run.Goal) == "" {
		return nil
	}
	from, _ := run.Context[collaborationHandedOffByContextKey].(string)
	return &agentConversationAssignment{From: from, Kind: string(run.Source), Work: run.Goal}
}

// collaborationHandedOffByContextKey records the teammate that handed work over in a chat.
const collaborationHandedOffByContextKey = "handedOffBy"
