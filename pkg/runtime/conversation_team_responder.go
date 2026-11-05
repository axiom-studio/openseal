package runtime

import (
	"context"
	"errors"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// teamResponderContextKey marks a Team member's child agent_work Run that
	// answers one channel message. Its turn is the ordinary Agent turn.
	teamResponderContextKey = "teamResponder"
	// routingReasonContextKey carries the router's reason onto the answer.
	routingReasonContextKey = "routingReason"
	teamResponderGoalPrefix = "Answer in the Team channel: "
)

// teamChannelRules are the Team-only rules a member gets with its roster.
const teamChannelRules = "You are one member of this Team channel and answer under your own name. " +
	"Answer the current message yourself; do not speak for teammates. " +
	"To bring a teammate in, end your answer with a one-line @Name mention and the exact ask; that teammate is then asked to answer. " +
	"Never @mention teammates just to agree, thank or acknowledge. " +
	"Prefer a reaction (👀 picked up, ✅ done, 👍 agree) over a filler message."

// isTeamResponderRun reports whether run is a Team member's child Run that
// answers a Team channel message.
func isTeamResponderRun(run *AgentRun) bool {
	if run == nil || run.Kind != RunKindAgentWork || run.Owner.Type != OwnerTypeTeam ||
		strings.TrimSpace(run.ParentRunID) == "" || strings.TrimSpace(run.AssignedAgentID) == "" {
		return false
	}
	marked, _ := run.Context[teamResponderContextKey].(bool)
	return marked
}

func teamResponderConcurrencyKey(conversationKey, agentID string) string {
	return conversationKey + ":member:" + agentID
}

func conversationRoutingKey(scope Scope, conversationID, triggerID string) string {
	return "participation-round:" + hashString(scope.Kind+"\x00"+scope.ID+"\x00"+conversationID+"\x00"+triggerID)
}

// TeamResponderTurnResolver sends Team members' child Runs to the conversation
// runner, so they answer through the same Agent turn as a 1:1 chat and post
// their own answer; every other Run goes to next. Hosts wrap their agent_work
// worker resolver with it.
func TeamResponderTurnResolver(conversations *ConversationRunTurnRunner, next TurnRunnerResolver) TurnRunnerResolver {
	return TurnRunnerResolverFunc(func(ctx context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
		if conversations != nil && isTeamResponderRun(run) {
			return conversations.ResolveTurnRunner(ctx, run)
		}
		if next == nil {
			return nil, ErrTurnHostUnavailable
		}
		return next.ResolveTurnRunner(ctx, run)
	})
}

// routeTeamMessage is a Team channel's conversation Run: it records who answers
// the message and starts one child agent_work Run per responder. It writes no
// answer itself.
func (r *ConversationRunTurnRunner) routeTeamMessage(ctx context.Context, run *AgentRun, conversation *Conversation, triggerID string) (*TurnOutcome, error) {
	if r.runs == nil || r.coordinator == nil {
		return nil, ErrConversationCoordinationUnavailable
	}
	trigger, err := r.conversations.GetChannelMessage(ctx, run.Scope, conversation.ID, triggerID)
	if err != nil {
		return nil, err
	}
	childKey := func(participant ConversationParticipant) string {
		return "team-responder:" + hashString(run.Scope.Kind+"\x00"+run.Scope.ID+"\x00"+run.ID+"\x00"+participant.ID)
	}
	result, err := r.coordinator.Coordinate(ctx, ConversationRoutingRequest{
		Scope: run.Scope, ConversationID: conversation.ID, TriggerMessageID: triggerID,
		IdempotencyKey: conversationRoutingKey(run.Scope, conversation.ID, triggerID), childRunKey: childKey,
	})
	if err != nil {
		if ctx.Err() != nil || permanentConversationRunError(err) {
			return nil, err
		}
		return r.retryOutcome(run, err)
	}
	runIDs := make([]interface{}, 0, len(result.Round.Responders))
	for _, responder := range result.Round.Responders {
		created, err := r.runs.CreateAgentRun(ctx, teamResponderRunRequest(run, trigger, responder, childKey(responder.Participant)))
		if err != nil {
			return nil, err
		}
		runIDs = append(runIDs, created.Run.ID)
	}
	return &TurnOutcome{
		NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Team message routed",
		RunOutput: map[string]interface{}{
			"conversationId": conversation.ID, "triggerMessageId": triggerID,
			"participationRoundId": result.Round.ID, "responderRunIds": runIDs, "replayed": result.Replayed,
		},
	}, nil
}

func teamResponderRunRequest(parent *AgentRun, trigger *ChannelMessage, responder ConversationResponder, key string) CreateAgentRunRequest {
	context := map[string]interface{}{
		conversationRunContextConversationID: parent.Context[conversationRunContextConversationID],
		conversationRunContextTriggerID:      trigger.ID,
		conversationRunContextTriggerSeq:     trigger.Sequence,
		teamResponderContextKey:              true,
	}
	for _, copied := range []string{"threadRootMessageId", "responseMode", "voiceCallStarted", RunContextSessionKey} {
		if value, ok := parent.Context[copied]; ok {
			context[copied] = value
		}
	}
	if responder.Reason != "" {
		context[routingReasonContextKey] = responder.Reason
	}
	return CreateAgentRunRequest{
		Scope: parent.Scope, Kind: RunKindAgentWork, ParentRunID: parent.ID, Owner: parent.Owner,
		AssignedAgentID: responder.Participant.ID,
		ConcurrencyKey:  teamResponderConcurrencyKey(parent.ConcurrencyKey, responder.Participant.ID),
		Goal:            teamResponderGoalPrefix + truncateRunes(strings.Join(strings.Fields(trigger.Content), " "), 120),
		Source:          RunSourceChat, Priority: parent.Priority, Context: context, Budget: cloneBudgetPolicy(parent.Budget),
		IdempotencyKey: key, Actor: ActivityActor{Type: "service", ID: conversationRunSchedulerParticipant},
		Visibility: ActivityVisibilityTeam,
	}
}

// agentConversationTeam is the short Team context a member's turn gets.
type agentConversationTeam struct {
	Notes         string                        `json:"routingNotes,omitempty"`
	RoutingReason string                        `json:"routingReason,omitempty"`
	Roster        []agentConversationTeamMember `json:"roster"`
	Rules         string                        `json:"rules"`
}

type agentConversationTeamMember struct {
	AgentID string   `json:"agentId"`
	Name    string   `json:"name"`
	Role    string   `json:"role,omitempty"`
	Owns    []string `json:"owns,omitempty"`
	Lead    bool     `json:"lead,omitempty"`
	You     bool     `json:"you,omitempty"`
}

// teamMembers lists the members who may answer in the channel, with the
// names an @mention uses.
func (r *ConversationRunTurnRunner) teamMembers(ctx context.Context, conversation *Conversation, trigger *ChannelMessage) (conversationTeamRoster, []ConversationParticipantBinding, error) {
	roster := loadConversationTeamRoster(ctx, r.conversations.store, conversation)
	if r.coordinator == nil {
		return roster, nil, nil
	}
	bindings, err := r.coordinator.participants.ResolveConversationParticipants(ctx, ConversationParticipantQuery{
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
	reason, _ := run.Context[routingReasonContextKey].(string)
	team := &agentConversationTeam{Notes: roster.notes, RoutingReason: reason, Rules: teamChannelRules, Roster: make([]agentConversationTeamMember, 0, len(bindings))}
	for _, binding := range bindings {
		info := roster.members[binding.Participant.ID]
		member := agentConversationTeamMember{
			AgentID: binding.Participant.ID, Name: teamMemberName(info, binding), Role: info.role, Owns: info.owns,
			Lead: binding.Participant.ID == roster.lead, You: binding.Participant.ID == run.AssignedAgentID,
		}
		team.Roster = append(team.Roster, member)
	}
	return team, nil
}

func teamMemberName(info conversationTeamMember, binding ConversationParticipantBinding) string {
	if info.name != "" {
		return info.name
	}
	return binding.Participant.ID
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

func findHandoffMentions(content, sender string, roster conversationTeamRoster, bindings []ConversationParticipantBinding) []ConversationParticipant {
	lower := strings.ToLower(content)
	type found struct {
		at          int
		participant ConversationParticipant
	}
	mentions := make([]found, 0)
	for _, binding := range bindings {
		if binding.Participant.ID == sender {
			continue
		}
		for _, name := range []string{teamMemberName(roster.members[binding.Participant.ID], binding), binding.Participant.ID} {
			if at := mentionIndex(lower, "@"+strings.ToLower(name)); at >= 0 {
				mentions = append(mentions, found{at: at, participant: binding.Participant})
				break
			}
		}
	}
	sort.SliceStable(mentions, func(i, j int) bool { return mentions[i].at < mentions[j].at })
	result := make([]ConversationParticipant, 0, len(mentions))
	for _, mention := range mentions {
		result = append(result, mention.participant)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// mentionIndex finds token in text where it is not followed by a letter or
// digit, so "@rio" does not match "@riot".
func mentionIndex(text, token string) int {
	for offset := 0; ; {
		at := strings.Index(text[offset:], token)
		if at < 0 {
			return -1
		}
		end := offset + at + len(token)
		next, _ := utf8.DecodeRuneInString(text[end:])
		if end == len(text) || !unicode.IsLetter(next) && !unicode.IsDigit(next) {
			return offset + at
		}
		offset = end
	}
}
