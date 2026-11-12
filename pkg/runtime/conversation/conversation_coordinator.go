package conversation

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
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
	Participant   models.ConversationParticipant `json:"participant"`
	SemanticRoles []string                       `json:"semanticRoles,omitempty"`
}

func (b ConversationParticipantBinding) Validate() error {
	if err := b.Participant.Validate(); err != nil {
		return err
	}
	if b.Participant.Type != models.ConversationParticipantAgent {
		return fmt.Errorf("%w: only Agent participants can be coordinated", models.ErrInvalidConversation)
	}
	seen := make(map[string]struct{}, len(b.SemanticRoles))
	for _, role := range b.SemanticRoles {
		role = strings.TrimSpace(role)
		if role == "" || len(role) > 80 {
			return fmt.Errorf("%w: semantic roles must be 1-80 characters", models.ErrInvalidConversation)
		}
		if _, exists := seen[role]; exists {
			return fmt.Errorf("%w: duplicate semantic role %q", models.ErrInvalidConversation, role)
		}
		seen[role] = struct{}{}
	}
	return nil
}

type ConversationParticipantQuery struct {
	Conversation *models.Conversation
	Trigger      *models.ChannelMessage
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
	Scope            models.Scope
	ConversationID   string
	TriggerMessageID string
	IdempotencyKey   string
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
		return ConversationCoordinatorConfig{}, fmt.Errorf("%w: invalid conversation coordinator configuration", models.ErrInvalidConversation)
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
func (c *ConversationCoordinator) Coordinate(ctx context.Context, req ConversationRoutingRequest) (*models.ParticipationRoundResult, error) {
	return c.CoordinateWithChildRuns(ctx, req, nil)
}

// CoordinateWithChildRuns is Coordinate for a Team channel's routing Run.
// childRunKey derives each responder's child Run idempotency key, so the
// round records the Run id before the Run exists.
func (c *ConversationCoordinator) CoordinateWithChildRuns(ctx context.Context, req ConversationRoutingRequest, childRunKey func(models.ConversationParticipant) string) (*models.ParticipationRoundResult, error) {
	if c == nil || c.conversations == nil || c.participants == nil || c.router == nil {
		return nil, ErrConversationCoordinationUnavailable
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	triggerID := strings.TrimSpace(req.TriggerMessageID)
	if key == "" || !models.IsValidOpaqueIdentifier(strings.TrimSpace(req.ConversationID), 128) || !models.IsValidOpaqueIdentifier(triggerID, 128) {
		return nil, fmt.Errorf("%w: conversation, trigger, and idempotency key are required", models.ErrInvalidConversation)
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if existing, err := c.conversations.FindParticipationRoundByIdempotencyKey(ctx, req.Scope, req.ConversationID, key); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.Round.TriggerMessageID != triggerID {
			return nil, models.ErrMessageConflict
		}
		existing.Replayed = true
		return existing, nil
	}
	conversation, err := c.conversations.GetConversation(ctx, req.Scope, req.ConversationID)
	if err != nil {
		return nil, err
	}
	if conversation.Status != models.ConversationStatusActive {
		return nil, models.ErrInvalidConversation
	}
	trigger, err := c.conversations.GetChannelMessage(ctx, req.Scope, conversation.ID, triggerID)
	if err != nil {
		return nil, err
	}
	bindings, err := c.participants.ResolveConversationParticipants(ctx, ConversationParticipantQuery{
		Conversation: models.CloneConversation(conversation), Trigger: models.CloneChannelMessage(trigger),
	})
	if err != nil {
		return nil, fmt.Errorf("resolve conversation participants: %w", err)
	}
	if len(bindings) == 0 {
		return nil, ErrNoConversationParticipants
	}
	if len(bindings) > c.config.MaximumParticipants {
		return nil, fmt.Errorf("%w: participant count exceeds configured maximum", models.ErrInvalidConversation)
	}
	seen := make(map[models.ConversationParticipant]bool, len(bindings))
	for _, binding := range bindings {
		if err := binding.Validate(); err != nil {
			return nil, err
		}
		if seen[binding.Participant] {
			return nil, fmt.Errorf("%w: duplicate conversation participant", models.ErrInvalidConversation)
		}
		seen[binding.Participant] = true
	}
	bindings = append([]ConversationParticipantBinding(nil), bindings...)
	sort.SliceStable(bindings, func(i, j int) bool { return bindings[i].Participant.ID < bindings[j].Participant.ID })

	responders, err := c.addressedResponders(ctx, conversation, trigger, bindings)
	if err != nil {
		return nil, err
	}
	if len(responders) == 0 && trigger.Sender.Type != models.ConversationParticipantAgent {
		routed, err := c.route(ctx, conversation, trigger, bindings)
		if err != nil {
			return nil, err
		}
		responders = []models.ConversationResponder{routed}
	}
	if childRunKey != nil {
		for index := range responders {
			responders[index].RunID = agentrun.RunIDForIdempotencyKey(req.Scope, childRunKey(responders[index].Participant))
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
		if !errors.Is(err, models.ErrRevisionConflict) {
			return result, err
		}
	}
	return nil, models.ErrRevisionConflict
}

// addressedResponders applies the addressing rules, which need no model call:
// @mentioned Agents, then @everyone, then the author of a quoted or
// replied-to Agent message. An Agent's own message (a handoff) is addressed
// only through its @mentions and never fans out to everyone.
func (c *ConversationCoordinator) addressedResponders(ctx context.Context, conversation *models.Conversation, trigger *models.ChannelMessage, bindings []ConversationParticipantBinding) ([]models.ConversationResponder, error) {
	members := make(map[models.ConversationParticipant]ConversationParticipantBinding, len(bindings))
	for _, binding := range bindings {
		members[binding.Participant] = binding
	}
	responders := make([]models.ConversationResponder, 0)
	added := make(map[models.ConversationParticipant]bool)
	add := func(participant models.ConversationParticipant, addressed models.ConversationAddressing) {
		if _, member := members[participant]; !member || added[participant] || participant == trigger.Sender {
			return
		}
		added[participant] = true
		responders = append(responders, models.ConversationResponder{Participant: participant, Addressed: addressed})
	}
	for _, mention := range trigger.Mentions {
		add(mention, models.ConversationAddressedMention)
	}
	if trigger.Audience.Kind == models.ConversationAudienceParticipants {
		for _, participant := range trigger.Audience.Participants {
			add(participant, models.ConversationAddressedMention)
		}
	}
	if trigger.Sender.Type == models.ConversationParticipantAgent {
		return responders, nil
	}
	if len(responders) > 0 {
		return responders, nil
	}
	if trigger.Audience.Kind == models.ConversationAudienceEveryone {
		for _, binding := range bindings {
			add(binding.Participant, models.ConversationAddressedEveryone)
		}
		return responders, nil
	}
	for _, id := range []string{QuotedMessageID(trigger.Quote), trigger.ReplyToMessageID} {
		if id == "" {
			continue
		}
		referenced, err := c.conversations.GetChannelMessage(ctx, conversation.Scope, conversation.ID, id)
		if errors.Is(err, models.ErrChannelMessageNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if referenced.Sender.Type == models.ConversationParticipantAgent {
			add(referenced.Sender, models.ConversationAddressedReply)
			if len(responders) > 0 {
				return responders, nil
			}
		}
	}
	if trigger.Audience.Kind == models.ConversationAudienceRoles {
		for _, binding := range bindings {
			for _, role := range binding.SemanticRoles {
				for _, addressed := range trigger.Audience.Roles {
					if strings.EqualFold(strings.TrimSpace(role), strings.TrimSpace(addressed)) {
						add(binding.Participant, models.ConversationAddressedRole)
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
func (c *ConversationCoordinator) route(ctx context.Context, conversation *models.Conversation, trigger *models.ChannelMessage, bindings []ConversationParticipantBinding) (models.ConversationResponder, error) {
	roster := LoadConversationTeamRoster(ctx, c.conversations.store, conversation)
	request := ConversationRouteRequest{
		Scope: conversation.Scope, ConversationID: conversation.ID, TeamID: conversation.Owner.ID,
		RoutingNotes: roster.notes, Trigger: *models.CloneChannelMessage(trigger),
	}
	busy, err := teamBusyMembers(ctx, c.conversations.store, conversation)
	if err != nil {
		return models.ConversationResponder{}, err
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
	viewer := models.ConversationViewer{Participant: trigger.Sender}
	recent, err := c.conversations.ListChannelMessages(ctx, models.ChannelMessageFilter{
		Scope: conversation.Scope, ConversationID: conversation.ID, BeforeSequence: trigger.Sequence,
		Limit: 50, Descending: true, Viewer: &viewer,
	})
	if err != nil {
		return models.ConversationResponder{}, err
	}
	ReverseChannelMessages(recent)
	request.LastAgentWithUser = lastAgentWithUser(recent, trigger.Sender, request.Members)
	if len(recent) > c.config.RecentMessageLimit {
		recent = recent[len(recent)-c.config.RecentMessageLimit:]
	}
	for _, message := range recent {
		request.Recent = append(request.Recent, *models.CloneChannelMessage(message))
	}
	routeCtx, cancel := context.WithTimeout(ctx, c.config.RouteTimeout)
	decision, routeErr := c.router.Route(routeCtx, request)
	cancel()
	if routeErr == nil {
		for _, member := range request.Members {
			if member.AgentID == strings.TrimSpace(decision.AgentID) {
				return models.ConversationResponder{
					Participant: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: member.AgentID},
					Addressed:   models.ConversationAddressedRouted, Reason: routingReason(decision.Reason, "best fit for this message"),
				}, nil
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return models.ConversationResponder{}, err
	}
	return fallbackConversationResponder(request), nil
}

func fallbackConversationResponder(request ConversationRouteRequest) models.ConversationResponder {
	pick := func(id, reason string) models.ConversationResponder {
		return models.ConversationResponder{Participant: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: id}, Addressed: models.ConversationAddressedRouted, Reason: reason}
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
	for len(reason) > models.MaximumRoutingReasonBytes {
		reason = models.TruncateRunes(reason, len([]rune(reason))-1)
	}
	return reason
}

// lastAgentWithUser returns the newest member answer to one of user's
// messages; members is the eligible roster.
func lastAgentWithUser(messages []*models.ChannelMessage, user models.ConversationParticipant, members []RouteMember) string {
	eligible := make(map[string]bool, len(members))
	for _, member := range members {
		eligible[member.AgentID] = true
	}
	senders := make(map[string]models.ConversationParticipant, len(messages))
	for _, message := range messages {
		senders[message.ID] = message.Sender
	}
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		if message.Sender.Type != models.ConversationParticipantAgent || !eligible[message.Sender.ID] {
			continue
		}
		for _, id := range []string{message.ResolvesMessageID, message.ReplyToMessageID, QuotedMessageID(message.Quote)} {
			if id != "" && senders[id] == user {
				return message.Sender.ID
			}
		}
	}
	return ""
}

type conversationTeamMember struct {
	name, role string
	owns       []string
}

type ConversationTeamRoster struct {
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

// LoadConversationTeamRoster reads the Team's routing notes, lead and member
// names, purposes and owned topics from the kernel store. It is best effort: a
// store without Team records yields an empty roster, never an error, because
// membership itself comes from the host's participant source.
func LoadConversationTeamRoster(ctx context.Context, store models.ConversationStore, conversation *models.Conversation) ConversationTeamRoster {
	roster := ConversationTeamRoster{members: map[string]conversationTeamMember{}}
	teams, ok := store.(conversationTeamCatalog)
	if !ok || conversation == nil || conversation.Owner.Type != models.OwnerTypeTeam {
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
	agents, _ := store.(conversationAgentCatalog)
	for _, assignment := range deployment.Roster {
		member := conversationTeamMember{name: strings.TrimSpace(assignment.DisplayName), role: strings.TrimSpace(assignment.Purpose), owns: append([]string(nil), assignment.Owns...)}
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
		if roster.lead == "" && assignment.Lead {
			roster.lead = assignment.AgentDeploymentID
		}
	}
	return roster
}

// activeConversationRunStatuses are the non-terminal Run statuses.
var activeConversationRunStatuses = []models.AgentRunStatus{models.AgentRunStatusQueued, models.AgentRunStatusPlanning, models.AgentRunStatusRunning, models.AgentRunStatusPaused, models.AgentRunStatusSleeping,
	models.AgentRunStatusWaitingForDependency, models.AgentRunStatusWaitingForAgent, models.AgentRunStatusWaitingForApproval, models.AgentRunStatusWaitingForEvent}

// teamBusyMembers maps each member with an active Run of this Team (an answer
// or a task) to a short label of that work.
func teamBusyMembers(ctx context.Context, store models.ConversationStore, conversation *models.Conversation) (map[string]string, error) {
	busy := make(map[string]string)
	runs, ok := store.(models.PortfolioStore)
	if !ok {
		return busy, nil
	}
	const pageSize = 200
	for offset := 0; ; offset += pageSize {
		page, err := runs.ListAgentRuns(ctx, models.AgentRunFilter{
			Scope: conversation.Scope, Owner: &conversation.Owner,
			Statuses: activeConversationRunStatuses,
			Limit:    pageSize, Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		for _, run := range page {
			if run == nil || run.AssignedAgentID == "" || models.IsTerminalAgentRunStatus(run.Status) {
				continue
			}
			if _, exists := busy[run.AssignedAgentID]; !exists {
				label := run.Goal
				if run.Kind == models.RunKindConversation {
					label = "answering in the Team channel"
				}
				busy[run.AssignedAgentID] = models.TruncateRunes(label, 80)
			}
		}
		if len(page) < pageSize {
			return busy, nil
		}
	}
}

func (c *ConversationCoordinator) MarkObserved(ctx context.Context, conversation *models.Conversation, participant models.ConversationParticipant, sequence int64) error {
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
		if !errors.Is(err, models.ErrConversationCursorConflict) {
			return err
		}
	}
	return models.ErrConversationCursorConflict
}

func mergeConversationReferences(left, right []models.ConversationReference) []models.ConversationReference {
	merged := make([]models.ConversationReference, 0, len(left)+len(right))
	seen := make(map[models.ConversationReference]struct{}, len(left)+len(right))
	for _, references := range [][]models.ConversationReference{left, right} {
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

func latestConversationSequence(messages []*models.ChannelMessage) int64 {
	var latest int64
	for _, message := range messages {
		if message != nil && message.Sequence > latest {
			latest = message.Sequence
		}
	}
	return latest
}

func ReverseChannelMessages(messages []*models.ChannelMessage) {
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
}

// ResolveParticipants lists the participants who may answer in query's
// conversation.
func (c *ConversationCoordinator) ResolveParticipants(ctx context.Context, query ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
	return c.participants.ResolveConversationParticipants(ctx, query)
}
