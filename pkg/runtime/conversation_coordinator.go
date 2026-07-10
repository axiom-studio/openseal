package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrConversationCoordinationUnavailable = errors.New("conversation coordination is not configured")
	ErrNoConversationParticipants          = errors.New("conversation has no eligible agent participants")
)

// ConversationParticipantBinding is the portable identity and semantic role
// information needed to invite an Agent into a participation round. Hosts
// resolve authorization, membership, deployment health, and credentials before
// returning a binding; OpenSeal never transfers credentials between Agents.
type ConversationParticipantBinding struct {
	Participant   ConversationParticipant `json:"participant"`
	SemanticRoles []string                `json:"semanticRoles,omitempty"`
	Priority      int                     `json:"priority,omitempty"`
}

func (b ConversationParticipantBinding) Validate() error {
	if err := b.Participant.Validate(); err != nil {
		return err
	}
	if b.Participant.Type != ConversationParticipantAgent {
		return fmt.Errorf("%w: only Agent participants can be coordinated", ErrInvalidConversation)
	}
	if b.Priority < -100 || b.Priority > 100 {
		return fmt.Errorf("%w: participant priority must be between -100 and 100", ErrInvalidConversation)
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

type ParticipationProposalContext struct {
	Conversation   *Conversation
	Trigger        *ChannelMessage
	RecentMessages []*ChannelMessage
	Participant    ConversationParticipant
	SemanticRoles  []string
	Priority       int
}

// ParticipationProposalProvider asks one Agent whether it has new,
// role-relevant information to add. The provider returns only a bounded,
// user-visible proposal and structured signals; hidden reasoning is never part
// of this contract. OpenSeal overwrites identity, roles, priority, and IDs so a
// provider cannot impersonate another participant or elevate its authority.
type ParticipationProposalProvider interface {
	ProposeParticipation(context.Context, ParticipationProposalContext) (ParticipationProposal, error)
}

type ParticipationProposalProviderFunc func(context.Context, ParticipationProposalContext) (ParticipationProposal, error)

func (f ParticipationProposalProviderFunc) ProposeParticipation(ctx context.Context, input ParticipationProposalContext) (ParticipationProposal, error) {
	return f(ctx, input)
}

type ConversationCoordinationRequest struct {
	Scope              Scope
	ConversationID     string
	ExpectedRevision   int64
	TriggerMessageID   string
	Policy             ConversationArbitrationPolicy
	MaximumConcurrency int
	IdempotencyKey     string
}

type ConversationCoordinatorConfig struct {
	MaximumParticipants int
	MaximumConcurrency  int
	RecentMessageLimit  int
	ProposalTimeout     time.Duration
	PresenceTTL         time.Duration
}

func DefaultConversationCoordinatorConfig() ConversationCoordinatorConfig {
	return ConversationCoordinatorConfig{
		MaximumParticipants: 32,
		MaximumConcurrency:  4,
		RecentMessageLimit:  50,
		ProposalTimeout:     2 * time.Minute,
		PresenceTTL:         2 * time.Minute,
	}
}

func (c ConversationCoordinatorConfig) normalize() (ConversationCoordinatorConfig, error) {
	if c == (ConversationCoordinatorConfig{}) {
		return DefaultConversationCoordinatorConfig(), nil
	}
	if c.MaximumParticipants < 1 || c.MaximumParticipants > 256 || c.MaximumConcurrency < 1 ||
		c.MaximumConcurrency > c.MaximumParticipants || c.RecentMessageLimit < 1 || c.RecentMessageLimit > 500 ||
		c.ProposalTimeout < time.Second || c.ProposalTimeout > 30*time.Minute ||
		c.PresenceTTL < 5*time.Second || c.PresenceTTL > 30*time.Minute {
		return ConversationCoordinatorConfig{}, fmt.Errorf("%w: invalid conversation coordinator configuration", ErrInvalidConversation)
	}
	return c, nil
}

// ConversationCoordinator turns a channel wake into one durable, audited
// participation round. Model calls happen concurrently behind a bounded host
// interface; arbitration and persistence remain deterministic and atomic.
type ConversationCoordinator struct {
	conversations *ConversationService
	participants  ConversationParticipantSource
	proposals     ParticipationProposalProvider
	config        ConversationCoordinatorConfig
}

func NewConversationCoordinator(
	conversations *ConversationService,
	participants ConversationParticipantSource,
	proposals ParticipationProposalProvider,
	config ConversationCoordinatorConfig,
) (*ConversationCoordinator, error) {
	if conversations == nil || participants == nil || proposals == nil {
		return nil, ErrConversationCoordinationUnavailable
	}
	normalized, err := config.normalize()
	if err != nil {
		return nil, err
	}
	return &ConversationCoordinator{conversations: conversations, participants: participants, proposals: proposals, config: normalized}, nil
}

func (c *ConversationCoordinator) Coordinate(ctx context.Context, req ConversationCoordinationRequest) (*ParticipationRoundResult, error) {
	if c == nil || c.conversations == nil || c.participants == nil || c.proposals == nil {
		return nil, ErrConversationCoordinationUnavailable
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" || req.ExpectedRevision <= 0 || !validOpaqueIdentifier(strings.TrimSpace(req.ConversationID), 128) {
		return nil, fmt.Errorf("%w: conversation, positive revision, and idempotency key are required", ErrInvalidConversation)
	}
	if req.MaximumConcurrency < 0 || req.MaximumConcurrency > c.config.MaximumConcurrency {
		return nil, fmt.Errorf("%w: round concurrency exceeds the configured coordinator maximum", ErrInvalidConversation)
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	policy, err := req.Policy.normalize()
	if err != nil {
		return nil, err
	}
	if existing, err := c.conversations.FindParticipationRoundByIdempotencyKey(ctx, req.Scope, req.ConversationID, key); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.Round.TriggerMessageID != strings.TrimSpace(req.TriggerMessageID) || existing.Round.Policy != policy {
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
	if conversation.Revision != req.ExpectedRevision {
		return nil, ErrRevisionConflict
	}

	var trigger *ChannelMessage
	if triggerID := strings.TrimSpace(req.TriggerMessageID); triggerID != "" {
		trigger, err = c.conversations.GetChannelMessage(ctx, req.Scope, conversation.ID, triggerID)
		if err != nil {
			return nil, err
		}
	}
	recent, err := c.conversations.ListChannelMessages(ctx, ChannelMessageFilter{
		Scope: req.Scope, ConversationID: conversation.ID, Limit: c.config.RecentMessageLimit, Descending: true,
	})
	if err != nil {
		return nil, err
	}
	reverseChannelMessages(recent)

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
	bindings = cloneParticipantBindings(bindings)
	sort.Slice(bindings, func(i, j int) bool {
		return bindings[i].Participant.ID < bindings[j].Participant.ID
	})
	seen := make(map[ConversationParticipant]struct{}, len(bindings))
	for index := range bindings {
		if err := bindings[index].Validate(); err != nil {
			return nil, err
		}
		if _, exists := seen[bindings[index].Participant]; exists {
			return nil, fmt.Errorf("%w: duplicate conversation participant", ErrInvalidConversation)
		}
		seen[bindings[index].Participant] = struct{}{}
	}

	proposals := make([]ParticipationProposal, len(bindings))
	leases := make([]*ConversationPresence, len(bindings))
	errs := make([]error, len(bindings))
	jobs := make(chan int)
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var workers sync.WaitGroup
	maximumConcurrency := c.config.MaximumConcurrency
	if req.MaximumConcurrency > 0 {
		maximumConcurrency = req.MaximumConcurrency
	}
	workerCount := min(maximumConcurrency, len(bindings))
	workers.Add(workerCount)
	for range workerCount {
		go func() {
			defer workers.Done()
			for index := range jobs {
				binding := bindings[index]
				presence, presenceErr := c.conversations.SetPresence(workerCtx, SetConversationPresenceRequest{
					Scope: req.Scope, ConversationID: conversation.ID, Participant: binding.Participant,
					State: ConversationPresenceWorking, Summary: "Reviewing channel activity", TTL: c.config.PresenceTTL,
				})
				if presenceErr != nil {
					errs[index] = fmt.Errorf("set participant %s presence: %w", binding.Participant.ID, presenceErr)
					cancel()
					continue
				}
				leases[index] = presence
				proposalCtx, proposalCancel := context.WithTimeout(workerCtx, c.config.ProposalTimeout)
				proposal, proposalErr := c.proposals.ProposeParticipation(proposalCtx, ParticipationProposalContext{
					Conversation: cloneConversation(conversation), Trigger: cloneChannelMessage(trigger),
					RecentMessages: cloneChannelMessages(recent), Participant: binding.Participant,
					SemanticRoles: append([]string(nil), binding.SemanticRoles...), Priority: binding.Priority,
				})
				proposalCancel()
				if proposalErr != nil {
					errs[index] = fmt.Errorf("participant %s proposal: %w", binding.Participant.ID, proposalErr)
					cancel()
					continue
				}
				proposal.ID = ""
				proposal.RoundID = ""
				proposal.Participant = binding.Participant
				proposal.SemanticRoles = append([]string(nil), binding.SemanticRoles...)
				proposal.Priority = binding.Priority
				proposal.Signals.DirectlyMentioned = proposal.Signals.DirectlyMentioned || participantDirectlyMentioned(trigger, binding.Participant)
				proposal.Signals.RoleRelevant = proposal.Signals.RoleRelevant || participantRoleAddressed(trigger, binding.SemanticRoles)
				if err := validateGeneratedParticipationProposal(proposal); err != nil {
					errs[index] = fmt.Errorf("participant %s proposal: %w", binding.Participant.ID, err)
					cancel()
					continue
				}
				if err := c.markObserved(workerCtx, conversation, binding.Participant); err != nil {
					errs[index] = fmt.Errorf("mark participant %s read: %w", binding.Participant.ID, err)
					cancel()
					continue
				}
				proposals[index] = proposal
			}
		}()
	}
queue:
	for index := range bindings {
		select {
		case jobs <- index:
		case <-workerCtx.Done():
			break queue
		}
	}
	close(jobs)
	workers.Wait()
	defer c.releasePresenceLeases(ctx, req.Scope, conversation.ID, bindings, leases)
	for _, proposalErr := range errs {
		if proposalErr != nil {
			return nil, proposalErr
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return c.conversations.CoordinateParticipation(ctx, CoordinateParticipationRequest{
		Scope: req.Scope, ConversationID: conversation.ID, ExpectedRevision: req.ExpectedRevision,
		TriggerMessageID: strings.TrimSpace(req.TriggerMessageID), Policy: policy, Proposals: proposals, IdempotencyKey: key,
	})
}

func (c *ConversationCoordinator) markObserved(ctx context.Context, conversation *Conversation, participant ConversationParticipant) error {
	for range 2 {
		cursor, err := c.conversations.GetCursor(ctx, conversation.Scope, conversation.ID, participant)
		if err != nil {
			return err
		}
		if cursor != nil && cursor.ReadSequence >= conversation.LastSequence && cursor.DeliveredSequence >= conversation.LastSequence {
			return nil
		}
		expectedRevision := int64(0)
		if cursor != nil {
			expectedRevision = cursor.Revision
		}
		_, _, err = c.conversations.AdvanceCursor(ctx, AdvanceConversationCursorRequest{
			Scope: conversation.Scope, ConversationID: conversation.ID, Participant: participant,
			ExpectedRevision: expectedRevision, DeliveredSequence: conversation.LastSequence, ReadSequence: conversation.LastSequence,
		})
		if !errors.Is(err, ErrConversationCursorConflict) {
			return err
		}
	}
	return ErrConversationCursorConflict
}

func (c *ConversationCoordinator) releasePresenceLeases(
	ctx context.Context,
	scope Scope,
	conversationID string,
	bindings []ConversationParticipantBinding,
	leases []*ConversationPresence,
) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	for index, lease := range leases {
		if lease == nil {
			continue
		}
		_ = c.conversations.ReleasePresence(cleanupCtx, ReleaseConversationPresenceRequest{
			Scope: scope, ConversationID: conversationID, Participant: bindings[index].Participant, LeaseID: lease.LeaseID,
		})
	}
}

func validateGeneratedParticipationProposal(proposal ParticipationProposal) error {
	copy := proposal
	copy.ID = "candidate"
	copy.RoundID = "candidate-round"
	return copy.Validate()
}

func participantDirectlyMentioned(trigger *ChannelMessage, participant ConversationParticipant) bool {
	if trigger == nil {
		return false
	}
	for _, mention := range trigger.Mentions {
		if mention == participant {
			return true
		}
	}
	if trigger.Audience.Kind == ConversationAudienceParticipants {
		for _, target := range trigger.Audience.Participants {
			if target == participant {
				return true
			}
		}
	}
	return false
}

func participantRoleAddressed(trigger *ChannelMessage, roles []string) bool {
	if trigger == nil || trigger.Audience.Kind != ConversationAudienceRoles {
		return false
	}
	for _, addressed := range trigger.Audience.Roles {
		for _, role := range roles {
			if strings.EqualFold(strings.TrimSpace(addressed), strings.TrimSpace(role)) {
				return true
			}
		}
	}
	return false
}

func reverseChannelMessages(messages []*ChannelMessage) {
	for left, right := 0, len(messages)-1; left < right; left, right = left+1, right-1 {
		messages[left], messages[right] = messages[right], messages[left]
	}
}

func cloneParticipantBindings(values []ConversationParticipantBinding) []ConversationParticipantBinding {
	result := make([]ConversationParticipantBinding, len(values))
	for index, value := range values {
		result[index] = value
		result[index].SemanticRoles = append([]string(nil), value.SemanticRoles...)
	}
	return result
}
