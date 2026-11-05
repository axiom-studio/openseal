package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

var (
	ErrConversationCoordinationUnavailable = errors.New("conversation coordination is not configured")
	ErrNoConversationParticipants          = errors.New("conversation has no eligible agent participants")
)

// ConversationParticipantBinding is one Team member that may answer in a
// channel. Hosts resolve authorization, membership, deployment health, and
// credentials before returning a binding; observers are left out. OpenSeal
// never transfers credentials between Agents.
type ConversationParticipantBinding struct {
	Participant   ConversationParticipant `json:"participant"`
	SemanticRoles []string                `json:"semanticRoles,omitempty"`
}

func (b ConversationParticipantBinding) Validate() error {
	if err := b.Participant.Validate(); err != nil {
		return err
	}
	if b.Participant.Type != ConversationParticipantAgent {
		return fmt.Errorf("%w: only Agent participants can be coordinated", ErrInvalidConversation)
	}
	seen := make(map[string]struct{}, len(b.SemanticRoles))
	for _, role := range b.SemanticRoles {
		role = strings.TrimSpace(role)
		if role == "" || len(role) > 80 {
			return fmt.Errorf("%w: semantic roles must be 1-80 characters", ErrInvalidConversation)
		}
		if _, exists := seen[role]; exists {
			return fmt.Errorf("%w: duplicate semantic role %q", ErrInvalidConversation, role)
		}
		seen[role] = struct{}{}
	}
	return nil
}

type ConversationParticipantQuery struct {
	Conversation *Conversation
	Trigger      *ChannelMessage
}

// ConversationParticipantSource is an embedding boundary. An enterprise host
// can resolve Team membership, RBAC, health, and policy; standalone OpenSeal can
// resolve local Agent deployments through the same contract.
type ConversationParticipantSource interface {
	ResolveConversationParticipants(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error)
}

type ConversationParticipantSourceFunc func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error)

func (f ConversationParticipantSourceFunc) ResolveConversationParticipants(ctx context.Context, query ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
	return f(ctx, query)
}

// ConversationRoutingRequest asks who answers one Team channel message.
type ConversationRoutingRequest struct {
	Scope            Scope
	ConversationID   string
	TriggerMessageID string
	IdempotencyKey   string
	// childRunKey derives each responder's child Run idempotency key, so the
	// round can record the Run id before the Run exists.
	childRunKey func(ConversationParticipant) string
}

type ConversationCoordinatorConfig struct {
	MaximumParticipants int
	// RecentMessageLimit bounds the history the router sees.
	RecentMessageLimit int
	RouteTimeout       time.Duration
}

func DefaultConversationCoordinatorConfig() ConversationCoordinatorConfig {
	return ConversationCoordinatorConfig{MaximumParticipants: 32, RecentMessageLimit: 10, RouteTimeout: 30 * time.Second}
}

func (c ConversationCoordinatorConfig) normalize() (ConversationCoordinatorConfig, error) {
	if c == (ConversationCoordinatorConfig{}) {
		return DefaultConversationCoordinatorConfig(), nil
	}
	if c.MaximumParticipants < 1 || c.MaximumParticipants > 256 || c.RecentMessageLimit < 1 || c.RecentMessageLimit > 100 ||
		c.RouteTimeout < time.Second || c.RouteTimeout > 5*time.Minute {
		return ConversationCoordinatorConfig{}, fmt.Errorf("%w: invalid conversation coordinator configuration", ErrInvalidConversation)
	}
	return c, nil
}

// ConversationCoordinator decides who answers a Team channel message. It never
// writes an answer: addressed members (mentions, @everyone, a quoted or
// replied-to Agent) answer directly; otherwise one router call picks a member.
type ConversationCoordinator struct {
	conversations *ConversationService
	participants  ConversationParticipantSource
	router        ConversationRouter
	config        ConversationCoordinatorConfig
}

func NewConversationCoordinator(
	conversations *ConversationService,
	participants ConversationParticipantSource,
	router ConversationRouter,
	config ConversationCoordinatorConfig,
) (*ConversationCoordinator, error) {
	if conversations == nil || participants == nil || router == nil {
		return nil, ErrConversationCoordinationUnavailable
	}
	normalized, err := config.normalize()
	if err != nil {
		return nil, err
	}
	return &ConversationCoordinator{conversations: conversations, participants: participants, router: router, config: normalized}, nil
}

// Coordinate commits the routing decision for a trigger once. A replay with the
// same idempotency key returns the saved decision without calling the router.
func (c *ConversationCoordinator) Coordinate(ctx context.Context, req ConversationRoutingRequest) (*ParticipationRoundResult, error) {
	if c == nil || c.conversations == nil || c.participants == nil || c.router == nil {
		return nil, ErrConversationCoordinationUnavailable
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	triggerID := strings.TrimSpace(req.TriggerMessageID)
	if key == "" || !validOpaqueIdentifier(strings.TrimSpace(req.ConversationID), 128) || !validOpaqueIdentifier(triggerID, 128) {
		return nil, fmt.Errorf("%w: conversation, trigger, and idempotency key are required", ErrInvalidConversation)
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if existing, err := c.conversations.FindParticipationRoundByIdempotencyKey(ctx, req.Scope, req.ConversationID, key); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.Round.TriggerMessageID != triggerID {
			return nil, ErrMessageConflict
		}
		existing.Replayed = true
		return existing, nil
	}
	conversation, err := c.conversations.GetConversation(ctx, req.Scope, req.ConversationID)
	if err != nil {
		return nil, err
	}
	if conversation.Status != ConversationStatusActive {
		return nil, ErrInvalidConversation
	}
	trigger, err := c.conversations.GetChannelMessage(ctx, req.Scope, conversation.ID, triggerID)
	if err != nil {
		return nil, err
	}
	bindings, err := c.participants.ResolveConversationParticipants(ctx, ConversationParticipantQuery{
		Conversation: cloneConversation(conversation), Trigger: cloneChannelMessage(trigger),
	})
	if err != nil {
		return nil, fmt.Errorf("resolve conversation participants: %w", err)
	}
	if len(bindings) == 0 {
		return nil, ErrNoConversationParticipants
	}
	if len(bindings) > c.config.MaximumParticipants {
		return nil, fmt.Errorf("%w: participant count exceeds configured maximum", ErrInvalidConversation)
	}
	seen := make(map[ConversationParticipant]bool, len(bindings))
	for _, binding := range bindings {
		if err := binding.Validate(); err != nil {
			return nil, err
		}
		if seen[binding.Participant] {
			return nil, fmt.Errorf("%w: duplicate conversation participant", ErrInvalidConversation)
		}
		seen[binding.Participant] = true
	}
	bindings = append([]ConversationParticipantBinding(nil), bindings...)
	sort.SliceStable(bindings, func(i, j int) bool { return bindings[i].Participant.ID < bindings[j].Participant.ID })

	responders, err := c.addressedResponders(ctx, conversation, trigger, bindings)
	if err != nil {
		return nil, err
	}
	if len(responders) == 0 && trigger.Sender.Type != ConversationParticipantAgent {
		routed, err := c.route(ctx, conversation, trigger, bindings)
		if err != nil {
			return nil, err
		}
		responders = []ConversationResponder{routed}
	}
	if req.childRunKey != nil {
		for index := range responders {
			responders[index].RunID = runIDForIdempotencyKey(req.Scope, req.childRunKey(responders[index].Participant))
		}
	}
	for range 3 {
		current, err := c.conversations.GetConversation(ctx, req.Scope, conversation.ID)
		if err != nil {
			return nil, err
		}
		result, err := c.conversations.CommitConversationRouting(ctx, CommitConversationRoutingRequest{
			Scope: req.Scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
			TriggerMessageID: triggerID, Responders: responders, IdempotencyKey: key,
		})
		if !errors.Is(err, ErrRevisionConflict) {
			return result, err
		}
	}
	return nil, ErrRevisionConflict
}

// addressedResponders applies the addressing rules, which need no model call:
// @mentioned Agents, then @everyone, then the author of a quoted or
// replied-to Agent message. An Agent's own message (a handoff) is addressed
// only through its @mentions and never fans out to everyone.
func (c *ConversationCoordinator) addressedResponders(ctx context.Context, conversation *Conversation, trigger *ChannelMessage, bindings []ConversationParticipantBinding) ([]ConversationResponder, error) {
	members := make(map[ConversationParticipant]ConversationParticipantBinding, len(bindings))
	for _, binding := range bindings {
		members[binding.Participant] = binding
	}
	responders := make([]ConversationResponder, 0)
	added := make(map[ConversationParticipant]bool)
	add := func(participant ConversationParticipant, addressed ConversationAddressing) {
		if _, member := members[participant]; !member || added[participant] || participant == trigger.Sender {
			return
		}
		added[participant] = true
		responders = append(responders, ConversationResponder{Participant: participant, Addressed: addressed})
	}
	for _, mention := range trigger.Mentions {
		add(mention, ConversationAddressedMention)
	}
	if trigger.Audience.Kind == ConversationAudienceParticipants {
		for _, participant := range trigger.Audience.Participants {
			add(participant, ConversationAddressedMention)
		}
	}
	if trigger.Sender.Type == ConversationParticipantAgent {
		return responders, nil
	}
	if len(responders) > 0 {
		return responders, nil
	}
	if trigger.Audience.Kind == ConversationAudienceEveryone {
		for _, binding := range bindings {
			add(binding.Participant, ConversationAddressedEveryone)
		}
		return responders, nil
	}
	// "Cove, can you…" names who answers just like @Cove does: a member named at the start of the
	// message is addressed, whatever the rest of it is about.
	if named, ok := namedAtStart(trigger.Content, loadConversationTeamRoster(ctx, c.conversations.store, conversation)); ok {
		add(ConversationParticipant{Type: ConversationParticipantAgent, ID: named}, ConversationAddressedMention)
		if len(responders) > 0 {
			return responders, nil
		}
	}
	for _, id := range []string{quotedMessageID(trigger.Quote), trigger.ReplyToMessageID} {
		if id == "" {
			continue
		}
		referenced, err := c.conversations.GetChannelMessage(ctx, conversation.Scope, conversation.ID, id)
		if errors.Is(err, ErrChannelMessageNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if referenced.Sender.Type == ConversationParticipantAgent {
			add(referenced.Sender, ConversationAddressedReply)
			if len(responders) > 0 {
				return responders, nil
			}
		}
	}
	if trigger.Audience.Kind == ConversationAudienceRoles {
		for _, binding := range bindings {
			for _, role := range binding.SemanticRoles {
				for _, addressed := range trigger.Audience.Roles {
					if strings.EqualFold(strings.TrimSpace(role), strings.TrimSpace(addressed)) {
						add(binding.Participant, ConversationAddressedRole)
					}
				}
			}
		}
	}
	return responders, nil
}

// route makes the one routing call and applies the fixed fallback order when
// it fails: the Agent last talking with this user, else the lead, else the
// first member who is not busy.
func (c *ConversationCoordinator) route(ctx context.Context, conversation *Conversation, trigger *ChannelMessage, bindings []ConversationParticipantBinding) (ConversationResponder, error) {
	roster := loadConversationTeamRoster(ctx, c.conversations.store, conversation)
	request := ConversationRouteRequest{
		Scope: conversation.Scope, ConversationID: conversation.ID, TeamID: conversation.Owner.ID,
		RoutingNotes: roster.notes, Trigger: *cloneChannelMessage(trigger),
	}
	busy, err := teamBusyMembers(ctx, c.conversations.store, conversation)
	if err != nil {
		return ConversationResponder{}, err
	}
	for _, binding := range bindings {
		id := binding.Participant.ID
		info := roster.members[id]
		member := RouteMember{AgentID: id, Name: info.name, Role: info.role, Owns: append([]string(nil), info.owns...)}
		if member.Name == "" {
			member.Name = id
		}
		if member.Role == "" && len(binding.SemanticRoles) > 0 {
			member.Role = binding.SemanticRoles[0]
		}
		if work, ok := busy[id]; ok {
			member.Busy, member.BusyWith = true, work
		}
		if id == roster.lead {
			request.Lead = id
		}
		request.Members = append(request.Members, member)
	}
	viewer := ConversationViewer{Participant: trigger.Sender}
	recent, err := c.conversations.ListChannelMessages(ctx, ChannelMessageFilter{
		Scope: conversation.Scope, ConversationID: conversation.ID, BeforeSequence: trigger.Sequence,
		Limit: 50, Descending: true, Viewer: &viewer,
	})
	if err != nil {
		return ConversationResponder{}, err
	}
	reverseChannelMessages(recent)
	request.LastAgentWithUser = lastAgentWithUser(recent, trigger.Sender, request.Members)
	if len(recent) > c.config.RecentMessageLimit {
		recent = recent[len(recent)-c.config.RecentMessageLimit:]
	}
	for _, message := range recent {
		request.Recent = append(request.Recent, *cloneChannelMessage(message))
	}
	routeCtx, cancel := context.WithTimeout(ctx, c.config.RouteTimeout)
	decision, routeErr := c.router.Route(routeCtx, request)
	cancel()
	if routeErr == nil {
		for _, member := range request.Members {
			if member.AgentID == strings.TrimSpace(decision.AgentID) {
				return ConversationResponder{
					Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: member.AgentID},
					Addressed:   ConversationAddressedRouted, Reason: routingReason(decision.Reason, "best fit for this message"),
				}, nil
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return ConversationResponder{}, err
	}
	return fallbackConversationResponder(request), nil
}

func fallbackConversationResponder(request ConversationRouteRequest) ConversationResponder {
	pick := func(id, reason string) ConversationResponder {
		return ConversationResponder{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: id}, Addressed: ConversationAddressedRouted, Reason: reason}
	}
	if request.LastAgentWithUser != "" {
		return pick(request.LastAgentWithUser, "was just talking with you")
	}
	if request.Lead != "" {
		return pick(request.Lead, "leads the team")
	}
	for _, member := range request.Members {
		if !member.Busy {
			return pick(member.AgentID, "is free")
		}
	}
	return pick(request.Members[0].AgentID, "first on the team")
}

// routingReason keeps the router's reason short and on one line.
func routingReason(reason, fallback string) string {
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" {
		reason = fallback
	}
	for len(reason) > MaximumRoutingReasonBytes {
		reason = truncateRunes(reason, len([]rune(reason))-1)
	}
	return reason
}

// lastAgentWithUser returns the newest member answer to one of user's
// messages; members is the eligible roster.
func lastAgentWithUser(messages []*ChannelMessage, user ConversationParticipant, members []RouteMember) string {
	eligible := make(map[string]bool, len(members))
	for _, member := range members {
		eligible[member.AgentID] = true
	}
	senders := make(map[string]ConversationParticipant, len(messages))
	for _, message := range messages {
		senders[message.ID] = message.Sender
	}
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message.Sender.Type != ConversationParticipantAgent || !eligible[message.Sender.ID] {
			continue
		}
		for _, id := range []string{message.ResolvesMessageID, message.ReplyToMessageID, quotedMessageID(message.Quote)} {
			if id != "" && senders[id] == user {
				return message.Sender.ID
			}
		}
	}
	return ""
}

// DefaultTeamLeadRoleID is the lead role when a Team names none.
const DefaultTeamLeadRoleID = "lead"

type conversationTeamMember struct {
	name, role string
	owns       []string
}

type conversationTeamRoster struct {
	notes   string
	lead    string
	members map[string]conversationTeamMember
}

type conversationTeamCatalog interface {
	GetTeamDeployment(context.Context, capability.ScopeReference, string) (*kernelteam.Deployment, error)
	GetTeamDefinition(context.Context, string, string) (*kernelteam.Definition, error)
}

type conversationAgentCatalog interface {
	GetDeployment(context.Context, capability.ScopeReference, string) (*kernelagent.AgentDeployment, error)
	GetDefinition(context.Context, string, string) (*kernelagent.AgentDefinition, error)
}

// loadConversationTeamRoster reads the Team's routing notes, lead and member
// names, roles and owned topics from the kernel store. It is best effort: a
// store without Team records yields an empty roster, never an error, because
// membership itself comes from the host's participant source.
func loadConversationTeamRoster(ctx context.Context, store ConversationStore, conversation *Conversation) conversationTeamRoster {
	roster := conversationTeamRoster{members: map[string]conversationTeamMember{}}
	teams, ok := store.(conversationTeamCatalog)
	if !ok || conversation == nil || conversation.Owner.Type != OwnerTypeTeam {
		return roster
	}
	scope := capability.ScopeReference{Kind: conversation.Scope.Kind, ID: conversation.Scope.ID}
	deployment, err := teams.GetTeamDeployment(ctx, scope, conversation.Owner.ID)
	if err != nil || deployment == nil {
		return roster
	}
	definition, err := teams.GetTeamDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil || definition == nil {
		return roster
	}
	roster.notes = strings.TrimSpace(definition.Routing.Notes)
	roles := make(map[string]string, len(definition.Roles))
	for _, role := range definition.Roles {
		roles[role.ID] = role.DisplayName
	}
	leadRole := definition.Routing.LeadRoleID
	if leadRole == "" {
		leadRole = DefaultTeamLeadRoleID
	}
	agents, _ := store.(conversationAgentCatalog)
	for _, assignment := range deployment.Roster {
		member := conversationTeamMember{name: strings.TrimSpace(assignment.DisplayName), role: roles[assignment.RoleID], owns: append([]string(nil), assignment.Owns...)}
		if member.name == "" && agents != nil {
			if agent, err := agents.GetDeployment(ctx, scope, assignment.AgentDeploymentID); err == nil && agent != nil {
				member.name = strings.TrimSpace(agent.DisplayName)
				if member.name == "" {
					if definition, err := agents.GetDefinition(ctx, agent.DefinitionID, agent.ActiveVersion); err == nil && definition != nil {
						member.name = strings.TrimSpace(definition.DisplayName)
					}
				}
			}
		}
		roster.members[assignment.AgentDeploymentID] = member
		if roster.lead == "" && assignment.RoleID == leadRole {
			roster.lead = assignment.AgentDeploymentID
		}
	}
	return roster
}

// activeConversationRunStatuses are the non-terminal Run statuses.
var activeConversationRunStatuses = []AgentRunStatus{AgentRunStatusQueued, AgentRunStatusPlanning, AgentRunStatusRunning, AgentRunStatusPaused, AgentRunStatusSleeping,
	AgentRunStatusWaitingForDependency, AgentRunStatusWaitingForAgent, AgentRunStatusWaitingForApproval, AgentRunStatusWaitingForEvent}

// teamBusyMembers maps each member with an active Run of this Team (an answer
// or a task) to a short label of that work.
func teamBusyMembers(ctx context.Context, store ConversationStore, conversation *Conversation) (map[string]string, error) {
	busy := make(map[string]string)
	runs, ok := store.(PortfolioStore)
	if !ok {
		return busy, nil
	}
	const pageSize = 200
	for offset := 0; ; offset += pageSize {
		page, err := runs.ListAgentRuns(ctx, AgentRunFilter{
			Scope: conversation.Scope, Owner: &conversation.Owner,
			Statuses: activeConversationRunStatuses,
			Limit:    pageSize, Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		for _, run := range page {
			if run == nil || run.AssignedAgentID == "" || isTerminalAgentRunStatus(run.Status) {
				continue
			}
			if _, exists := busy[run.AssignedAgentID]; !exists {
				label := run.Goal
				if run.Kind == RunKindConversation {
					label = "answering in the Team channel"
				}
				busy[run.AssignedAgentID] = truncateRunes(label, 80)
			}
		}
		if len(page) < pageSize {
			return busy, nil
		}
	}
}

func (c *ConversationCoordinator) markObserved(ctx context.Context, conversation *Conversation, participant ConversationParticipant, sequence int64) error {
	if sequence <= 0 {
		return nil
	}
	for range 2 {
		cursor, err := c.conversations.GetCursor(ctx, conversation.Scope, conversation.ID, participant)
		if err != nil {
			return err
		}
		if cursor != nil && cursor.ReadSequence >= sequence && cursor.DeliveredSequence >= sequence {
			return nil
		}
		expectedRevision := int64(0)
		if cursor != nil {
			expectedRevision = cursor.Revision
		}
		_, _, err = c.conversations.AdvanceCursor(ctx, AdvanceConversationCursorRequest{
			Scope: conversation.Scope, ConversationID: conversation.ID, Participant: participant,
			ExpectedRevision: expectedRevision, DeliveredSequence: sequence, ReadSequence: sequence,
		})
		if !errors.Is(err, ErrConversationCursorConflict) {
			return err
		}
	}
	return ErrConversationCursorConflict
}

func mergeConversationReferences(left, right []ConversationReference) []ConversationReference {
	merged := make([]ConversationReference, 0, len(left)+len(right))
	seen := make(map[ConversationReference]struct{}, len(left)+len(right))
	for _, references := range [][]ConversationReference{left, right} {
		for _, reference := range references {
			if _, exists := seen[reference]; exists {
				continue
			}
			seen[reference] = struct{}{}
			merged = append(merged, reference)
		}
	}
	return merged
}

func latestConversationSequence(messages []*ChannelMessage) int64 {
	var latest int64
	for _, message := range messages {
		if message != nil && message.Sequence > latest {
			latest = message.Sequence
		}
	}
	return latest
}

func reverseChannelMessages(messages []*ChannelMessage) {
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
}

// namedAtStart finds a Team member addressed by name at the start of a message
// ("Cove, …", "hey Miso can you …", "@Cove …"). Only a whole-word name counts.
func namedAtStart(content string, roster conversationTeamRoster) (string, bool) {
	text := strings.ToLower(strings.TrimSpace(content))
	for _, lead := range []string{"hey ", "hi ", "hello ", "ok ", "okay ", "so ", "and ", "@"} {
		if strings.HasPrefix(text, lead) {
			text = strings.TrimSpace(strings.TrimPrefix(text, lead))
			break
		}
	}
	best, bestLen := "", 0
	for id, member := range roster.members {
		name := strings.ToLower(strings.TrimSpace(member.name))
		if name == "" || !strings.HasPrefix(text, name) || len(name) <= bestLen {
			continue
		}
		if rest := text[len(name):]; rest != "" {
			if r := rune(rest[0]); r == '_' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= 0x80 {
				continue
			}
		}
		best, bestLen = id, len(name)
	}
	return best, best != ""
}
