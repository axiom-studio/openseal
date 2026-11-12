package conversationrun

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
)

const (
	governedActionProposalFailedStatus = "proposal_failed"
)

type SchedulerConfig struct {
	ConversationPageSize int
	MessagePageSize      int
	// RequireParticipationOptIn leaves unconfigured channels inert. Existing
	// embedding hosts retain their prior scheduling policy when false.
	RequireParticipationOptIn bool
	// Budget is host-owned and copied onto each newly scheduled Run.
	Budget *models.BudgetPolicy
}

func (c SchedulerConfig) normalize() (SchedulerConfig, error) {
	if c.ConversationPageSize == 0 {
		c.ConversationPageSize = 100
	}
	if c.MessagePageSize == 0 {
		c.MessagePageSize = 100
	}
	if c.ConversationPageSize < 1 || c.ConversationPageSize > 1000 || c.MessagePageSize < 1 || c.MessagePageSize > 1000 {
		return SchedulerConfig{}, fmt.Errorf("%w: conversation reconciliation page sizes must be between 1 and 1000", models.ErrInvalidConversation)
	}
	if c.Budget != nil {
		if err := c.Budget.Validate(); err != nil {
			return SchedulerConfig{}, err
		}
		c.Budget = models.CloneBudgetPolicy(c.Budget)
	}
	return c, nil
}

type ReconcileResult struct {
	Conversations int `json:"conversations"`
	Messages      int `json:"messages"`
	Scheduled     int `json:"scheduled"`
	Replayed      int `json:"replayed"`
	Skipped       int `json:"skipped"`
	Results       int `json:"results"`
}

// Scheduler projects durable channel messages into canonical
// Runs. Message identity is the durable wake fact: immediate scheduling is a
// latency optimization, while ReconcileScope closes the crash gap by advancing
// a service cursor only after each page has been scheduled idempotently.
type Scheduler struct {
	conversations  *runtimeconversation.Service
	runs           *agentrun.RunCommandService
	approvals      *action.ApprovalCoordinator
	config         SchedulerConfig
	sessionContext ConversationSessionContextResolver
	// conversationStore and runStore are the stores behind conversations
	// and runs, for the reads the services do not offer.
	conversationStore models.ConversationStore
	runStore          models.RunCommandStore
	// now is the clock the scheduler gives the services it builds later.
	now func() time.Time
}

type ConversationSessionContextResolver interface {
	ResolveConversationSessionContext(context.Context, models.Scope, string) (map[string]interface{}, error)
}

func NewScheduler(
	conversationStore models.ConversationStore,
	runStore models.RunCommandStore,
	config SchedulerConfig,
	opts ...models.ServiceOption,
) (*Scheduler, error) {
	if conversationStore == nil || runStore == nil {
		return nil, errors.New("conversation and run command stores are required")
	}
	normalized, err := config.normalize()
	if err != nil {
		return nil, err
	}
	return &Scheduler{
		conversations:     runtimeconversation.NewService(conversationStore, opts...),
		runs:              agentrun.NewRunCommandService(runStore, opts...),
		conversationStore: conversationStore,
		runStore:          runStore,
		approvals:         action.NewApprovalCoordinator(runStore, runStore, nil, opts...),
		config:            normalized,
		now:               models.ClockFrom(opts),
	}, nil
}

func (s *Scheduler) SetSessionContextResolver(resolver ConversationSessionContextResolver) {
	if s != nil {
		s.sessionContext = resolver
	}
}

func (s *Scheduler) ScheduleMessage(
	ctx context.Context,
	scope models.Scope,
	conversationID string,
	messageID string,
) (*agentrun.CommandResult, bool, error) {
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
	if !conversationMessageStartsRun(conversation, message) || (s.config.RequireParticipationOptIn && !conversationParticipationAllows(conversation, message)) {
		return nil, false, nil
	}
	request, err := s.conversationAgentRunRequest(ctx, conversation, message)
	if err != nil {
		return nil, false, err
	}
	current, err := s.runStore.GetAgentRun(ctx, conversation.Scope, agentrun.RunIDForIdempotencyKey(conversation.Scope, request.IdempotencyKey))
	if err != nil {
		return nil, false, err
	}
	if current != nil {
		// Existing imported runs retain their original receipt and execution key.
		// Replaying an event must not recreate or cancel already accepted work.
		if models.IsExternalChannelConversation(conversation) {
			return &agentrun.CommandResult{Run: current}, true, nil
		}
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

func (s *Scheduler) scheduleLoadedMessage(
	ctx context.Context,
	conversation *models.Conversation,
	message *models.ChannelMessage,
) (*agentrun.CommandResult, bool, error) {
	if resumed, handled, err := s.resumeConversationAnswer(ctx, conversation, message); err != nil || handled {
		return resumed, resumed != nil && resumed.Event == nil, err
	}
	if delivered, err := s.deliverToActiveConversationRun(ctx, conversation, message); err != nil || delivered != nil {
		return delivered, delivered != nil && delivered.Event == nil, err
	}
	request, err := s.conversationAgentRunRequest(ctx, conversation, message)
	if err != nil {
		return nil, false, err
	}
	result, err := s.runs.CreateAgentRun(ctx, request)
	if err != nil {
		return nil, false, err
	}
	return result, result.Event == nil, nil
}

func (s *Scheduler) conversationAgentRunRequest(ctx context.Context, conversation *models.Conversation, message *models.ChannelMessage) (models.CreateAgentRunRequest, error) {
	request := runtimeconversation.AgentRunRequest(conversation, message)
	request.Budget = models.CloneBudgetPolicy(s.config.Budget)
	if s.sessionContext == nil {
		return request, nil
	}
	session, err := s.sessionContext.ResolveConversationSessionContext(ctx, conversation.Scope, conversation.ID)
	if err != nil {
		return models.CreateAgentRunRequest{}, err
	}
	if session != nil {
		if err := models.ValidateCredentialFreeContext(session); err != nil {
			return models.CreateAgentRunRequest{}, err
		}
		request.Context[agentrun.RunContextSessionKey] = session
	}
	return request, nil
}

func (s *Scheduler) ReconcileScope(ctx context.Context, scope models.Scope) (*ReconcileResult, error) {
	if s == nil || s.conversations == nil || s.runs == nil {
		return nil, errors.New("conversation run scheduler is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	result := &ReconcileResult{}
	if err := s.reconcileSkillSetupTasks(ctx, scope, result); err != nil {
		return result, err
	}
	if err := s.reconcileConversationQuestions(ctx, scope, result); err != nil {
		return result, err
	}

	for offset := 0; ; offset += s.config.ConversationPageSize {
		conversations, err := s.conversations.ListConversations(ctx, models.ConversationFilter{
			Scope: scope, Statuses: []models.ConversationStatus{models.ConversationStatusActive},
			Limit: s.config.ConversationPageSize, Offset: offset,
		})
		if err != nil {
			return result, err
		}
		for _, conversation := range conversations {
			if conversation.Owner.Type != models.OwnerTypeTeam && conversation.Owner.Type != models.OwnerTypeAgent {
				continue
			}
			result.Conversations++
			if err := s.reconcileConversation(ctx, conversation, result); err != nil {
				return result, err
			}
		}
		if len(conversations) < s.config.ConversationPageSize {
			break
		}
	}
	if err := s.reconcileConversationOperationOutputs(ctx, scope, result); err != nil {
		return result, err
	}
	if err := s.reconcileCanceledConversationRuns(ctx, scope, result); err != nil {
		return result, err
	}
	return result, nil
}

// reconcileCanceledConversationRuns closes the crash gap between atomically
// canceling a Run/Approval/ActionCall and projecting that terminal fact back
// into its command channel. The message key is Run-stable, so any replica can
// recover it after a restart without duplicating user-visible results.
func (s *Scheduler) reconcileCanceledConversationRuns(ctx context.Context, scope models.Scope, result *ReconcileResult) error {
	const pageSize = 100
	for offset := 0; ; offset += pageSize {
		runs, err := s.runStore.ListAgentRuns(ctx, models.AgentRunFilter{
			Scope: scope, Kind: models.RunKindConversation, Statuses: []models.AgentRunStatus{models.AgentRunStatusCanceled}, Limit: pageSize, Offset: offset,
		})
		if err != nil {
			return err
		}
		for _, run := range runs {
			if run == nil {
				continue
			}
			conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
			triggerID, _ := run.Context[models.ConversationRunTriggerIDContextKey].(string)
			if !models.IsValidOpaqueIdentifier(conversationID, 128) || !models.IsValidOpaqueIdentifier(triggerID, 128) {
				continue
			}
			conversation, err := s.conversations.GetConversation(ctx, scope, conversationID)
			if err != nil {
				return err
			}
			if conversation.Status != models.ConversationStatusActive || conversation.Owner != run.Owner {
				continue
			}
			trigger, err := s.conversations.GetChannelMessage(ctx, scope, conversationID, triggerID)
			if err != nil {
				return err
			}
			if !trigger.RequiresResponse {
				continue
			}
			resolved, err := s.conversationTriggerResolved(ctx, scope, conversationID, triggerID)
			if err != nil {
				return err
			}
			if resolved {
				continue
			}
			content := "This request was canceled before completion."
			references := []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: run.ID}}
			approvals, err := s.runStore.ListApprovals(ctx, models.ApprovalFilter{Scope: scope, RunID: run.ID, Status: []models.ApprovalStatus{models.ApprovalStatusCanceled}, Limit: 1})
			if err != nil {
				return err
			}
			if len(approvals) == 1 {
				content = "The proposed action was canceled before execution. No changes were applied."
				references = append(references, models.ConversationReference{Kind: models.ConversationReferenceApproval, ID: approvals[0].ID})
			}
			current, err := s.conversations.GetConversation(ctx, scope, conversationID)
			if err != nil {
				return err
			}
			posted, err := s.conversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
				Scope: scope, ConversationID: conversationID, ExpectedRevision: current.Revision,
				Sender: models.ConversationParticipant{Type: models.ConversationParticipantService, ID: "openseal.conversation"},
				Intent: models.MessageIntentSystem, Content: content, Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
				ReplyToMessageID: triggerID, References: references, ResolvesMessageID: triggerID,
				IdempotencyKey: "conversation-run-canceled-result:" + run.ID,
			})
			if errors.Is(err, models.ErrRevisionConflict) {
				continue
			}
			if err != nil {
				return err
			}
			if !posted.Replayed {
				result.Results++
			}
		}
		if len(runs) < pageSize {
			return nil
		}
	}
}

func (s *Scheduler) conversationTriggerResolved(ctx context.Context, scope models.Scope, conversationID, triggerID string) (bool, error) {
	const pageSize = 100
	for after := int64(0); ; {
		messages, err := s.conversations.ListChannelMessages(ctx, models.ChannelMessageFilter{
			Scope: scope, ConversationID: conversationID, AfterSequence: after, Limit: pageSize,
		})
		if err != nil {
			return false, err
		}
		for _, message := range messages {
			if message.ResolvesMessageID == triggerID {
				return true, nil
			}
		}
		if len(messages) < pageSize {
			return false, nil
		}
		after = messages[len(messages)-1].Sequence
	}
}

func (s *Scheduler) reconcileConversation(
	ctx context.Context,
	conversation *models.Conversation,
	result *ReconcileResult,
) error {
	participant := models.ConversationParticipant{Type: models.ConversationParticipantService, ID: models.ConversationRunSchedulerParticipant}
	var coordinated map[string]bool
	for {
		cursor, err := s.conversations.GetCursor(ctx, conversation.Scope, conversation.ID, participant)
		if err != nil {
			return err
		}
		afterSequence := int64(0)
		if cursor != nil {
			afterSequence = cursor.ReadSequence
		}
		// The canonical conversation snapshot and the current durable cursor
		// prove this snapshot has no unread messages. A concurrent later post
		// retains its sequence beyond the cursor and is handled immediately or
		// by the next recovery pass; no delivery position is advanced here.
		if conversation.LastSequence <= afterSequence {
			return nil
		}
		messages, err := s.conversations.ListChannelMessages(ctx, models.ChannelMessageFilter{
			Scope: conversation.Scope, ConversationID: conversation.ID,
			AfterSequence: afterSequence, Limit: s.config.MessagePageSize,
		})
		if err != nil {
			return err
		}
		if len(messages) == 0 {
			return nil
		}
		if coordinated == nil {
			coordinated, err = s.coordinatedTriggerIDs(ctx, conversation)
			if err != nil {
				return err
			}
		}
		for _, message := range messages {
			result.Messages++
			if !conversationMessageStartsRun(conversation, message) || (s.config.RequireParticipationOptIn && !conversationParticipationAllows(conversation, message)) || coordinated[message.ID] {
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

func (s *Scheduler) coordinatedTriggerIDs(ctx context.Context, conversation *models.Conversation) (map[string]bool, error) {
	const pageSize = 100
	result := make(map[string]bool)
	for offset := 0; ; offset += pageSize {
		rounds, err := s.conversations.ListParticipationRounds(ctx, models.ParticipationRoundFilter{
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

func (s *Scheduler) advanceSchedulerCursor(
	ctx context.Context,
	conversation *models.Conversation,
	participant models.ConversationParticipant,
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
		_, _, err = s.conversations.AdvanceCursor(ctx, runtimeconversation.AdvanceConversationCursorRequest{
			Scope: conversation.Scope, ConversationID: conversation.ID, Participant: participant,
			ExpectedRevision: revision, DeliveredSequence: sequence, ReadSequence: sequence,
		})
		if !errors.Is(err, models.ErrConversationCursorConflict) {
			return err
		}
	}
	return models.ErrConversationCursorConflict
}

func conversationMessageStartsRun(conversation *models.Conversation, message *models.ChannelMessage) bool {
	if conversation == nil || message == nil || conversation.Status != models.ConversationStatusActive ||
		(conversation.Owner.Type != models.OwnerTypeTeam && conversation.Owner.Type != models.OwnerTypeAgent) ||
		message.ConversationID != conversation.ID || message.Scope != conversation.Scope {
		return false
	}
	if message.Historical || message.Intent == models.MessageIntentSystem || message.ParticipationRoundID != "" {
		return false
	}
	// Approval notifications are control-plane projections for a human reviewer,
	// not new work for the owning Agent. Scheduling them would let an approval
	// request recursively trigger another governed action and another approval.
	if message.Sender.Type == models.ConversationParticipantService && message.Sender.ID == "approval-coordinator" {
		return false
	}
	// Connector review requests project a pending approval or setup request.
	if message.Sender.Type == models.ConversationParticipantService && message.Sender.ID == ChannelReviewParticipantID {
		return false
	}
	// Saved-artifact receipts project completed work; they are not new tasks.
	if message.Sender.Type == models.ConversationParticipantService && message.Sender.ID == runtimeconversation.ArtifactReceiptService {
		return false
	}
	// The owning Agent's reply is the projection of the current conversation
	// Run, never a new wake. Other Agents may still hand work into this channel.
	if conversation.Owner.Type == models.OwnerTypeAgent && message.Sender.Type == models.ConversationParticipantAgent &&
		message.Sender.ID == conversation.Owner.ID {
		return false
	}
	// In a Team channel a member's answer is a new wake only when it hands
	// off: it @mentions a teammate. Anything else would route forever.
	if conversation.Owner.Type == models.OwnerTypeTeam && message.Sender.Type == models.ConversationParticipantAgent {
		for _, mention := range message.Mentions {
			if mention.Type == models.ConversationParticipantAgent && mention != message.Sender {
				return true
			}
		}
		return false
	}
	switch message.Sender.Type {
	case models.ConversationParticipantUser, models.ConversationParticipantAgent, models.ConversationParticipantService:
		return true
	default:
		return false
	}
}
