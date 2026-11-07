package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/presentation"
)

const (
	conversationRunContextConversationID = "conversationId"
	conversationRunContextTriggerID      = "triggerMessageId"
	conversationRunContextTriggerSeq     = "triggerSequence"
	conversationRunSchedulerParticipant  = "conversation-run-scheduler"
)

const (
	governedActionProposalFailedStatus = "proposal_failed"
)

type ConversationRunSchedulerConfig struct {
	ConversationPageSize int
	MessagePageSize      int
	// RequireParticipationOptIn leaves unconfigured channels inert. Existing
	// embedding hosts retain their prior scheduling policy when false.
	RequireParticipationOptIn bool
	// Budget is host-owned and copied onto each newly scheduled Run.
	Budget *BudgetPolicy
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
	if c.Budget != nil {
		if err := c.Budget.Validate(); err != nil {
			return ConversationRunSchedulerConfig{}, err
		}
		c.Budget = cloneBudgetPolicy(c.Budget)
	}
	return c, nil
}

type ConversationRunReconcileResult struct {
	Conversations int `json:"conversations"`
	Messages      int `json:"messages"`
	Scheduled     int `json:"scheduled"`
	Replayed      int `json:"replayed"`
	Skipped       int `json:"skipped"`
	Results       int `json:"results"`
}

// ConversationRunScheduler projects durable channel messages into canonical
// Runs. Message identity is the durable wake fact: immediate scheduling is a
// latency optimization, while ReconcileScope closes the crash gap by advancing
// a service cursor only after each page has been scheduled idempotently.
type ConversationRunScheduler struct {
	conversations  *ConversationService
	runs           *RunCommandService
	approvals      *ApprovalCoordinator
	config         ConversationRunSchedulerConfig
	sessionContext ConversationSessionContextResolver
}

type ConversationSessionContextResolver interface {
	ResolveConversationSessionContext(context.Context, Scope, string) (map[string]interface{}, error)
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
		approvals:     NewApprovalCoordinator(runStore, runStore, nil),
		config:        normalized,
	}, nil
}

func (s *ConversationRunScheduler) SetSessionContextResolver(resolver ConversationSessionContextResolver) {
	if s != nil {
		s.sessionContext = resolver
	}
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
	if !conversationMessageStartsRun(conversation, message) || (s.config.RequireParticipationOptIn && !conversationParticipationAllows(conversation, message)) {
		return nil, false, nil
	}
	request, err := s.conversationAgentRunRequest(ctx, conversation, message)
	if err != nil {
		return nil, false, err
	}
	current, err := s.runs.store.GetAgentRun(ctx, conversation.Scope, runIDForIdempotencyKey(conversation.Scope, request.IdempotencyKey))
	if err != nil {
		return nil, false, err
	}
	if current != nil {
		// Existing imported runs retain their original receipt and execution key.
		// Replaying an event must not recreate or cancel already accepted work.
		if externalChannelContext(conversation) {
			return &AgentRunCommandResult{Run: current}, true, nil
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

func (s *ConversationRunScheduler) scheduleLoadedMessage(
	ctx context.Context,
	conversation *Conversation,
	message *ChannelMessage,
) (*AgentRunCommandResult, bool, error) {
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

// conversationMessageInitiatingUser resolves only the canonical scoped human
// identity. A host-owned voice-call start may name its authenticated initiator;
// arbitrary service metadata and copied Run context never grant that authority.
func conversationMessageInitiatingUser(conversation *Conversation, message *ChannelMessage) (ConversationParticipant, bool) {
	if conversation == nil || message == nil || message.Scope != conversation.Scope || message.ConversationID != conversation.ID {
		return ConversationParticipant{}, false
	}
	actor := message.Sender
	if voiceCallStartedMessage(message) && message.Initiator != nil {
		actor = *message.Initiator
	}
	return actor, actor.Type == ConversationParticipantUser && actor.Validate() == nil
}

func (s *ConversationRunScheduler) conversationAgentRunRequest(ctx context.Context, conversation *Conversation, message *ChannelMessage) (CreateAgentRunRequest, error) {
	request := conversationAgentRunRequest(conversation, message)
	request.Budget = cloneBudgetPolicy(s.config.Budget)
	if s.sessionContext == nil {
		return request, nil
	}
	session, err := s.sessionContext.ResolveConversationSessionContext(ctx, conversation.Scope, conversation.ID)
	if err != nil {
		return CreateAgentRunRequest{}, err
	}
	if session != nil {
		if err := ValidateCredentialFreeContext(session); err != nil {
			return CreateAgentRunRequest{}, err
		}
		request.Context[RunContextSessionKey] = session
	}
	return request, nil
}

func conversationAgentRunRequest(conversation *Conversation, message *ChannelMessage) CreateAgentRunRequest {
	goal := teamRoutingRunGoal
	assignedAgentID := ""
	visibility := ActivityVisibilityTeam
	if conversation.Owner.Type == OwnerTypeAgent {
		goal = "Respond to an Agent channel message"
		assignedAgentID = conversation.Owner.ID
		visibility = ActivityVisibilityPrivate
	}
	request := CreateAgentRunRequest{
		Scope: conversation.Scope, Kind: RunKindConversation, Owner: conversation.Owner,
		AssignedAgentID: assignedAgentID, ActingUser: MessageActingUser(conversation, message),
		ConcurrencyKey: conversationRunLane(conversation.ID, "", assignedAgentID), Goal: goal, Source: RunSourceChat,
		Context: map[string]interface{}{
			conversationRunContextConversationID: conversation.ID,
			conversationRunContextTriggerID:      message.ID,
			conversationRunContextTriggerSeq:     message.Sequence,
		},
		IdempotencyKey: conversationRunIdempotencyKey(conversation.Scope, conversation.ID, message.ID),
		Actor:          ActivityActor{Type: "service", ID: conversationRunSchedulerParticipant},
		Visibility:     visibility,
	}
	if root := externalConversationThreadRoot(conversation, message); root != "" {
		request.Context["threadRootMessageId"] = root
		request.ConcurrencyKey = conversationRunLane(conversation.ID, root, assignedAgentID)
	}
	if message.ResponseMode != "" {
		request.Context["responseMode"] = message.ResponseMode
	}
	if voiceCallStartedMessage(message) {
		request.Context["voiceCallStarted"] = true
	}
	if assignedAgentID != "" {
		request.SessionID = conversationSessionID(conversation.Scope, request.ConcurrencyKey)
	}
	return request
}

func (s *ConversationRunScheduler) ReconcileScope(ctx context.Context, scope Scope) (*ConversationRunReconcileResult, error) {
	if s == nil || s.conversations == nil || s.runs == nil {
		return nil, errors.New("conversation run scheduler is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	result := &ConversationRunReconcileResult{}
	if err := s.reconcileSkillSetupTasks(ctx, scope, result); err != nil {
		return result, err
	}
	if err := s.reconcileConversationQuestions(ctx, scope, result); err != nil {
		return result, err
	}

	for offset := 0; ; offset += s.config.ConversationPageSize {
		conversations, err := s.conversations.ListConversations(ctx, ConversationFilter{
			Scope: scope, Statuses: []ConversationStatus{ConversationStatusActive},
			Limit: s.config.ConversationPageSize, Offset: offset,
		})
		if err != nil {
			return result, err
		}
		for _, conversation := range conversations {
			if conversation.Owner.Type != OwnerTypeTeam && conversation.Owner.Type != OwnerTypeAgent {
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
func (s *ConversationRunScheduler) reconcileCanceledConversationRuns(ctx context.Context, scope Scope, result *ConversationRunReconcileResult) error {
	const pageSize = 100
	for offset := 0; ; offset += pageSize {
		runs, err := s.runs.store.ListAgentRuns(ctx, AgentRunFilter{
			Scope: scope, Kind: RunKindConversation, Statuses: []AgentRunStatus{AgentRunStatusCanceled}, Limit: pageSize, Offset: offset,
		})
		if err != nil {
			return err
		}
		for _, run := range runs {
			if run == nil {
				continue
			}
			conversationID, _ := run.Context[conversationRunContextConversationID].(string)
			triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
			if !validOpaqueIdentifier(conversationID, 128) || !validOpaqueIdentifier(triggerID, 128) {
				continue
			}
			conversation, err := s.conversations.GetConversation(ctx, scope, conversationID)
			if err != nil {
				return err
			}
			if conversation.Status != ConversationStatusActive || conversation.Owner != run.Owner {
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
			references := []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}}
			approvals, err := s.runs.store.ListApprovals(ctx, ApprovalFilter{Scope: scope, RunID: run.ID, Status: []ApprovalStatus{ApprovalStatusCanceled}, Limit: 1})
			if err != nil {
				return err
			}
			if len(approvals) == 1 {
				content = "The proposed action was canceled before execution. No changes were applied."
				references = append(references, ConversationReference{Kind: ConversationReferenceApproval, ID: approvals[0].ID})
			}
			current, err := s.conversations.GetConversation(ctx, scope, conversationID)
			if err != nil {
				return err
			}
			posted, err := s.conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
				Scope: scope, ConversationID: conversationID, ExpectedRevision: current.Revision,
				Sender: ConversationParticipant{Type: ConversationParticipantService, ID: "openseal.conversation"},
				Intent: MessageIntentSystem, Content: content, Audience: ConversationAudience{Kind: ConversationAudienceChannel},
				ReplyToMessageID: triggerID, References: references, ResolvesMessageID: triggerID,
				IdempotencyKey: "conversation-run-canceled-result:" + run.ID,
			})
			if errors.Is(err, ErrRevisionConflict) {
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

func (s *ConversationRunScheduler) conversationTriggerResolved(ctx context.Context, scope Scope, conversationID, triggerID string) (bool, error) {
	const pageSize = 100
	for after := int64(0); ; {
		messages, err := s.conversations.ListChannelMessages(ctx, ChannelMessageFilter{
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

func (s *ConversationRunScheduler) reconcileConversation(
	ctx context.Context,
	conversation *Conversation,
	result *ConversationRunReconcileResult,
) error {
	participant := ConversationParticipant{Type: ConversationParticipantService, ID: conversationRunSchedulerParticipant}
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
		(conversation.Owner.Type != OwnerTypeTeam && conversation.Owner.Type != OwnerTypeAgent) ||
		message.ConversationID != conversation.ID || message.Scope != conversation.Scope {
		return false
	}
	if message.Historical || message.Intent == MessageIntentSystem || message.ParticipationRoundID != "" {
		return false
	}
	// Approval notifications are control-plane projections for a human reviewer,
	// not new work for the owning Agent. Scheduling them would let an approval
	// request recursively trigger another governed action and another approval.
	if message.Sender.Type == ConversationParticipantService && message.Sender.ID == "approval-coordinator" {
		return false
	}
	// Connector review requests project a pending approval or setup request.
	if message.Sender.Type == ConversationParticipantService && message.Sender.ID == ChannelReviewParticipantID {
		return false
	}
	// Saved-artifact receipts project completed work; they are not new tasks.
	if message.Sender.Type == ConversationParticipantService && message.Sender.ID == conversationArtifactReceiptService {
		return false
	}
	// The owning Agent's reply is the projection of the current conversation
	// Run, never a new wake. Other Agents may still hand work into this channel.
	if conversation.Owner.Type == OwnerTypeAgent && message.Sender.Type == ConversationParticipantAgent &&
		message.Sender.ID == conversation.Owner.ID {
		return false
	}
	// In a Team channel a member's answer is a new wake only when it hands
	// off: it @mentions a teammate. Anything else would route forever.
	if conversation.Owner.Type == OwnerTypeTeam && message.Sender.Type == ConversationParticipantAgent {
		for _, mention := range message.Mentions {
			if mention.Type == ConversationParticipantAgent && mention != message.Sender {
				return true
			}
		}
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
	RequireParticipationOptIn bool
	MaximumRetries            int
	InitialRetryDelay         time.Duration
	MaximumRetryDelay         time.Duration
	// AgentTurns resolves the active prompt-first Agent definition and its
	// authorized Skills. It runs every Agent turn: Agent-owned channels and
	// each Team member's child Run, which answers exactly like a 1:1 Agent.
	AgentTurns TurnRunnerResolver
	// AttachmentContent supplies tenant-scoped bytes for user-attached text files
	// and stores the visuals Agents attach to their answers. Attachment content
	// is projected into the ephemeral model goal, never message state.
	AttachmentContent ArtifactContentStore
	// VisualChecks supplies the binding, link, image and place checks for
	// visuals Team members attach to their answers (places are then stamped
	// with its PlaceProvenance). The run it gets is the Team's
	// conversation Run with AssignedAgentID set to the answering member, so
	// the checks know which Agent published. Nil rejects bound or linked
	// visuals and visuals with images.
	VisualChecks func(run *AgentRun) presentation.ValidateOptions
	// HumanParticipants lists the people in a conversation. The card state
	// note uses it to say how many of them voted in a poll ("4 of 5
	// voted"); without it the note counts only the votes ("4 voted").
	HumanParticipants func(ctx context.Context, conversation *Conversation) ([]ConversationParticipant, error)
	// ImageStyle returns the picture style saved by the person this chat
	// turn answers (the sender of the run's trigger message, Context
	// "triggerMessageId"; the chat's owner when no person started it), or ""
	// when none is saved. A non-empty style reaches the Agent as imageStyle
	// in its turn context. Nil leaves it out.
	ImageStyle func(ctx context.Context, run *AgentRun) string
	// Places returns the places find_place already found in the run's chat,
	// most recent first. They reach the Agent as places in its turn context
	// (at most MaximumConversationPlaces) so it reuses their ids instead of
	// searching again. Nil leaves them out.
	Places func(ctx context.Context, run *AgentRun) []ConversationPlace
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

func (c ConversationRunTurnRunnerConfig) normalize() (ConversationRunTurnRunnerConfig, error) {
	if c.InitialRetryDelay == 0 {
		c.InitialRetryDelay = time.Second
	}
	if c.MaximumRetryDelay == 0 {
		c.MaximumRetryDelay = time.Minute
	}
	if c.MaximumRetries < 0 || c.MaximumRetries > 100 || c.InitialRetryDelay <= 0 ||
		c.MaximumRetryDelay < c.InitialRetryDelay || c.MaximumRetryDelay > time.Hour {
		return ConversationRunTurnRunnerConfig{}, fmt.Errorf("%w: invalid conversation Run retry configuration", ErrInvalidConversation)
	}
	return c, nil
}

// ConversationRunTurnRunner binds conversation Runs to the run loop. An Agent
// channel's Run is the Agent's own loop on its lane session. A Team channel's
// Run only routes: it records who answers and starts one member Run per
// responder, and each member answers on its own lane session exactly like a
// 1:1 chat.
type ConversationRunTurnRunner struct {
	conversations *ConversationService
	portfolio     PortfolioStore
	runbooks      RunbookActivationStore
	artifacts     ArtifactStore
	coordinator   *ConversationCoordinator
	config        ConversationRunTurnRunnerConfig
	agentTurns    TurnRunnerResolver
	runs          *RunCommandService
	// The stores of the conversation loop seams (ConversationLoopStores),
	// taken from the conversation store when it provides them.
	sessions AgentSessionStore
	actions  conversationLoopActions
	activity activityAppender
	now      func() time.Time
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
	runner := &ConversationRunTurnRunner{
		conversations: NewConversationService(conversationStore), coordinator: coordinator,
		config: normalized, agentTurns: normalized.AgentTurns, now: time.Now,
	}
	if runs, ok := conversationStore.(RunCommandStore); ok {
		runner.runs = NewRunCommandService(runs)
	}
	if portfolio, ok := conversationStore.(PortfolioStore); ok {
		runner.portfolio = portfolio
	}
	if runbooks, ok := conversationStore.(RunbookActivationStore); ok {
		runner.runbooks = runbooks
	}
	if artifacts, ok := conversationStore.(ArtifactStore); ok {
		runner.artifacts = artifacts
	}
	if sessions, ok := conversationStore.(AgentSessionStore); ok {
		runner.sessions = sessions
	}
	if actions, ok := conversationStore.(conversationLoopActions); ok {
		runner.actions = actions
	}
	if activity, ok := conversationStore.(activityAppender); ok {
		runner.activity = activity
	}
	return runner, nil
}

// conversationRunHasActingAgent reports whether a validated conversation Run
// speaks as an Agent. Only a Team chat's routing Run has none.
func conversationRunHasActingAgent(run *AgentRun) bool {
	return run != nil && strings.TrimSpace(run.AssignedAgentID) != ""
}

// conversationRosterMember proves the Run's exact acting Agent may still speak
// in its chat: the owner of an Agent's chat, or a current, authorized member
// of a Team's. Attribution is kernel-owned and rechecked before resolving an
// Agent model.
func (r *ConversationRunTurnRunner) conversationRosterMember(ctx context.Context, run *AgentRun) error {
	participant := run.AssignedAgentID
	if !validOpaqueIdentifier(participant, 256) || strings.TrimSpace(participant) != participant {
		return errors.New("failed action is missing Agent attribution")
	}
	switch run.Owner.Type {
	case OwnerTypeAgent:
		if participant != run.Owner.ID {
			return ErrInvalidAgentRun
		}
		return nil
	case OwnerTypeTeam:
	default:
		return ErrInvalidAgentRun
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
	conversation, err := r.conversations.GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return err
	}
	if conversation.Scope != run.Scope || conversation.Owner != run.Owner {
		return ErrInvalidAgentRun
	}
	trigger, err := r.conversations.GetChannelMessage(ctx, run.Scope, conversationID, triggerID)
	if err != nil {
		return err
	}
	bindings, err := r.coordinator.participants.ResolveConversationParticipants(ctx, ConversationParticipantQuery{Conversation: cloneConversation(conversation), Trigger: cloneChannelMessage(trigger)})
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		if binding.Participant.Type == ConversationParticipantAgent && binding.Participant.ID == participant {
			return binding.Validate()
		}
	}
	return errors.New("failed Team action Agent is no longer authorized in this conversation")
}

// ResolveTurnRunner binds a conversation Run to the run loop (run-loop
// section 05 §13, SPEC §2.4). In order:
//  1. the Run's envelope is validated;
//  2. a Run whose message is outside the channel's participation opt-in gets
//     a producer that ends it at once with the participationSkipped output;
//  3. a pinned runbook Run (Plan "runbook") gets the Agent's runbook
//     producer, with no conversation seams;
//  4. an Agent chat Run (an Agent channel's Run or a Team member's, also when
//     it only explains a failure) gets the Agent's own loop binding with the
//     conversation seams on top (bindConversationLoop);
//  5. a Team channel's routing Run gets the routing producer: no session, no
//     model, one step that starts the member Runs.
func (r *ConversationRunTurnRunner) ResolveTurnRunner(ctx context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
	if r == nil || r.coordinator == nil || r.conversations == nil {
		return nil, ErrConversationCoordinationUnavailable
	}
	if err := r.validateConversationRun(ctx, run); err != nil {
		return nil, err
	}
	if stopped, err := r.participationStopped(ctx, run); err != nil {
		return nil, err
	} else if stopped {
		return &TurnRunnerBinding{
			DefinitionID: "openseal.conversation-coordinator", DefinitionVersion: "1", ModelProvider: "host", Model: "participation-disabled",
			Producer: conversationOutputProducer{output: participationStoppedOutput()},
		}, nil
	}
	pinnedRunbook := run.Plan != nil && run.Plan["runbook"] != nil
	if !pinnedRunbook && !conversationRunHasActingAgent(run) && !runExplainingFailure(run) {
		return &TurnRunnerBinding{
			DefinitionID: "openseal.conversation-router", DefinitionVersion: "1", ModelProvider: "host", Model: "conversation-router",
			InputContextRefs: []string{conversationRunContextConversationID, conversationRunContextTriggerID},
			Producer:         teamRoutingStepProducer{runner: r},
		}, nil
	}
	if r.agentTurns == nil {
		return nil, ErrConversationCoordinationUnavailable
	}
	if runExplainingFailure(run) {
		if err := r.conversationRosterMember(ctx, run); err != nil {
			return nil, err
		}
	}
	binding, err := r.agentTurns.ResolveTurnRunner(ctx, cloneAgentRun(run))
	if err != nil {
		return nil, err
	}
	if binding == nil || binding.Producer == nil {
		return nil, ErrConversationCoordinationUnavailable
	}
	if pinnedRunbook {
		return binding, nil
	}
	binding.ModelActions = constrainEmbedSessionActions(run, binding.ModelActions)
	if err := r.bindConversationLoop(ctx, run, binding, ConversationLoopStores{Sessions: r.sessions, Actions: r.actions, Activity: r.activity}); err != nil {
		return nil, err
	}
	return binding, nil
}

// conversationOutputProducer ends a conversation Run in its first step with a
// fixed output, without a model or tool call.
type conversationOutputProducer struct {
	output map[string]interface{}
}

func (conversationOutputProducer) Kind() StepProducerKind { return StepProducerDeterministic }

func (p conversationOutputProducer) Produce(context.Context, StepInput) (StepOutput, error) {
	return StepOutput{Final: &LoopFinal{Status: AgentRunStatusCompleted, Output: cloneMap(p.output)}}, nil
}

// teamRoutingStepProducer is a Team channel's routing Run (section 05 §2): one
// deterministic step that records who answers the message, starts one member
// Run per responder and completes with their ids. It has no session and makes
// no tool calls; the members answer in their own lane sessions.
type teamRoutingStepProducer struct {
	runner *ConversationRunTurnRunner
}

func (teamRoutingStepProducer) Kind() StepProducerKind { return StepProducerDeterministic }

func (p teamRoutingStepProducer) Produce(ctx context.Context, in StepInput) (StepOutput, error) {
	run := in.Run
	if err := p.runner.validateConversationRun(ctx, run); err != nil {
		return StepOutput{}, err
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
	conversation, err := p.runner.conversations.GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return StepOutput{}, err
	}
	if conversation.Owner != run.Owner {
		return StepOutput{}, fmt.Errorf("%w: conversation Run owner does not match its channel", ErrInvalidAgentRun)
	}
	output, err := p.runner.routeTeamMessage(ctx, run, conversation, triggerID)
	if err != nil {
		return StepOutput{}, err
	}
	return StepOutput{Final: &LoopFinal{Status: AgentRunStatusCompleted, Output: output}}, nil
}

func (r *ConversationRunTurnRunner) agentConversationTurnContext(ctx context.Context, input TurnExecutionContext, conversation *Conversation, triggerID string) (string, ConversationViewer, *ChannelMessage, error) {
	participantID := input.Run.AssignedAgentID
	if runExplainingFailure(input.Run) {
		if err := r.conversationRosterMember(ctx, input.Run); err != nil {
			return "", ConversationViewer{}, nil, err
		}
	}
	if participantID == "" {
		return "", ConversationViewer{}, nil, fmt.Errorf("%w: conversation Run has no acting Agent", ErrInvalidAgentRun)
	}
	viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: participantID}}
	trigger, err := r.conversations.GetVisibleChannelMessage(ctx, input.Run.Scope, conversation.ID, triggerID, viewer)
	if err != nil {
		return "", viewer, nil, err
	}
	if root, _ := input.Run.Context["threadRootMessageId"].(string); root != "" && root != externalConversationThreadRoot(conversation, trigger) {
		return "", viewer, nil, fmt.Errorf("%w: thread does not match the canonical trigger", ErrInvalidAgentRun)
	}
	return participantID, viewer, trigger, nil
}

type agentConversationActiveRun struct {
	ID                string         `json:"id"`
	RootRunID         string         `json:"rootRunId"`
	Entrypoint        string         `json:"entrypoint,omitempty"`
	Status            AgentRunStatus `json:"status"`
	Goal              string         `json:"goal"`
	Source            RunSource      `json:"source"`
	Revision          int64          `json:"revision"`
	AvailableControls []string       `json:"availableControls,omitempty"`
}

func (r *ConversationRunTurnRunner) activeConversationRuns(ctx context.Context, conversation *Conversation, triggers ...*ChannelMessage) ([]agentConversationActiveRun, error) {
	if r == nil || r.portfolio == nil || conversation == nil {
		return nil, nil
	}
	var runs []*AgentRun
	var err error
	indexed := false
	if store, ok := r.portfolio.(ConversationActiveRunsReadStore); ok {
		runs, err = store.ListConversationActiveRuns(ctx, conversation.Scope, conversation.Owner, conversation.ID, 100)
		indexed = true
	} else {
		runs, err = r.portfolio.ListAgentRuns(ctx, AgentRunFilter{
			Scope: conversation.Scope, Owner: &conversation.Owner, Order: AgentRunOrderCreatedDesc, Limit: 100,
		})
	}
	if err != nil {
		return nil, err
	}
	relevantRoots := make(map[string]bool)
	if !indexed {
		for _, run := range runs {
			if run == nil || run.Kind != RunKindConversation {
				continue
			}
			conversationID, _ := run.Context[conversationRunContextConversationID].(string)
			if strings.TrimSpace(conversationID) == conversation.ID {
				relevantRoots[run.ID] = true
			}
		}
	}
	active := make([]agentConversationActiveRun, 0)
	for _, run := range runs {
		if run == nil || isTerminalAgentRunStatus(run.Status) || !indexed && !relevantRoots[run.RootRunID] || run.Kind == RunKindConversation {
			continue
		}
		active = append(active, agentConversationActiveRun{
			ID: run.ID, RootRunID: run.RootRunID, Entrypoint: run.Entrypoint, Status: run.Status, Goal: run.Goal, Source: run.Source,
			Revision: run.Revision, AvailableControls: applicableAgentRunCommands(run),
		})
	}
	if len(triggers) == 0 || triggers[0] == nil {
		return active, nil
	}
	trigger := triggers[0]
	if trigger.Scope != conversation.Scope || trigger.ConversationID != conversation.ID {
		return nil, ErrInvalidConversation
	}
	actor := trigger.Sender
	if voiceCallStartedMessage(trigger) && trigger.Initiator != nil {
		actor = *trigger.Initiator
	}
	if actor.Type != ConversationParticipantUser || actor.Validate() != nil {
		return active, nil
	}
	tasks, ok := r.portfolio.(ConversationTaskStore)
	if !ok {
		return active, nil
	}
	threadID := externalConversationThreadRoot(conversation, trigger)
	results, err := tasks.ListConversationTasks(ctx, ConversationTaskFilter{
		Scope: conversation.Scope, Owner: conversation.Owner, ConversationID: conversation.ID,
		ThreadRootID: threadID, AuthenticatedActor: actor, ActiveOnly: true, Limit: 10,
	})
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(active))
	for _, run := range active {
		seen[run.ID] = true
	}
	for _, result := range results {
		if result == nil || result.Task == nil || result.WorkRun == nil {
			continue
		}
		task, work := result.Task, result.WorkRun
		if task.Scope != conversation.Scope || task.Owner != conversation.Owner || task.ConversationID != conversation.ID ||
			threadID != "" && task.ThreadRootID != threadID || task.AuthenticatedActor != actor ||
			!ConversationTaskMatchesWorkRun(task, work) || isTerminalAgentRunStatus(work.Status) || seen[work.ID] {
			continue
		}
		if task.Mode == ConversationTaskModeContinuation {
			proof, ok := r.portfolio.(ConversationTaskProofStore)
			if !ok {
				continue
			}
			verified, err := VerifyConversationTaskWorkRun(ctx, proof, task, work)
			if err != nil {
				return nil, err
			}
			if !verified {
				continue
			}
		}
		active = append(active, agentConversationActiveRun{
			ID: work.ID, RootRunID: work.RootRunID, Entrypoint: work.Entrypoint, Status: work.Status, Goal: ConversationTaskGoalSummary(work.Goal), Source: work.Source,
			Revision: work.Revision, AvailableControls: applicableAgentRunCommands(work),
		})
		seen[work.ID] = true
	}
	return active, nil
}

func (r *ConversationRunTurnRunner) postAgentResponseWithReferences(
	ctx context.Context,
	run *AgentRun,
	conversation *Conversation,
	trigger *ChannelMessage,
	content string,
	references []ConversationReference,
	broadcastToChannel bool,
	guard *ReplyRunGuard,
) (*ChannelMessage, bool, error) {
	key := conversationRunResponseKey(run, trigger.ID)
	for range 3 {
		current, err := r.conversations.GetConversation(ctx, run.Scope, conversation.ID)
		if err != nil {
			return nil, false, err
		}
		if r.config.RequireParticipationOptIn && !conversationParticipationAllows(current, trigger) {
			return nil, false, errConversationParticipationStopped
		}
		responseReferences, err := r.conversationTaskResponseReferences(ctx, run, references)
		if err != nil {
			return nil, false, err
		}
		participantID := run.AssignedAgentID
		if participantID == "" {
			return nil, false, fmt.Errorf("%w: conversation Run has no acting Agent", ErrInvalidAgentRun)
		}
		var routingReason string
		var mentions []ConversationParticipant
		if isTeamMemberRun(run) {
			routingReason, _ = run.Context[routingReasonContextKey].(string)
			// One hop: an answer to a teammate's handoff never hands off again.
			if trigger.Sender.Type != ConversationParticipantAgent {
				if mentions, err = r.handoffMentions(ctx, current, trigger, participantID, content); err != nil {
					return nil, false, err
				}
			}
		}
		request := PostChannelMessageRequest{
			Scope: run.Scope, ConversationID: current.ID, ExpectedRevision: current.Revision,
			Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: participantID},
			Intent: MessageIntentAnswer, Content: content, Audience: ConversationAudience{Kind: ConversationAudienceChannel},
			ReplyToMessageID: trigger.ID, ResolvesMessageID: trigger.ID,
			ResponseMode:       trigger.ResponseMode,
			BroadcastToChannel: broadcastToChannel,
			Mentions:           mentions,
			References:         responseReferences,
			RoutingReason:      routingReason,
			IdempotencyKey:     key,
			RunGuard:           guard,
		}
		result, err := r.conversations.PostChannelMessage(ctx, request)
		if err == nil {
			return result.Message, result.Replayed, nil
		}
		if errors.Is(err, ErrMessageConflict) {
			if saved, replayErr := r.conversations.replayLegacySpokenResponse(ctx, request); replayErr != nil {
				return nil, false, replayErr
			} else if saved != nil {
				return saved, true, nil
			}
		}
		if !errors.Is(err, ErrRevisionConflict) && !errors.Is(err, ErrMessageConflict) {
			return nil, false, err
		}
	}
	return nil, false, ErrRevisionConflict
}

func conversationResultMap(value interface{}) map[string]interface{} {
	if result, ok := value.(map[string]interface{}); ok {
		return result
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var result map[string]interface{}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil
	}
	return result
}

func conversationResultString(value map[string]interface{}, key string) string {
	if value == nil {
		return ""
	}
	result, ok := value[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(result)
}

// conversationRetryError is a routing failure as the Run reports it.
// Coordination failures are terminal. Retriability is intentionally disabled
// until a specific error can be opted in under a reviewed policy.
func conversationRetryError(cause error) error {
	return errors.New(publicConversationRetryReason(cause))
}

func (r *ConversationRunTurnRunner) validateConversationRun(ctx context.Context, run *AgentRun) error {
	if run == nil {
		return ErrInvalidAgentRun
	}
	if taskID, _ := run.Context[ConversationTaskContextKey].(string); taskID != "" {
		task, err := canonicalConversationTaskContinuation(ctx, r.portfolio, run)
		if err != nil {
			return err
		}
		if task == nil {
			return ErrInvalidConversationTask
		}
		// Reuse the ordinary envelope validation only after proving the new
		// lane against the canonical Task ledger.
		run = cloneAgentRun(run)
		root, _ := run.Context["threadRootMessageId"].(string)
		run.ConcurrencyKey = conversationRunLane(task.ConversationID, root, run.AssignedAgentID)
	}
	return validateConversationRun(run)
}

func (r *ConversationRunTurnRunner) conversationTaskResponseReferences(ctx context.Context, run *AgentRun, references []ConversationReference) ([]ConversationReference, error) {
	result := append([]ConversationReference(nil), references...)
	if r.portfolio == nil {
		return result, nil
	}
	// The provider may have started before promotion. Reload the current Run
	// while publishing its response so the original invocation gains Task refs.
	current, err := r.portfolio.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		if taskID, _ := run.Context[ConversationTaskContextKey].(string); taskID != "" || strings.HasPrefix(run.ConcurrencyKey, "task:") {
			return nil, ErrRunNotFound
		}
		return result, nil
	}
	task, err := canonicalConversationTaskContinuation(ctx, r.portfolio, current)
	if err != nil || task == nil {
		return result, err
	}
	for _, reference := range []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}, {Kind: ConversationReferenceTask, ID: task.ID}} {
		found := false
		for _, existing := range result {
			if existing.Kind == reference.Kind && existing.ID == reference.ID {
				found = true
				break
			}
		}
		if !found {
			result = append(result, reference)
		}
	}
	return result, nil
}

// agentConversationAssignment is the work a teammate handed over in the chat.
type agentConversationAssignment struct {
	From string `json:"fromAgentId,omitempty"`
	Kind string `json:"kind"`
	Work string `json:"work"`
}

func conversationRunAssignment(run *AgentRun) *agentConversationAssignment {
	if run == nil || run.Source != RunSourceRequest && run.Source != RunSourceHandoff && run.Source != RunSourceEscalation || strings.TrimSpace(run.Goal) == "" {
		return nil
	}
	from, _ := run.Context[collaborationHandedOffByContextKey].(string)
	return &agentConversationAssignment{From: from, Kind: string(run.Source), Work: run.Goal}
}

func validateConversationRun(run *AgentRun) error {
	if run == nil || run.Kind != RunKindConversation || (run.Owner.Type != OwnerTypeTeam && run.Owner.Type != OwnerTypeAgent) {
		return fmt.Errorf("%w: Team- or Agent-owned conversation Run is required", ErrInvalidAgentRun)
	}
	if run.Owner.Type == OwnerTypeAgent && run.AssignedAgentID != run.Owner.ID {
		return fmt.Errorf("%w: Agent-owned conversation Run must be assigned to its owner", ErrInvalidAgentRun)
	}
	conversationID, conversationOK := run.Context[conversationRunContextConversationID].(string)
	triggerID, triggerOK := run.Context[conversationRunContextTriggerID].(string)
	root, _ := run.Context["threadRootMessageId"].(string)
	if root != "" && !validOpaqueIdentifier(root, 128) {
		return ErrInvalidAgentRun
	}
	reason, _ := run.Context[routingReasonContextKey].(string)
	if !conversationOK || !triggerOK || !validOpaqueIdentifier(conversationID, 128) || !validOpaqueIdentifier(triggerID, 128) ||
		run.ConcurrencyKey != conversationRunLane(conversationID, root, run.AssignedAgentID) || len(reason) > MaximumRoutingReasonBytes {
		return fmt.Errorf("%w: conversation Run context or concurrency key is invalid", ErrInvalidAgentRun)
	}
	return validateConversationRunSession(run)
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

func conversationParticipationAllows(conversation *Conversation, message *ChannelMessage) bool {
	return conversation != nil && message != nil && conversation.Status == ConversationStatusActive &&
		conversation.Participation != nil && conversation.Participation.Enabled && message.Sequence > conversation.Participation.AfterSequence
}

// Recheck the canonical channel at execution, not just scheduling. A queued Run
// must not make model calls after opt-out or after a later activation boundary.
func (r *ConversationRunTurnRunner) participationStopped(ctx context.Context, run *AgentRun) (bool, error) {
	if !r.config.RequireParticipationOptIn {
		return false, nil
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
	conversation, err := r.conversations.GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return false, err
	}
	if conversation.Owner != run.Owner {
		return false, fmt.Errorf("%w: conversation Run owner does not match its channel", ErrInvalidAgentRun)
	}
	trigger, err := r.conversations.GetChannelMessage(ctx, run.Scope, conversationID, triggerID)
	if err != nil {
		return false, err
	}
	return !conversationParticipationAllows(conversation, trigger), nil
}

var errConversationParticipationStopped = errors.New("channel participation is disabled for this message")

// participationStoppedOutput is the output of a Run whose message is outside
// the channel's participation opt-in. externalConversationRunReply posts its
// reply.
func participationStoppedOutput() map[string]interface{} {
	return map[string]interface{}{"participationSkipped": true, "reply": "No further participation will start for this message because it is outside the channel’s current participation opt-in. Previously started actions may still finish."}
}

// External provider messages never inherit the agent’s other private work.
func externalChannelContext(conversation *Conversation) bool {
	return conversation != nil && conversation.Origin != nil && conversation.Origin.Kind == ConversationReferenceExternalSource
}

// An explicit thread remains inside its conversation. Its root scopes model
// context and execution; unthreaded application chats have no such scope.
func externalConversationThreadRoot(conversation *Conversation, trigger *ChannelMessage) string {
	if trigger == nil {
		return ""
	}
	// Telegram DM reply IDs identify quoted messages, not separate conversations.
	// Keep real Telegram topics isolated, and retain Slack's thread semantics.
	if source := trigger.ExternalSource; externalChannelContext(conversation) && source != nil &&
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
	if externalChannelContext(conversation) {
		return trigger.ID
	}
	return ""
}
