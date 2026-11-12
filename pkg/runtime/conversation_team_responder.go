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
	teamRoutingRunContextKey = "routingRunId"
	teamMemberRunGoal        = "Respond to a Team channel message"
	teamRoutingRunGoal       = "Route a Team channel message"
)

// teamChannelRules are the Team-only rules a member gets with its roster.
const teamChannelRules = "You are one member of this Team channel and answer under your own name. " +
	"Answer the current message yourself; do not speak for teammates. " +
	"To bring a teammate in, end your answer with a one-line @Name mention and the exact ask; that teammate is then asked to answer. " +
	"Never @mention teammates just to agree, thank or acknowledge. " +
	"Prefer a reaction (👀 picked up, ✅ done, 👍 agree) over a filler message."

// isTeamRoutingRun reports whether run is a Team's routing Run: a
// conversation Run with no assigned Agent, which starts member Runs.
func isTeamRoutingRun(run *AgentRun) bool {
	return run != nil && run.Kind == RunKindConversation && run.Owner.Type == OwnerTypeTeam &&
		strings.TrimSpace(run.AssignedAgentID) == ""
}

func conversationRoutingKey(scope Scope, conversationID, triggerID string) string {
	return "participation-round:" + hashString(scope.Kind+"\x00"+scope.ID+"\x00"+conversationID+"\x00"+triggerID)
}

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
		approvals: NewApprovalCoordinator(r.runStore, r.runStore, nil),
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
		existing, err := r.runStore.GetAgentRun(ctx, run.Scope, runIDForIdempotencyKey(run.Scope, key))
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

// teamMemberRunRequest is a member's answer Run: a root conversation Run in
// the member's own lane, shaped like a 1:1 Agent chat's Run.
func teamMemberRunRequest(routing *AgentRun, conversation *Conversation, trigger *ChannelMessage, responder ConversationResponder, key string) CreateAgentRunRequest {
	context := map[string]interface{}{
		conversationRunContextConversationID: conversation.ID,
		conversationRunContextTriggerID:      trigger.ID,
		conversationRunContextTriggerSeq:     trigger.Sequence,
		// The client's link from the routing Run to the answers it started.
		teamRoutingRunContextKey: routing.ID,
	}
	for _, copied := range []string{"threadRootMessageId", "responseMode", "voiceCallStarted", RunContextSessionKey} {
		if value, ok := routing.Context[copied]; ok {
			context[copied] = value
		}
	}
	if responder.Reason != "" {
		context[routingReasonContextKey] = responder.Reason
	}
	root, _ := routing.Context["threadRootMessageId"].(string)
	lane := ConversationRunLane(conversation.ID, root, responder.Participant.ID)
	return CreateAgentRunRequest{
		Scope: routing.Scope, Kind: RunKindConversation, Owner: routing.Owner,
		AssignedAgentID: responder.Participant.ID, ActingUser: routing.ActingUser,
		ConcurrencyKey: lane, SessionID: conversationSessionID(routing.Scope, lane),
		Goal: teamMemberRunGoal, Source: RunSourceChat, Priority: routing.Priority, Context: context,
		Budget: cloneBudgetPolicy(routing.Budget), IdempotencyKey: key,
		Actor:      ActivityActor{Type: "service", ID: conversationRunSchedulerParticipant},
		Visibility: ActivityVisibilityTeam,
	}
}

// agentConversationTeam is the short Team context a member's turn gets.
// The routing reason is a respond_to line of the run context and the Team
// rules are a system section (teamChannelRules).
type agentConversationTeam struct {
	Notes  string                        `json:"routingNotes,omitempty"`
	Roster []agentConversationTeamMember `json:"roster"`
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

// agentTeam is the team context of the Agent you: the routing notes and
// every member who may answer, by binding.
func (roster conversationTeamRoster) agentTeam(bindings []ConversationParticipantBinding, you string) *agentConversationTeam {
	team := &agentConversationTeam{Notes: roster.notes, Roster: make([]agentConversationTeamMember, 0, len(bindings))}
	for _, binding := range bindings {
		info := roster.members[binding.Participant.ID]
		member := agentConversationTeamMember{
			AgentID: binding.Participant.ID, Name: teamMemberName(info, binding), Role: info.role, Owns: info.owns,
			Lead: binding.Participant.ID == roster.lead, You: binding.Participant.ID == you,
		}
		team.Roster = append(team.Roster, member)
	}
	return team
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
