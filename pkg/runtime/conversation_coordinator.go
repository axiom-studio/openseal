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
	ErrConversationParticipationQuorum     = errors.New("conversation participation quorum is unavailable")
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

// ParticipationProposalBudget is an enforceable allowance for one provider
// invocation. A metered host must preflight input and cap generation to it.
type ParticipationProposalBudget struct {
	InputTokens  int64
	OutputTokens int64
}

func (b ParticipationProposalBudget) validate() error {
	if b.InputTokens < 1 || b.OutputTokens < 1 || b.InputTokens > 1_000_000_000 || b.OutputTokens > 1_000_000_000 {
		return errors.New("participation token allowances must be between 1 and 1000000000")
	}
	return nil
}

type ParticipationProposalContext struct {
	HistorySummary    string
	HistoryCompaction *ConversationCompactionRequest
	Budget            *ParticipationProposalBudget
	Conversation      *Conversation
	Trigger           *ChannelMessage
	RecentMessages    []*ChannelMessage
	OpenMessages      []*ChannelMessage
	Participant       ConversationParticipant
	SemanticRoles     []string
	Priority          int
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

// MeteredParticipationProposal is a host report, not model-authored usage.
// Providers return any known usage even when generation or validation fails.
type MeteredParticipationProposal struct {
	Proposal ParticipationProposal
	Usage    TurnUsage
}

// MeteredParticipationProposalProvider extends the legacy embedding contract.
// Durable conversation Runs use this usage to charge every attempted proposal,
// including proposals discarded by arbitration or a channel revision conflict.
type MeteredParticipationProposalProvider interface {
	ProposeParticipationWithUsage(context.Context, ParticipationProposalContext) (MeteredParticipationProposal, error)
}

type MeteredParticipationProposalProviderFunc func(context.Context, ParticipationProposalContext) (MeteredParticipationProposal, error)

func (f MeteredParticipationProposalProviderFunc) ProposeParticipationWithUsage(ctx context.Context, input ParticipationProposalContext) (MeteredParticipationProposal, error) {
	return f(ctx, input)
}

func (f MeteredParticipationProposalProviderFunc) ProposeParticipation(ctx context.Context, input ParticipationProposalContext) (ParticipationProposal, error) {
	result, err := f(ctx, input)
	return result.Proposal, err
}

type ConversationCoordinationRequest struct {
	// UsageTurnID is the durable host turn that will settle this invocation.
	UsageTurnID      string
	Scope            Scope
	ConversationID   string
	ExpectedRevision int64
	TriggerMessageID string
	// MessageReferences are host-owned durable links attached to every message
	// emitted by this coordination round. Providers cannot remove them. A Run
	// host uses this to make each visible contribution traceable to the durable
	// work that produced it.
	MessageReferences  []ConversationReference
	Policy             ConversationArbitrationPolicy
	MaximumConcurrency int
	IdempotencyKey     string
}

type ConversationCoordinatorConfig struct {
	// RequireUsageReporting rejects providers that cannot report actual usage.
	RequireUsageReporting bool
	// ProposalBudget bounds each participant; planning reserves for the configured
	// maximum roster so membership changes cannot exceed the admitted round.
	ProposalBudget      ParticipationProposalBudget
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
	if c.ProposalBudget != (ParticipationProposalBudget{}) {
		if err := c.ProposalBudget.validate(); err != nil {
			return ConversationCoordinatorConfig{}, err
		}
		c.RequireUsageReporting = true
	}
	return c, nil
}

// ConversationCoordinator turns a channel wake into one durable, audited
// participation round. Model calls happen concurrently behind a bounded host
// interface; arbitration and persistence remain deterministic and atomic.
type ConversationCoordinator struct {
	summaries     conversationSummaryCache
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
	if normalized.RequireUsageReporting {
		if _, ok := proposals.(MeteredParticipationProposalProvider); !ok {
			return nil, errors.New("conversation provider must report usage")
		}
	}
	return &ConversationCoordinator{conversations: conversations, participants: participants, proposals: proposals, config: normalized}, nil
}

func (c *ConversationCoordinator) Coordinate(ctx context.Context, req ConversationCoordinationRequest) (*ParticipationRoundResult, error) {
	result, _, err := c.CoordinateWithUsage(ctx, req)
	return result, err
}

// CoordinateWithUsage reports usage from this invocation only. Replaying a
// persisted round performs no provider calls and returns zero usage. Usage is
// returned even when the round fails or its messages cannot be committed.
func (c *ConversationCoordinator) CoordinateWithUsage(ctx context.Context, req ConversationCoordinationRequest) (*ParticipationRoundResult, TurnUsage, error) {
	var usage TurnUsage
	result, err := c.coordinate(ctx, req, &usage)
	return result, usage, err
}

func (c *ConversationCoordinator) coordinate(ctx context.Context, req ConversationCoordinationRequest, usage *TurnUsage) (*ParticipationRoundResult, error) {
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
	for _, reference := range req.MessageReferences {
		if err := reference.Validate(); err != nil {
			return nil, err
		}
	}
	policy, err := req.Policy.normalize()
	if err != nil {
		return nil, err
	}
	if existing, err := c.conversations.FindParticipationRoundByIdempotencyKey(ctx, req.Scope, req.ConversationID, key); err != nil {
		return nil, err
	} else if existing != nil {
		if existing.Round.TriggerMessageID != strings.TrimSpace(req.TriggerMessageID) ||
			!sameConversationArbitrationPolicy(existing.Round.Policy, policy) {
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
	participantRecent := make([][]*ChannelMessage, len(bindings))
	participantTriggers := make([]*ChannelMessage, len(bindings))
	participantOpen := make([][]*ChannelMessage, len(bindings))
	for index, binding := range bindings {
		viewer := ConversationViewer{Participant: binding.Participant, Roles: append([]string(nil), binding.SemanticRoles...)}
		visible, err := c.conversations.filterVisibleChannelMessages(ctx, req.Scope, conversation.ID, recent, viewer)
		if err != nil {
			return nil, fmt.Errorf("project participant %s channel context: %w", binding.Participant.ID, err)
		}
		participantRecent[index] = visible
		participantOpen[index] = openConversationMessages(visible)
		if trigger != nil {
			projected, err := c.conversations.filterVisibleChannelMessages(ctx, req.Scope, conversation.ID, []*ChannelMessage{trigger}, viewer)
			if err != nil {
				return nil, fmt.Errorf("project participant %s trigger: %w", binding.Participant.ID, err)
			}
			if len(projected) == 1 {
				participantTriggers[index] = projected[0]
			}
		}
	}

	proposals := make([]ParticipationProposal, len(bindings))
	usages := make([]TurnUsage, len(bindings))
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
				visibleTrigger := participantTriggers[index]
				visibleRecent := participantRecent[index]
				visibleOpen := participantOpen[index]
				if trigger != nil && visibleTrigger == nil {
					if err := c.markObserved(workerCtx, conversation, binding.Participant, latestConversationSequence(visibleRecent)); err != nil {
						errs[index] = fmt.Errorf("mark participant %s read: %w", binding.Participant.ID, err)
						cancel()
						continue
					}
					proposals[index] = ParticipationProposal{Participant: binding.Participant, SemanticRoles: append([]string(nil), binding.SemanticRoles...), Priority: binding.Priority, Availability: ParticipationAvailability{Status: ParticipationNotInvited}}
					continue
				}
				if workerCtx.Err() != nil {
					continue
				}
				current, checkErr := c.conversations.GetConversation(workerCtx, req.Scope, conversation.ID)
				if checkErr != nil || current.Revision != conversation.Revision {
					if checkErr == nil {
						checkErr = ErrRevisionConflict
					}
					errs[index] = checkErr
					cancel()
					continue
				}
				presence, presenceErr := c.conversations.SetPresence(workerCtx, SetConversationPresenceRequest{
					Scope: req.Scope, ConversationID: conversation.ID, Participant: binding.Participant,
					State: ConversationPresenceThinking, Summary: "Reviewing channel activity", TTL: c.config.PresenceTTL,
				})
				if presenceErr != nil {
					errs[index] = fmt.Errorf("set participant %s presence: %w", binding.Participant.ID, presenceErr)
					cancel()
					continue
				}
				leases[index] = presence
				proposalCtx, proposalCancel := context.WithTimeout(workerCtx, c.config.ProposalTimeout)
				proposalInput := ParticipationProposalContext{
					Conversation: cloneConversation(conversation), Trigger: cloneChannelMessage(visibleTrigger),
					RecentMessages: cloneChannelMessages(visibleRecent), Participant: binding.Participant,
					OpenMessages:  cloneChannelMessages(visibleOpen),
					SemanticRoles: append([]string(nil), binding.SemanticRoles...), Priority: binding.Priority,
				}
				viewer := ConversationViewer{Participant: binding.Participant, Roles: append([]string(nil), binding.SemanticRoles...)}
				historyPlan := conversationHistoryPlan{Messages: visibleRecent}
				saved := c.summaries.get(conversation.Scope, conversation.ID, conversationViewerKey(viewer))
				history, historyErr := loadConversationCompactionHistory(proposalCtx, c.conversations, conversation, viewer, visibleRecent, saved)
				if historyErr == nil {
					triggerID := ""
					if visibleTrigger != nil {
						triggerID = visibleTrigger.ID
					}
					historyPlan = planConversationHistory(conversation, triggerID, viewer, history, saved)
					proposalInput.RecentMessages = cloneChannelMessages(historyPlan.Messages)
					if historyPlan.Summary != nil {
						proposalInput.HistorySummary = historyPlan.Summary.Text
					}
					if historyPlan.Request != nil {
						request := *historyPlan.Request
						request.SourceMessageIDs = append([]string(nil), request.SourceMessageIDs...)
						proposalInput.HistoryCompaction = &request
					}
				}
				if c.config.ProposalBudget != (ParticipationProposalBudget{}) {
					budget := c.config.ProposalBudget
					proposalInput.Budget = &budget
				}
				var proposal ParticipationProposal
				var proposalErr error
				if metered, ok := c.proposals.(MeteredParticipationProposalProvider); ok {
					report, err := metered.ProposeParticipationWithUsage(proposalCtx, proposalInput)
					proposal, proposalErr = report.Proposal, err
					if usageErr := report.Usage.Validate(); usageErr != nil {
						errs[index] = fmt.Errorf("participant %s reported invalid usage: %w", binding.Participant.ID, usageErr)
						proposalCancel()
						cancel()
						continue
					}
					usages[index] = report.Usage
					budget := c.config.ProposalBudget
					if budget != (ParticipationProposalBudget{}) && (int64(report.Usage.InputTokens) > budget.InputTokens || int64(report.Usage.OutputTokens) > budget.OutputTokens) {
						errs[index] = fmt.Errorf("%w: participant %s exceeded its token allowance", ErrInvalidConversation, binding.Participant.ID)
						proposalCancel()
						cancel()
						continue
					}
				} else {
					proposal, proposalErr = c.proposals.ProposeParticipation(proposalCtx, proposalInput)
				}
				proposalCancel()
				if proposalErr != nil {
					proposals[index] = unavailableParticipationProposal(binding, publicParticipationFailureCode(proposalErr))
					continue
				}
				if summary := acceptedConversationSummary(conversation, viewer, historyPlan, proposal.WorkingContextCheckpoint); summary != nil {
					c.summaries.put(summary)
				}
				proposal.WorkingContextCheckpoint = nil
				proposal.ID = ""
				proposal.RoundID = ""
				proposal.Participant = binding.Participant
				proposal.SemanticRoles = append([]string(nil), binding.SemanticRoles...)
				proposal.Priority = binding.Priority
				proposal.Availability = ParticipationAvailability{Status: ParticipationAvailable}
				directlyMentioned := participantDirectlyMentioned(visibleTrigger, binding.Participant)
				proposal.Signals.DirectlyMentioned = directlyMentioned
				proposal.Signals.TriggerTargetsOtherParticipant = triggerTargetsSpecificAgent(visibleTrigger) && !directlyMentioned
				proposal.Signals.RoleRelevant = proposal.Signals.RoleRelevant || participantRoleAddressed(visibleTrigger, binding.SemanticRoles)
				proposal.Signals.AnswersOpenQuestion = proposal.Signals.AnswersOpenQuestion && proposalTargetsOpenMessage(proposal, visibleOpen, MessageIntentQuestion, false)
				proposal.Signals.ResolvesOpenWork = proposal.Signals.ResolvesOpenWork && proposalTargetsOpenMessage(proposal, visibleOpen, "", true)
				if err := validateGeneratedParticipationProposal(proposal); err != nil {
					errs[index] = fmt.Errorf("participant %s proposal: %w", binding.Participant.ID, err)
					cancel()
					continue
				}
				if err := c.markObserved(workerCtx, conversation, binding.Participant, latestConversationSequence(visibleRecent)); err != nil {
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
	for _, reported := range usages {
		combined, err := addParticipationUsage(*usage, reported)
		if err != nil {
			return nil, err
		}
		*usage = combined
	}
	for _, proposalErr := range errs {
		if proposalErr != nil {
			return nil, proposalErr
		}
	}
	for index := range proposals {
		if proposals[index].WantsToSpeak {
			proposals[index].References = mergeConversationReferences(proposals[index].References, req.MessageReferences)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateParticipationQuorum(proposals, policy); err != nil {
		return nil, err
	}

	return c.conversations.CoordinateParticipation(ctx, CoordinateParticipationRequest{
		Scope: req.Scope, ConversationID: conversation.ID, ExpectedRevision: req.ExpectedRevision,
		TriggerMessageID: strings.TrimSpace(req.TriggerMessageID), Policy: policy, Proposals: proposals, IdempotencyKey: key,
		Usage: *usage, UsageTurnID: req.UsageTurnID,
	})
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

func unavailableParticipationProposal(binding ConversationParticipantBinding, failureCode string) ParticipationProposal {
	return ParticipationProposal{
		Participant: binding.Participant, SemanticRoles: append([]string(nil), binding.SemanticRoles...), Priority: binding.Priority,
		Availability: ParticipationAvailability{Status: ParticipationUnavailable, FailureCode: failureCode},
	}
}

func publicParticipationFailureCode(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "participant_timeout"
	}
	return "participant_runtime_unavailable"
}

func validateParticipationQuorum(proposals []ParticipationProposal, policy ConversationArbitrationPolicy) error {
	available := 0
	requiredRoleAvailable := strings.TrimSpace(policy.RequiredAvailableRole) == ""
	for _, proposal := range proposals {
		if proposal.Availability.Status != ParticipationAvailable {
			continue
		}
		available++
		for _, role := range proposal.SemanticRoles {
			if role == policy.RequiredAvailableRole {
				requiredRoleAvailable = true
			}
		}
	}
	if available < policy.MinimumAvailableParticipants || !requiredRoleAvailable {
		return ErrConversationParticipationQuorum
	}
	return nil
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

func latestConversationSequence(messages []*ChannelMessage) int64 {
	var latest int64
	for _, message := range messages {
		if message != nil && message.Sequence > latest {
			latest = message.Sequence
		}
	}
	return latest
}

func openConversationMessages(messages []*ChannelMessage) []*ChannelMessage {
	closed := make(map[string]bool)
	for _, message := range messages {
		if message == nil {
			continue
		}
		for _, id := range []string{message.ResolvesMessageID, message.SupersedesMessageID} {
			if id != "" {
				closed[id] = true
			}
		}
	}
	open := make([]*ChannelMessage, 0)
	for _, message := range messages {
		if message != nil && message.RequiresResponse && !closed[message.ID] {
			open = append(open, cloneChannelMessage(message))
		}
	}
	return open
}

func proposalTargetsOpenMessage(proposal ParticipationProposal, open []*ChannelMessage, intent ConversationMessageIntent, requireResolution bool) bool {
	targets := []string{proposal.ReplyToMessageID, proposal.ResolvesMessageID}
	if requireResolution {
		targets = []string{proposal.ResolvesMessageID}
	}
	for _, target := range targets {
		if target == "" {
			continue
		}
		for _, message := range open {
			if message != nil && message.ID == target && (intent == "" || message.Intent == intent) {
				return true
			}
		}
	}
	return false
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

func triggerTargetsSpecificAgent(trigger *ChannelMessage) bool {
	if trigger == nil {
		return false
	}
	for _, mention := range trigger.Mentions {
		if mention.Type == ConversationParticipantAgent {
			return true
		}
	}
	if trigger.Audience.Kind == ConversationAudienceParticipants {
		for _, target := range trigger.Audience.Participants {
			if target.Type == ConversationParticipantAgent {
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
