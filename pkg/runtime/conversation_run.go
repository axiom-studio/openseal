package runtime

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	conversationRunContextConversationID = "conversationId"
	conversationRunContextTriggerID      = "triggerMessageId"
	conversationRunContextTriggerSeq     = "triggerSequence"
	conversationRunSchedulerParticipant  = "conversation-run-scheduler"
)

type ConversationRunSchedulerConfig struct {
	ConversationPageSize int
	MessagePageSize      int
}

func (c ConversationRunSchedulerConfig) normalize() (ConversationRunSchedulerConfig, error) {
	if c.ConversationPageSize == 0 {
		c.ConversationPageSize = 100
	}
	if c.MessagePageSize == 0 {
		c.MessagePageSize = 100
	}
	if c.ConversationPageSize < 1 || c.ConversationPageSize > 1000 || c.MessagePageSize < 1 || c.MessagePageSize > 1000 {
		return ConversationRunSchedulerConfig{}, fmt.Errorf("%w: conversation reconciliation page sizes must be between 1 and 1000", ErrInvalidConversation)
	}
	return c, nil
}

type ConversationRunReconcileResult struct {
	Conversations int `json:"conversations"`
	Messages      int `json:"messages"`
	Scheduled     int `json:"scheduled"`
	Replayed      int `json:"replayed"`
	Skipped       int `json:"skipped"`
}

// ConversationRunScheduler projects durable channel messages into canonical
// Runs. Message identity is the durable wake fact: immediate scheduling is a
// latency optimization, while ReconcileScope closes the crash gap by advancing
// a service cursor only after each page has been scheduled idempotently.
type ConversationRunScheduler struct {
	conversations *ConversationService
	runs          *RunCommandService
	config        ConversationRunSchedulerConfig
}

func NewConversationRunScheduler(
	conversationStore ConversationStore,
	runStore RunCommandStore,
	config ConversationRunSchedulerConfig,
) (*ConversationRunScheduler, error) {
	if conversationStore == nil || runStore == nil {
		return nil, errors.New("conversation and run command stores are required")
	}
	normalized, err := config.normalize()
	if err != nil {
		return nil, err
	}
	return &ConversationRunScheduler{
		conversations: NewConversationService(conversationStore),
		runs:          NewRunCommandService(runStore),
		config:        normalized,
	}, nil
}

func (s *ConversationRunScheduler) ScheduleMessage(
	ctx context.Context,
	scope Scope,
	conversationID string,
	messageID string,
) (*AgentRunCommandResult, bool, error) {
	if s == nil || s.conversations == nil || s.runs == nil {
		return nil, false, errors.New("conversation run scheduler is not configured")
	}
	conversation, err := s.conversations.GetConversation(ctx, scope, strings.TrimSpace(conversationID))
	if err != nil {
		return nil, false, err
	}
	message, err := s.conversations.GetChannelMessage(ctx, scope, conversation.ID, strings.TrimSpace(messageID))
	if err != nil {
		return nil, false, err
	}
	if !conversationMessageStartsRun(conversation, message) {
		return nil, false, nil
	}
	request := conversationAgentRunRequest(conversation, message)
	current, err := s.runs.store.GetAgentRun(ctx, conversation.Scope, runIDForIdempotencyKey(conversation.Scope, request.IdempotencyKey))
	if err != nil {
		return nil, false, err
	}
	if current != nil {
		result, err := s.runs.CreateAgentRun(ctx, request)
		return result, true, err
	}
	coordinated, err := s.coordinatedTriggerIDs(ctx, conversation)
	if err != nil {
		return nil, false, err
	}
	if coordinated[message.ID] {
		return nil, false, nil
	}
	return s.scheduleLoadedMessage(ctx, conversation, message)
}

func (s *ConversationRunScheduler) scheduleLoadedMessage(
	ctx context.Context,
	conversation *Conversation,
	message *ChannelMessage,
) (*AgentRunCommandResult, bool, error) {
	result, err := s.runs.CreateAgentRun(ctx, conversationAgentRunRequest(conversation, message))
	if err != nil {
		return nil, false, err
	}
	return result, result.Event == nil, nil
}

func conversationAgentRunRequest(conversation *Conversation, message *ChannelMessage) CreateAgentRunRequest {
	return CreateAgentRunRequest{
		Scope: conversation.Scope, Kind: RunKindConversation, Owner: conversation.Owner,
		ConcurrencyKey: conversation.ID, Goal: "Coordinate Team channel participation", Source: RunSourceChat,
		Context: map[string]interface{}{
			conversationRunContextConversationID: conversation.ID,
			conversationRunContextTriggerID:      message.ID,
			conversationRunContextTriggerSeq:     message.Sequence,
		},
		IdempotencyKey: conversationRunIdempotencyKey(conversation.Scope, conversation.ID, message.ID),
		Actor:          ActivityActor{Type: "service", ID: conversationRunSchedulerParticipant},
		Visibility:     ActivityVisibilityTeam,
	}
}

func (s *ConversationRunScheduler) ReconcileScope(ctx context.Context, scope Scope) (*ConversationRunReconcileResult, error) {
	if s == nil || s.conversations == nil || s.runs == nil {
		return nil, errors.New("conversation run scheduler is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	result := &ConversationRunReconcileResult{}
	for offset := 0; ; offset += s.config.ConversationPageSize {
		conversations, err := s.conversations.ListConversations(ctx, ConversationFilter{
			Scope: scope, Statuses: []ConversationStatus{ConversationStatusActive},
			Limit: s.config.ConversationPageSize, Offset: offset,
		})
		if err != nil {
			return result, err
		}
		for _, conversation := range conversations {
			if conversation.Owner.Type != OwnerTypeTeam {
				continue
			}
			result.Conversations++
			if err := s.reconcileConversation(ctx, conversation, result); err != nil {
				return result, err
			}
		}
		if len(conversations) < s.config.ConversationPageSize {
			return result, nil
		}
	}
}

func (s *ConversationRunScheduler) reconcileConversation(
	ctx context.Context,
	conversation *Conversation,
	result *ConversationRunReconcileResult,
) error {
	participant := ConversationParticipant{Type: ConversationParticipantService, ID: conversationRunSchedulerParticipant}
	coordinated, err := s.coordinatedTriggerIDs(ctx, conversation)
	if err != nil {
		return err
	}
	for {
		cursor, err := s.conversations.GetCursor(ctx, conversation.Scope, conversation.ID, participant)
		if err != nil {
			return err
		}
		afterSequence := int64(0)
		if cursor != nil {
			afterSequence = cursor.ReadSequence
		}
		messages, err := s.conversations.ListChannelMessages(ctx, ChannelMessageFilter{
			Scope: conversation.Scope, ConversationID: conversation.ID,
			AfterSequence: afterSequence, Limit: s.config.MessagePageSize,
		})
		if err != nil {
			return err
		}
		if len(messages) == 0 {
			return nil
		}
		for _, message := range messages {
			result.Messages++
			if !conversationMessageStartsRun(conversation, message) || coordinated[message.ID] {
				result.Skipped++
				continue
			}
			run, replayed, err := s.scheduleLoadedMessage(ctx, conversation, message)
			if err != nil {
				return err
			}
			if run == nil {
				result.Skipped++
			} else if replayed {
				result.Replayed++
			} else {
				result.Scheduled++
			}
		}
		lastSequence := messages[len(messages)-1].Sequence
		if err := s.advanceSchedulerCursor(ctx, conversation, participant, lastSequence); err != nil {
			return err
		}
		if len(messages) < s.config.MessagePageSize {
			return nil
		}
	}
}

func (s *ConversationRunScheduler) coordinatedTriggerIDs(ctx context.Context, conversation *Conversation) (map[string]bool, error) {
	const pageSize = 100
	result := make(map[string]bool)
	for offset := 0; ; offset += pageSize {
		rounds, err := s.conversations.ListParticipationRounds(ctx, ParticipationRoundFilter{
			Scope: conversation.Scope, ConversationID: conversation.ID, Limit: pageSize, Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		for _, round := range rounds {
			if round != nil && round.Round != nil && round.Round.TriggerMessageID != "" {
				result[round.Round.TriggerMessageID] = true
			}
		}
		if len(rounds) < pageSize {
			return result, nil
		}
	}
}

func (s *ConversationRunScheduler) advanceSchedulerCursor(
	ctx context.Context,
	conversation *Conversation,
	participant ConversationParticipant,
	sequence int64,
) error {
	for range 3 {
		cursor, err := s.conversations.GetCursor(ctx, conversation.Scope, conversation.ID, participant)
		if err != nil {
			return err
		}
		if cursor != nil && cursor.ReadSequence >= sequence {
			return nil
		}
		revision := int64(0)
		if cursor != nil {
			revision = cursor.Revision
		}
		_, _, err = s.conversations.AdvanceCursor(ctx, AdvanceConversationCursorRequest{
			Scope: conversation.Scope, ConversationID: conversation.ID, Participant: participant,
			ExpectedRevision: revision, DeliveredSequence: sequence, ReadSequence: sequence,
		})
		if !errors.Is(err, ErrConversationCursorConflict) {
			return err
		}
	}
	return ErrConversationCursorConflict
}

func conversationMessageStartsRun(conversation *Conversation, message *ChannelMessage) bool {
	if conversation == nil || message == nil || conversation.Status != ConversationStatusActive ||
		conversation.Owner.Type != OwnerTypeTeam || message.ConversationID != conversation.ID || message.Scope != conversation.Scope {
		return false
	}
	if message.Intent == MessageIntentSystem || message.ParticipationRoundID != "" {
		return false
	}
	switch message.Sender.Type {
	case ConversationParticipantUser, ConversationParticipantAgent, ConversationParticipantService:
		return true
	default:
		return false
	}
}

func conversationRunIdempotencyKey(scope Scope, conversationID, messageID string) string {
	return "conversation-run:" + hashString(scope.Kind+"\x00"+scope.ID+"\x00"+conversationID+"\x00"+messageID)
}

type ConversationRunTurnRunnerConfig struct {
	MaximumRetries     int
	InitialRetryDelay  time.Duration
	MaximumRetryDelay  time.Duration
	Policy             ConversationArbitrationPolicy
	MaximumConcurrency int
}

func (c ConversationRunTurnRunnerConfig) normalize() (ConversationRunTurnRunnerConfig, error) {
	if c.MaximumRetries == 0 {
		c.MaximumRetries = 5
	}
	if c.InitialRetryDelay == 0 {
		c.InitialRetryDelay = time.Second
	}
	if c.MaximumRetryDelay == 0 {
		c.MaximumRetryDelay = time.Minute
	}
	if c.MaximumRetries < 0 || c.MaximumRetries > 100 || c.InitialRetryDelay <= 0 ||
		c.MaximumRetryDelay < c.InitialRetryDelay || c.MaximumRetryDelay > time.Hour || c.MaximumConcurrency < 0 {
		return ConversationRunTurnRunnerConfig{}, fmt.Errorf("%w: invalid conversation Run retry configuration", ErrInvalidConversation)
	}
	policy, err := c.Policy.normalize()
	if err != nil {
		return ConversationRunTurnRunnerConfig{}, err
	}
	c.Policy = policy
	return c, nil
}

// ConversationRunTurnRunner executes one governed participation round as a
// bounded Run turn. The round idempotency key closes the crash gap after round
// commit; optimistic channel drift and transient proposal failures sleep and
// retry without occupying the per-channel concurrency lease.
type ConversationRunTurnRunner struct {
	conversations *ConversationService
	coordinator   *ConversationCoordinator
	config        ConversationRunTurnRunnerConfig
	now           func() time.Time
}

func NewConversationRunTurnRunner(
	conversationStore ConversationStore,
	coordinator *ConversationCoordinator,
	config ConversationRunTurnRunnerConfig,
) (*ConversationRunTurnRunner, error) {
	if conversationStore == nil || coordinator == nil {
		return nil, ErrConversationCoordinationUnavailable
	}
	normalized, err := config.normalize()
	if err != nil {
		return nil, err
	}
	return &ConversationRunTurnRunner{
		conversations: NewConversationService(conversationStore), coordinator: coordinator, config: normalized, now: time.Now,
	}, nil
}

func (r *ConversationRunTurnRunner) ResolveTurnRunner(_ context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
	if r == nil || r.coordinator == nil || r.conversations == nil {
		return nil, ErrConversationCoordinationUnavailable
	}
	if err := validateConversationRun(run); err != nil {
		return nil, err
	}
	return &TurnRunnerBinding{
		Runner: r, DefinitionID: "openseal.conversation-coordinator", DefinitionVersion: "1",
		ModelProvider: "host", Model: "participant-runtime",
		InputContextRefs: []string{conversationRunContextConversationID, conversationRunContextTriggerID},
	}, nil
}

func (r *ConversationRunTurnRunner) RunTurn(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
	if err := validateConversationRun(input.Run); err != nil {
		return nil, err
	}
	conversationID, _ := input.Run.Context[conversationRunContextConversationID].(string)
	triggerID, _ := input.Run.Context[conversationRunContextTriggerID].(string)
	conversation, err := r.conversations.GetConversation(ctx, input.Run.Scope, conversationID)
	if err != nil {
		return nil, err
	}
	key := "participation-round:" + hashString(input.Run.Scope.Kind+"\x00"+input.Run.Scope.ID+"\x00"+conversationID+"\x00"+triggerID)
	result, err := r.coordinator.Coordinate(ctx, ConversationCoordinationRequest{
		Scope: input.Run.Scope, ConversationID: conversationID, ExpectedRevision: conversation.Revision,
		TriggerMessageID: triggerID, Policy: r.config.Policy, MaximumConcurrency: r.config.MaximumConcurrency,
		IdempotencyKey: key,
	})
	if err != nil {
		if ctx.Err() != nil || permanentConversationRunError(err) {
			return nil, err
		}
		return r.retryOutcome(input.Run, err)
	}
	messageIDs := make([]interface{}, 0, len(result.Messages))
	for _, message := range result.Messages {
		messageIDs = append(messageIDs, message.ID)
	}
	return &TurnOutcome{
		NextRunStatus: AgentRunStatusCompleted,
		OutputSummary: "Team channel participation coordinated",
		RunOutput: map[string]interface{}{
			"conversationId": conversationID, "triggerMessageId": triggerID,
			"participationRoundId": result.Round.ID, "messageIds": messageIDs,
			"speakerCount": len(result.Messages), "replayed": result.Replayed,
		},
	}, nil
}

func (r *ConversationRunTurnRunner) retryOutcome(run *AgentRun, cause error) (*TurnOutcome, error) {
	retries := conversationRunRetryCount(run.Checkpoint)
	if retries >= r.config.MaximumRetries {
		return nil, errors.New("conversation coordination exhausted its retry budget")
	}
	retries++
	delay := time.Duration(float64(r.config.InitialRetryDelay) * math.Pow(2, float64(retries-1)))
	if delay > r.config.MaximumRetryDelay {
		delay = r.config.MaximumRetryDelay
	}
	wakeAt := r.now().UTC().Add(delay)
	return &TurnOutcome{
		NextRunStatus: AgentRunStatusSleeping,
		OutputSummary: "Conversation coordination retry scheduled",
		ContinuationCheckpoint: map[string]interface{}{
			"conversationRetryCount": retries,
			"lastRetryReason":        publicConversationRetryReason(cause),
		},
		WakeCondition: &WakeCondition{Type: "timer", WakeAt: &wakeAt, Reference: "conversation-retry"},
	}, nil
}

func validateConversationRun(run *AgentRun) error {
	if run == nil || run.Kind != RunKindConversation || run.Owner.Type != OwnerTypeTeam {
		return fmt.Errorf("%w: Team-owned conversation Run is required", ErrInvalidAgentRun)
	}
	conversationID, conversationOK := run.Context[conversationRunContextConversationID].(string)
	triggerID, triggerOK := run.Context[conversationRunContextTriggerID].(string)
	if !conversationOK || !triggerOK || !validOpaqueIdentifier(conversationID, 128) || !validOpaqueIdentifier(triggerID, 128) ||
		run.ConcurrencyKey != conversationID {
		return fmt.Errorf("%w: conversation Run context or concurrency key is invalid", ErrInvalidAgentRun)
	}
	return nil
}

func conversationRunRetryCount(checkpoint map[string]interface{}) int {
	value, ok := checkpoint["conversationRetryCount"]
	if !ok {
		return 0
	}
	switch count := value.(type) {
	case int:
		return max(count, 0)
	case float64:
		if count >= 0 && count <= 1_000_000 && count == math.Trunc(count) {
			return int(count)
		}
	}
	return 0
}

func permanentConversationRunError(err error) bool {
	return errors.Is(err, ErrConversationNotFound) || errors.Is(err, ErrChannelMessageNotFound) ||
		errors.Is(err, ErrNoConversationParticipants) || errors.Is(err, ErrConversationCoordinationUnavailable) ||
		errors.Is(err, ErrInvalidConversation) || errors.Is(err, ErrInvalidAgentRun) || errors.Is(err, ErrMessageConflict)
}

func publicConversationRetryReason(err error) string {
	switch {
	case errors.Is(err, ErrRevisionConflict):
		return "conversation_changed"
	case errors.Is(err, ErrConversationCursorConflict):
		return "cursor_changed"
	case errors.Is(err, ErrConversationPresenceConflict):
		return "presence_changed"
	case errors.Is(err, context.DeadlineExceeded):
		return "participant_timeout"
	default:
		return "participant_runtime_unavailable"
	}
}
