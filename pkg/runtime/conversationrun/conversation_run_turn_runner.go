package conversationrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
)

type TurnRunnerConfig struct {
	RequireParticipationOptIn bool
	MaximumRetries            int
	InitialRetryDelay         time.Duration
	MaximumRetryDelay         time.Duration
	// AgentTurns resolves the active prompt-first Agent definition and its
	// authorized Skills. It runs every Agent turn: Agent-owned channels and
	// each Team member's child Run, which answers exactly like a 1:1 Agent.
	AgentTurns runtimeconversation.TurnRunnerResolver
	// AttachmentContent supplies tenant-scoped bytes for user-attached text files
	// and stores the visuals Agents attach to their answers. Attachment content
	// is projected into the ephemeral model goal, never message state.
	AttachmentContent models.ArtifactContentStore
	// VisualChecks supplies the binding, link, image and place checks for
	// visuals Team members attach to their answers (places are then stamped
	// with its PlaceProvenance). The run it gets is the Team's
	// conversation Run with AssignedAgentID set to the answering member, so
	// the checks know which Agent published. Nil rejects bound or linked
	// visuals and visuals with images.
	VisualChecks func(run *models.AgentRun) presentation.ValidateOptions
	// HumanParticipants lists the people in a conversation. The card state
	// note uses it to say how many of them voted in a poll ("4 of 5
	// voted"); without it the note counts only the votes ("4 voted").
	HumanParticipants func(ctx context.Context, conversation *models.Conversation) ([]models.ConversationParticipant, error)
	// ImageStyle returns the picture style saved by the person this chat
	// turn answers (the sender of the run's trigger message, Context
	// "triggerMessageId"; the chat's owner when no person started it), or ""
	// when none is saved. A non-empty style reaches the Agent as imageStyle
	// in its turn context. Nil leaves it out.
	ImageStyle func(ctx context.Context, run *models.AgentRun) string
	// WorkMachine says where this Run works on files and commands when the
	// person chose a machine other than the chat's secure VM, such as their own
	// computer: a sentence or two for the run_context note, "" for the VM.
	WorkMachine func(ctx context.Context, run *models.AgentRun) string
	// Places returns the places find_place already found in the run's chat,
	// most recent first. They reach the Agent as places in its turn context
	// (at most MaximumConversationPlaces) so it reuses their ids instead of
	// searching again. Nil leaves them out.
	Places func(ctx context.Context, run *models.AgentRun) []runtimeconversation.Place
}

func (c TurnRunnerConfig) normalize() (TurnRunnerConfig, error) {
	if c.InitialRetryDelay == 0 {
		c.InitialRetryDelay = time.Second
	}
	if c.MaximumRetryDelay == 0 {
		c.MaximumRetryDelay = time.Minute
	}
	if c.MaximumRetries < 0 || c.MaximumRetries > 100 || c.InitialRetryDelay <= 0 ||
		c.MaximumRetryDelay < c.InitialRetryDelay || c.MaximumRetryDelay > time.Hour {
		return TurnRunnerConfig{}, fmt.Errorf("%w: invalid conversation Run retry configuration", models.ErrInvalidConversation)
	}
	return c, nil
}

// TurnRunner binds conversation Runs to the run loop. An Agent
// channel's Run is the Agent's own loop on its lane session. A Team channel's
// Run only routes: it records who answers and starts one member Run per
// responder, and each member answers on its own lane session exactly like a
// 1:1 chat.
type TurnRunner struct {
	conversations *runtimeconversation.Service
	portfolio     models.PortfolioStore
	runbooks      models.RunbookActivationStore
	artifacts     models.ArtifactStore
	coordinator   *runtimeconversation.Coordinator
	config        TurnRunnerConfig
	agentTurns    runtimeconversation.TurnRunnerResolver
	runs          *agentrun.RunCommandService
	// The stores of the conversation loop seams (ConversationLoopStores),
	// taken from the conversation store when it provides them.
	sessions models.AgentSessionStore
	actions  conversationLoopActions
	activity activityAppender
	now      func() time.Time
	// conversationStore is the store behind conversations; runStore the one
	// behind runs (nil without runs).
	conversationStore models.ConversationStore
	runStore          models.RunCommandStore
}

func NewTurnRunner(
	conversationStore models.ConversationStore,
	coordinator *runtimeconversation.Coordinator,
	config TurnRunnerConfig,
	opts ...models.ServiceOption,
) (*TurnRunner, error) {
	if conversationStore == nil || coordinator == nil {
		return nil, runtimeconversation.ErrConversationCoordinationUnavailable
	}
	normalized, err := config.normalize()
	if err != nil {
		return nil, err
	}
	runner := &TurnRunner{
		conversations: runtimeconversation.NewService(conversationStore, opts...), conversationStore: conversationStore, coordinator: coordinator,
		config: normalized, agentTurns: normalized.AgentTurns, now: models.ClockFrom(opts),
	}
	if runs, ok := conversationStore.(models.RunCommandStore); ok {
		runner.runs = agentrun.NewRunCommandService(runs, opts...)
		runner.runStore = runs
	}
	if portfolio, ok := conversationStore.(models.PortfolioStore); ok {
		runner.portfolio = portfolio
	}
	if runbooks, ok := conversationStore.(models.RunbookActivationStore); ok {
		runner.runbooks = runbooks
	}
	if artifacts, ok := conversationStore.(models.ArtifactStore); ok {
		runner.artifacts = artifacts
	}
	if sessions, ok := conversationStore.(models.AgentSessionStore); ok {
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
func conversationRunHasActingAgent(run *models.AgentRun) bool {
	return run != nil && strings.TrimSpace(run.AssignedAgentID) != ""
}

// conversationRosterMember proves the Run's exact acting Agent may still speak
// in its chat: the owner of an Agent's chat, or a current, authorized member
// of a Team's. Attribution is kernel-owned and rechecked before resolving an
// Agent model.
func (r *TurnRunner) conversationRosterMember(ctx context.Context, run *models.AgentRun) error {
	participant := run.AssignedAgentID
	if !models.IsValidOpaqueIdentifier(participant, 256) || strings.TrimSpace(participant) != participant {
		return errors.New("failed action is missing Agent attribution")
	}
	switch run.Owner.Type {
	case models.OwnerTypeAgent:
		if participant != run.Owner.ID {
			return models.ErrInvalidAgentRun
		}
		return nil
	case models.OwnerTypeTeam:
	default:
		return models.ErrInvalidAgentRun
	}
	conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
	triggerID, _ := run.Context[models.ConversationRunTriggerIDContextKey].(string)
	conversation, err := r.conversations.GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return err
	}
	if conversation.Scope != run.Scope || conversation.Owner != run.Owner {
		return models.ErrInvalidAgentRun
	}
	trigger, err := r.conversations.GetChannelMessage(ctx, run.Scope, conversationID, triggerID)
	if err != nil {
		return err
	}
	bindings, err := r.coordinator.ResolveParticipants(ctx, runtimeconversation.ParticipantQuery{Conversation: models.CloneConversation(conversation), Trigger: models.CloneChannelMessage(trigger)})
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		if binding.Participant.Type == models.ConversationParticipantAgent && binding.Participant.ID == participant {
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
func (r *TurnRunner) ResolveTurnRunner(ctx context.Context, run *models.AgentRun) (*agentrun.TurnRunnerBinding, error) {
	if r == nil || r.coordinator == nil || r.conversations == nil {
		return nil, runtimeconversation.ErrConversationCoordinationUnavailable
	}
	if err := r.validateConversationRun(ctx, run); err != nil {
		return nil, err
	}
	if stopped, err := r.participationStopped(ctx, run); err != nil {
		return nil, err
	} else if stopped {
		return &agentrun.TurnRunnerBinding{
			DefinitionID: "openseal.conversation-coordinator", DefinitionVersion: "1", ModelProvider: "host", Model: "participation-disabled",
			Producer: conversationOutputProducer{output: participationStoppedOutput()},
		}, nil
	}
	pinnedRunbook := run.Plan != nil && run.Plan["runbook"] != nil
	if !pinnedRunbook && !conversationRunHasActingAgent(run) && !runtimeconversation.RunExplainingFailure(run) {
		return &agentrun.TurnRunnerBinding{
			DefinitionID: "openseal.conversation-router", DefinitionVersion: "1", ModelProvider: "host", Model: "conversation-router",
			InputContextRefs: []string{models.ConversationRunConversationIDContextKey, models.ConversationRunTriggerIDContextKey},
			Producer:         teamRoutingStepProducer{runner: r},
		}, nil
	}
	if r.agentTurns == nil {
		return nil, runtimeconversation.ErrConversationCoordinationUnavailable
	}
	if runtimeconversation.RunExplainingFailure(run) {
		if err := r.conversationRosterMember(ctx, run); err != nil {
			return nil, err
		}
	}
	binding, err := r.agentTurns.ResolveTurnRunner(ctx, models.CloneAgentRun(run))
	if err != nil {
		return nil, err
	}
	if binding == nil || binding.Producer == nil {
		return nil, runtimeconversation.ErrConversationCoordinationUnavailable
	}
	if pinnedRunbook {
		return binding, nil
	}
	binding.ModelActions = skillbinding.ConstrainEmbedSessionActions(run, binding.ModelActions)
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

func (conversationOutputProducer) Kind() models.StepProducerKind {
	return models.StepProducerDeterministic
}

func (p conversationOutputProducer) Produce(context.Context, agentrun.StepInput) (agentrun.StepOutput, error) {
	return agentrun.StepOutput{Final: &agentrun.LoopFinal{Status: models.AgentRunStatusCompleted, Output: maps.Clone(p.output)}}, nil
}

// teamRoutingStepProducer is a Team channel's routing Run (section 05 §2): one
// deterministic step that records who answers the message, starts one member
// Run per responder and completes with their ids. It has no session and makes
// no tool calls; the members answer in their own lane sessions.
type teamRoutingStepProducer struct {
	runner *TurnRunner
}

func (teamRoutingStepProducer) Kind() models.StepProducerKind {
	return models.StepProducerDeterministic
}

func (p teamRoutingStepProducer) Produce(ctx context.Context, in agentrun.StepInput) (agentrun.StepOutput, error) {
	run := in.Run
	if err := p.runner.validateConversationRun(ctx, run); err != nil {
		return agentrun.StepOutput{}, err
	}
	conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
	triggerID, _ := run.Context[models.ConversationRunTriggerIDContextKey].(string)
	conversation, err := p.runner.conversations.GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return agentrun.StepOutput{}, err
	}
	if conversation.Owner != run.Owner {
		return agentrun.StepOutput{}, fmt.Errorf("%w: conversation Run owner does not match its channel", models.ErrInvalidAgentRun)
	}
	output, err := p.runner.routeTeamMessage(ctx, run, conversation, triggerID)
	if err != nil {
		return agentrun.StepOutput{}, err
	}
	return agentrun.StepOutput{Final: &agentrun.LoopFinal{Status: models.AgentRunStatusCompleted, Output: output}}, nil
}

func (r *TurnRunner) agentConversationTurnContext(ctx context.Context, run *models.AgentRun, conversation *models.Conversation, triggerID string) (string, models.ConversationViewer, *models.ChannelMessage, error) {
	participantID := run.AssignedAgentID
	if runtimeconversation.RunExplainingFailure(run) {
		if err := r.conversationRosterMember(ctx, run); err != nil {
			return "", models.ConversationViewer{}, nil, err
		}
	}
	if participantID == "" {
		return "", models.ConversationViewer{}, nil, fmt.Errorf("%w: conversation Run has no acting Agent", models.ErrInvalidAgentRun)
	}
	viewer := models.ConversationViewer{Participant: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: participantID}}
	trigger, err := r.conversations.GetVisibleChannelMessage(ctx, run.Scope, conversation.ID, triggerID, viewer)
	if err != nil {
		return "", viewer, nil, err
	}
	if root, _ := run.Context["threadRootMessageId"].(string); root != "" && root != models.ExternalConversationThreadRoot(conversation, trigger) {
		return "", viewer, nil, fmt.Errorf("%w: thread does not match the canonical trigger", models.ErrInvalidAgentRun)
	}
	return participantID, viewer, trigger, nil
}

func (r *TurnRunner) activeConversationRuns(ctx context.Context, conversation *models.Conversation, triggers ...*models.ChannelMessage) ([]runtimeconversation.AgentConversationActiveRun, error) {
	if r == nil || r.portfolio == nil || conversation == nil {
		return nil, nil
	}
	var runs []*models.AgentRun
	var err error
	indexed := false
	if store, ok := r.portfolio.(models.ConversationActiveRunsReadStore); ok {
		runs, err = store.ListConversationActiveRuns(ctx, conversation.Scope, conversation.Owner, conversation.ID, 100)
		indexed = true
	} else {
		runs, err = r.portfolio.ListAgentRuns(ctx, models.AgentRunFilter{
			Scope: conversation.Scope, Owner: &conversation.Owner, Order: models.AgentRunOrderCreatedDesc, Limit: 100,
		})
	}
	if err != nil {
		return nil, err
	}
	relevantRoots := make(map[string]bool)
	if !indexed {
		for _, run := range runs {
			if run == nil || run.Kind != models.RunKindConversation {
				continue
			}
			conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
			if strings.TrimSpace(conversationID) == conversation.ID {
				relevantRoots[run.ID] = true
			}
		}
	}
	active := make([]runtimeconversation.AgentConversationActiveRun, 0)
	for _, run := range runs {
		if run == nil || models.IsTerminalAgentRunStatus(run.Status) || !indexed && !relevantRoots[run.RootRunID] || run.Kind == models.RunKindConversation {
			continue
		}
		active = append(active, runtimeconversation.AgentConversationActiveRun{
			ID: run.ID, RootRunID: run.RootRunID, Entrypoint: run.Entrypoint, Status: run.Status, Goal: run.Goal, Source: run.Source,
			Revision: run.Revision, AvailableControls: agentrun.ApplicableAgentRunCommands(run),
		})
	}
	if len(triggers) == 0 || triggers[0] == nil {
		return active, nil
	}
	trigger := triggers[0]
	if trigger.Scope != conversation.Scope || trigger.ConversationID != conversation.ID {
		return nil, models.ErrInvalidConversation
	}
	actor := trigger.Sender
	if models.VoiceCallStartedMessage(trigger) && trigger.Initiator != nil {
		actor = *trigger.Initiator
	}
	if actor.Type != models.ConversationParticipantUser || actor.Validate() != nil {
		return active, nil
	}
	tasks, ok := r.portfolio.(models.ConversationTaskStore)
	if !ok {
		return active, nil
	}
	threadID := models.ExternalConversationThreadRoot(conversation, trigger)
	results, err := tasks.ListConversationTasks(ctx, models.ConversationTaskFilter{
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
			!models.ConversationTaskMatchesWorkRun(task, work) || models.IsTerminalAgentRunStatus(work.Status) || seen[work.ID] {
			continue
		}
		if task.Mode == models.ConversationTaskModeContinuation {
			proof, ok := r.portfolio.(models.ConversationTaskProofStore)
			if !ok {
				continue
			}
			verified, err := agentrun.VerifyConversationTaskWorkRun(ctx, proof, task, work)
			if err != nil {
				return nil, err
			}
			if !verified {
				continue
			}
		}
		active = append(active, runtimeconversation.AgentConversationActiveRun{
			ID: work.ID, RootRunID: work.RootRunID, Entrypoint: work.Entrypoint, Status: work.Status, Goal: ConversationTaskGoalSummary(work.Goal), Source: work.Source,
			Revision: work.Revision, AvailableControls: agentrun.ApplicableAgentRunCommands(work),
		})
		seen[work.ID] = true
	}
	return active, nil
}

func (r *TurnRunner) postAgentResponseWithReferences(
	ctx context.Context,
	run *models.AgentRun,
	conversation *models.Conversation,
	trigger *models.ChannelMessage,
	content string,
	references []models.ConversationReference,
	broadcastToChannel bool,
	guard *models.ReplyRunGuard,
) (*models.ChannelMessage, bool, error) {
	key := models.ConversationRunResponseKey(run, trigger.ID)
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
			return nil, false, fmt.Errorf("%w: conversation Run has no acting Agent", models.ErrInvalidAgentRun)
		}
		var routingReason string
		var mentions []models.ConversationParticipant
		if models.IsTeamMemberRun(run) {
			routingReason, _ = run.Context[models.RoutingReasonContextKey].(string)
			// One hop: an answer to a teammate's handoff never hands off again.
			if trigger.Sender.Type != models.ConversationParticipantAgent {
				if mentions, err = r.handoffMentions(ctx, current, trigger, participantID, content); err != nil {
					return nil, false, err
				}
			}
		}
		request := runtimeconversation.PostChannelMessageRequest{
			Scope: run.Scope, ConversationID: current.ID, ExpectedRevision: current.Revision,
			Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: participantID},
			Intent: models.MessageIntentAnswer, Content: content, Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel},
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
		if errors.Is(err, models.ErrMessageConflict) {
			if saved, replayErr := r.conversations.ReplayLegacySpokenResponse(ctx, request); replayErr != nil {
				return nil, false, replayErr
			} else if saved != nil {
				return saved, true, nil
			}
		}
		if !errors.Is(err, models.ErrRevisionConflict) && !errors.Is(err, models.ErrMessageConflict) {
			return nil, false, err
		}
	}
	return nil, false, models.ErrRevisionConflict
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

// ConversationResultString is the trimmed string at key in a Run result, or
// "" when it is missing or not a string.
func ConversationResultString(value map[string]interface{}, key string) string {
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

func (r *TurnRunner) validateConversationRun(ctx context.Context, run *models.AgentRun) error {
	if run == nil {
		return models.ErrInvalidAgentRun
	}
	if taskID, _ := run.Context[models.ConversationTaskContextKey].(string); taskID != "" {
		task, err := canonicalConversationTaskContinuation(ctx, r.portfolio, run)
		if err != nil {
			return err
		}
		if task == nil {
			return models.ErrInvalidConversationTask
		}
		// Reuse the ordinary envelope validation only after proving the new
		// lane against the canonical Task ledger.
		run = models.CloneAgentRun(run)
		root, _ := run.Context["threadRootMessageId"].(string)
		run.ConcurrencyKey = models.ConversationRunLane(task.ConversationID, root, run.AssignedAgentID)
	}
	return models.ValidateConversationRun(run)
}

func (r *TurnRunner) conversationTaskResponseReferences(ctx context.Context, run *models.AgentRun, references []models.ConversationReference) ([]models.ConversationReference, error) {
	result := append([]models.ConversationReference(nil), references...)
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
		if taskID, _ := run.Context[models.ConversationTaskContextKey].(string); taskID != "" || strings.HasPrefix(run.ConcurrencyKey, "task:") {
			return nil, models.ErrRunNotFound
		}
		return result, nil
	}
	task, err := canonicalConversationTaskContinuation(ctx, r.portfolio, current)
	if err != nil || task == nil {
		return result, err
	}
	for _, reference := range []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: run.ID}, {Kind: models.ConversationReferenceTask, ID: task.ID}} {
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

func permanentConversationRunError(err error) bool {
	return errors.Is(err, models.ErrConversationNotFound) || errors.Is(err, models.ErrChannelMessageNotFound) ||
		errors.Is(err, runtimeconversation.ErrNoConversationParticipants) || errors.Is(err, runtimeconversation.ErrConversationCoordinationUnavailable) ||
		errors.Is(err, models.ErrInvalidConversation) || errors.Is(err, models.ErrInvalidAgentRun) || errors.Is(err, models.ErrMessageConflict)
}

func publicConversationRetryReason(err error) string {
	switch {
	case errors.Is(err, models.ErrRevisionConflict):
		return "conversation_changed"
	case errors.Is(err, models.ErrConversationCursorConflict):
		return "cursor_changed"
	case errors.Is(err, models.ErrConversationPresenceConflict):
		return "presence_changed"
	case errors.Is(err, context.DeadlineExceeded):
		return "participant_timeout"
	default:
		return "participant_runtime_unavailable"
	}
}

func conversationParticipationAllows(conversation *models.Conversation, message *models.ChannelMessage) bool {
	if conversation == nil || message == nil || conversation.Status != models.ConversationStatusActive {
		return false
	}
	// A builder conversation exists to be answered by its builder: starting
	// one is the person's opt-in, so it needs no separate setting.
	if isAgentBuilderConversation(conversation) {
		return true
	}
	return conversation.Participation != nil && conversation.Participation.Enabled && message.Sequence > conversation.Participation.AfterSequence
}

// isAgentBuilderConversation reports a chat with the builder Agent that
// StartAgentBuilderConversation (or the cortex route) opened.
func isAgentBuilderConversation(conversation *models.Conversation) bool {
	return conversation.Origin != nil && conversation.Origin.Kind == models.ConversationReferenceAgentBuilder &&
		conversation.Owner == models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: AgentBuilderDeploymentID}
}

// Recheck the canonical channel at execution, not just scheduling. A queued Run
// must not make model calls after opt-out or after a later activation boundary.
func (r *TurnRunner) participationStopped(ctx context.Context, run *models.AgentRun) (bool, error) {
	if !r.config.RequireParticipationOptIn {
		return false, nil
	}
	conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
	triggerID, _ := run.Context[models.ConversationRunTriggerIDContextKey].(string)
	conversation, err := r.conversations.GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return false, err
	}
	if conversation.Owner != run.Owner {
		return false, fmt.Errorf("%w: conversation Run owner does not match its channel", models.ErrInvalidAgentRun)
	}
	trigger, err := r.conversations.GetChannelMessage(ctx, run.Scope, conversationID, triggerID)
	if err != nil {
		return false, err
	}
	return !conversationParticipationAllows(conversation, trigger), nil
}

var errConversationParticipationStopped = errors.New("channel participation is disabled for this message")

// participationStoppedOutput is the output of a Run whose message is outside
// the channel's participation opt-in. The external conversation reply
// (externalconversation's externalConversationRunReply) posts its reply.
func participationStoppedOutput() map[string]interface{} {
	return map[string]interface{}{"participationSkipped": true, "reply": "No further participation will start for this message because it is outside the channel’s current participation opt-in. Previously started actions may still finish."}
}

// SetReactionProjector mirrors reactions Agents add to external
// conversations.
func (r *TurnRunner) SetReactionProjector(projector runtimeconversation.MessageReactionProjector) {
	if r != nil && r.conversations != nil {
		r.conversations.SetReactionProjector(projector)
	}
}
