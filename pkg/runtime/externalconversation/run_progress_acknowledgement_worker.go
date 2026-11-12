package externalconversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const runProgressAcknowledgementCorrelationKind = "run_acknowledgement"

type RunProgressAcknowledgementRendererFunc func(context.Context, agentrun.RunProgressAcknowledgementRequest) (string, error)

func (f RunProgressAcknowledgementRendererFunc) RenderRunProgressAcknowledgement(ctx context.Context, request agentrun.RunProgressAcknowledgementRequest) (string, error) {
	return f(ctx, request)
}

type RunProgressAcknowledgement struct {
	RunID          string `json:"runId"`
	Phase          string `json:"phase"`
	Text           string `json:"text"`
	SourceEventID  string `json:"sourceEventId,omitempty"`
	SourceRevision int64  `json:"sourceRevision"`
}

type RunProgressAcknowledgementWorkerConfig struct {
	ReviewURL               func(kind, requestID, deploymentID, conversationID string, owner models.ObjectiveOwner) (string, error)
	MinimumRunAge           time.Duration
	MinimumInterval         time.Duration
	PageSize                int
	MaximumAcknowledgements int
}

func (c RunProgressAcknowledgementWorkerConfig) normalize() (RunProgressAcknowledgementWorkerConfig, error) {
	if c.MinimumRunAge == 0 {
		// A run created from an external message may legitimately finish in less
		// than two seconds. Schedule its first acknowledgement in the ingress
		// reconciliation cycle so native channel progress is still observable.
		c.MinimumRunAge = time.Nanosecond
	}
	if c.MinimumInterval == 0 {
		c.MinimumInterval = 4 * time.Second
	}
	if c.PageSize == 0 {
		c.PageSize = 100
	}
	if c.MaximumAcknowledgements == 0 {
		c.MaximumAcknowledgements = 8
	}
	if c.MinimumRunAge < 0 || c.MinimumInterval < 0 || c.PageSize < 1 || c.PageSize > 1000 ||
		c.MaximumAcknowledgements < 1 || c.MaximumAcknowledgements > 100 {
		return RunProgressAcknowledgementWorkerConfig{}, errors.New("invalid Run progress acknowledgement worker configuration")
	}
	return c, nil
}

// RunProgressAcknowledgementWorker renders meaningful Run progress and lets
// the bound conversation adapter choose its declared presentation capability.
type RunProgressAcknowledgementWorker struct {
	store         models.RunProgressAcknowledgementStore
	resolver      ExternalConversationAdapterResolver
	renderer      agentrun.RunProgressAcknowledgementRenderer
	conversations *runtimeconversation.ConversationService
	transport     *ExternalConversationTransportService
	config        RunProgressAcknowledgementWorkerConfig
	now           func() time.Time
}

func NewRunProgressAcknowledgementWorker(
	store models.RunProgressAcknowledgementStore,
	resolver ExternalConversationAdapterResolver,
	renderer agentrun.RunProgressAcknowledgementRenderer,
	config RunProgressAcknowledgementWorkerConfig,
	opts ...models.ServiceOption,
) (*RunProgressAcknowledgementWorker, error) {
	if store == nil || resolver == nil || renderer == nil {
		return nil, errors.New("Run progress acknowledgement store, adapter resolver, and renderer are required")
	}
	normalized, err := config.normalize()
	if err != nil {
		return nil, err
	}
	return &RunProgressAcknowledgementWorker{
		store: store, resolver: resolver, renderer: renderer, conversations: runtimeconversation.NewConversationService(store, opts...),
		transport: NewExternalConversationTransportService(store, resolver, opts...), config: normalized, now: models.ClockFrom(opts),
	}, nil
}

func (w *RunProgressAcknowledgementWorker) ProcessScope(ctx context.Context, scope models.Scope) ([]*models.ExternalConversationDelivery, error) {
	return w.processScope(ctx, scope, true)
}

// ProcessCommentaryScope delivers existing public Agent updates without a
// second model call or synthesized lifecycle narration.
func (w *RunProgressAcknowledgementWorker) ProcessCommentaryScope(ctx context.Context, scope models.Scope) ([]*models.ExternalConversationDelivery, error) {
	return w.processScope(ctx, scope, false)
}

func (w *RunProgressAcknowledgementWorker) processScope(ctx context.Context, scope models.Scope, renderStatus bool) ([]*models.ExternalConversationDelivery, error) {
	if w == nil || w.store == nil || w.resolver == nil || w.renderer == nil || w.conversations == nil || w.transport == nil {
		return nil, errors.New("Run progress acknowledgement worker is not configured")
	}
	items, err := w.store.ListExternalConversationInbox(ctx, models.ExternalConversationInboxFilter{
		Scope: scope, Statuses: []models.ExternalConversationInboxStatus{models.ExternalConversationInboxApplied}, Limit: w.config.PageSize,
	})
	if err != nil {
		return nil, err
	}
	result := make([]*models.ExternalConversationDelivery, 0)
	var processErrors []error
	if !renderStatus {
		reviews, err := w.projectReviewRequests(ctx, scope)
		result = append(result, reviews...)
		if err != nil {
			processErrors = append(processErrors, err)
		}
	}
	for _, item := range items {
		if item == nil || item.Status != models.ExternalConversationInboxApplied {
			continue
		}
		// A Team's item is answered by each member Run its routing started.
		replies, repliesErr := externalItemReplies(ctx, w.store, item)
		if repliesErr != nil {
			processErrors = append(processErrors, fmt.Errorf("resolve answering Runs for inbox item %s: %w", item.ID, repliesErr))
			continue
		}
		agents := externalItemReplyAgents(replies)
		for _, reply := range replies {
			deliveries, err := w.processReply(ctx, item, reply, agents, renderStatus)
			result = append(result, deliveries...)
			processErrors = append(processErrors, err...)
		}
	}
	return result, errors.Join(processErrors...)
}

func (w *RunProgressAcknowledgementWorker) processReply(ctx context.Context, item *models.ExternalConversationInboxItem, reply externalItemReply, agents []string, renderStatus bool) ([]*models.ExternalConversationDelivery, []error) {
	updates, hasCommentary, err := w.projectCommentary(ctx, item, reply)
	if err != nil {
		return nil, []error{fmt.Errorf("project channel commentary for inbox item %s: %w", item.ID, err)}
	}
	result := updates
	var processErrors []error
	// The answer streams after the commentary that precedes it.
	if stream, streamErr := w.projectAnswerStream(ctx, item, reply); streamErr != nil {
		processErrors = append(processErrors, fmt.Errorf("stream channel answer for inbox item %s: %w", item.ID, streamErr))
	} else if stream != nil {
		result = append(result, stream)
	}
	if !renderStatus {
		status, statusErr := w.projectThreadStatus(ctx, item, reply, agents)
		if statusErr != nil {
			processErrors = append(processErrors, statusErr)
		} else if status != nil {
			result = append(result, status)
		}
		return result, processErrors
	}
	if hasCommentary {
		return result, processErrors
	}
	delivery, err := w.process(ctx, item, reply)
	if err != nil {
		return result, append(processErrors, fmt.Errorf("project Run acknowledgement for inbox item %s: %w", item.ID, err))
	}
	if delivery != nil {
		result = append(result, delivery)
	}
	return result, processErrors
}

func (w *RunProgressAcknowledgementWorker) process(ctx context.Context, item *models.ExternalConversationInboxItem, reply externalItemReply) (*models.ExternalConversationDelivery, error) {
	if item == nil || item.Status != models.ExternalConversationInboxApplied || item.RunID == "" || item.ConversationID == "" || item.ChannelMessageID == "" {
		return nil, nil
	}
	endpoint, adapter, err := w.resolveEndpoint(ctx, item)
	if err != nil || endpoint == nil {
		return nil, err
	}
	run := reply.run
	now := w.now().UTC()
	if strings.TrimSpace(run.AssignedAgentID) == "" || models.IsTerminalAgentRunStatus(run.Status) || now.Sub(run.CreatedAt) < w.config.MinimumRunAge || conversationrun.ExternalInboxItemIsFollowUp(item, run) {
		return nil, nil
	}
	events, err := w.store.ListActivity(ctx, models.ActivityFilter{Scope: item.Scope, RunID: run.ID, Descending: true, Limit: 1})
	if err != nil {
		return nil, err
	}
	var latest *models.ActivityEvent
	if len(events) > 0 {
		latest = events[0]
	}
	snapshot := runProgressSnapshot(run, latest)
	phase := runProgressPhase(snapshot)
	prior, err := w.store.ListExternalConversationDeliveries(ctx, models.ExternalConversationDeliveryFilter{
		Scope: item.Scope, EndpointID: endpoint.ID, ConversationID: item.ConversationID,
		CorrelationKind: runProgressAcknowledgementCorrelationKind, CorrelationID: run.ID, Limit: 100,
	})
	if err != nil {
		return nil, err
	}
	if len(prior) >= w.config.MaximumAcknowledgements || runProgressPhaseDelivered(prior, phase) ||
		(len(prior) > 0 && now.Sub(prior[0].CreatedAt) < w.config.MinimumInterval) {
		return nil, nil
	}
	text, err := w.renderer.RenderRunProgressAcknowledgement(ctx, agentrun.RunProgressAcknowledgementRequest{
		Snapshot: snapshot, MaxSentences: 2, MaxCharacters: 100,
	})
	if err != nil {
		return nil, err
	}
	text, err = validateRenderedRunProgressAcknowledgement(text, 2, 100)
	if err != nil {
		return nil, err
	}
	acknowledgement := RunProgressAcknowledgement{
		RunID: run.ID, Phase: phase, Text: text, SourceRevision: run.Revision,
	}
	if latest != nil {
		acknowledgement.SourceEventID = latest.ID
	}
	threadID := strings.TrimSpace(item.Event.ExternalThreadID)
	if threadID == "" && endpoint.Policy.ReplyMode == models.ExternalConversationReplyThread {
		threadID = strings.TrimSpace(item.Event.ExternalMessageID)
	}
	operation := capability.ConversationDeliveryMessageSend
	messageID := item.ChannelMessageID
	parameters := map[string]interface{}(nil)
	if threadID != "" && containsConversationDeliveryOperation(adapter.Adapter.Delivery.Operations, capability.ConversationDeliveryTypingIndicator) {
		operation = capability.ConversationDeliveryTypingIndicator
		parameters = map[string]interface{}{"status": acknowledgement.Text}
	} else {
		message, postErr := w.post(ctx, item, reply.id, acknowledgement, snapshot.AgentID)
		if postErr != nil {
			return nil, postErr
		}
		messageID = message.ID
	}
	enqueued, err := w.transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
		Scope: item.Scope, EndpointID: endpoint.ID, Operation: operation,
		ConversationID: item.ConversationID, ChannelMessageID: messageID,
		ExternalConversationID: item.Event.ExternalConversationID, ExternalThreadID: threadID,
		Parameters:     parameters,
		Correlation:    &models.ExternalConversationDeliveryCorrelation{Kind: runProgressAcknowledgementCorrelationKind, ID: run.ID, Phase: phase},
		IdempotencyKey: "run-acknowledgement-delivery:" + reply.id + ":" + phase,
	})
	if err != nil {
		return nil, err
	}
	return enqueued.Delivery, nil
}

func (w *RunProgressAcknowledgementWorker) resolveEndpoint(ctx context.Context, item *models.ExternalConversationInboxItem) (*models.ExternalConversationEndpoint, *skill.BoundConversationAdapter, error) {
	endpoint, err := w.store.GetExternalConversationEndpoint(ctx, item.Scope, item.EndpointID)
	if err != nil {
		return nil, nil, err
	}
	if endpoint == nil || endpoint.Status != models.ExternalConversationEndpointActive || endpoint.Revision != item.EndpointRevision ||
		!models.ExternalConversationAdapterBelongsToEndpoint(endpoint.Adapter, item.Adapter) {
		return nil, nil, nil
	}
	adapter, err := w.resolver.ResolveConversationAdapterBinding(
		ctx, skill.ScopeReference{Kind: item.Scope.Kind, ID: item.Scope.ID}, endpoint.DeploymentID,
		endpoint.Adapter.BindingID, endpoint.Adapter.AdapterID,
	)
	if err != nil || adapter == nil || adapter.Binding == nil || adapter.Adapter.Provider != endpoint.Provider {
		return nil, nil, fmt.Errorf("%w: current conversation adapter is unavailable", models.ErrExternalConversationConflict)
	}
	return endpoint, adapter, nil
}

// post sends the acknowledgement as the Run's acting Agent, which rendered it.
// replyID keys it to that Agent's answer (externalItemReply).
func (w *RunProgressAcknowledgementWorker) post(ctx context.Context, item *models.ExternalConversationInboxItem, replyID string, acknowledgement RunProgressAcknowledgement, agentID string) (*models.ChannelMessage, error) {
	for range 3 {
		conversation, err := w.conversations.GetConversation(ctx, item.Scope, item.ConversationID)
		if err != nil {
			return nil, err
		}
		references := []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: acknowledgement.RunID, Version: acknowledgement.SourceRevision}}
		if acknowledgement.SourceEventID != "" {
			references = append(references, models.ConversationReference{Kind: models.ConversationReferenceActivity, ID: acknowledgement.SourceEventID})
		}
		posted, err := w.conversations.PostChannelMessage(ctx, runtimeconversation.PostChannelMessageRequest{
			Scope: item.Scope, ConversationID: item.ConversationID, ExpectedRevision: conversation.Revision,
			Sender: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: agentID},
			Intent: models.MessageIntentAcknowledgment, Content: acknowledgement.Text,
			Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, ReplyToMessageID: item.ChannelMessageID,
			References: references, IdempotencyKey: "run-acknowledgement-message:" + replyID + ":" + acknowledgement.Phase,
		})
		if err == nil {
			return posted.Message, nil
		}
		if !errors.Is(err, models.ErrRevisionConflict) && !errors.Is(err, models.ErrMessageConflict) {
			return nil, err
		}
	}
	return nil, models.ErrRevisionConflict
}

func runProgressSnapshot(run *models.AgentRun, event *models.ActivityEvent) agentrun.RunProgressSnapshot {
	result := agentrun.RunProgressSnapshot{
		RunID: run.ID, Scope: run.Scope, AgentID: run.AssignedAgentID, Goal: run.Goal, Status: run.Status, Revision: run.Revision,
	}
	if event != nil {
		result.ActivityID, result.ActivityType, result.ActivitySummary = event.ID, event.EventType, event.Summary
	}
	return result
}

func runProgressPhase(snapshot agentrun.RunProgressSnapshot) string {
	source := string(snapshot.Status) + "\x00" + snapshot.ActivityID + "\x00" + snapshot.ActivityType
	digest := models.SHA256DigestString(source)
	return "progress-" + digest[:16]
}

func runProgressPhaseDelivered(deliveries []*models.ExternalConversationDelivery, phase string) bool {
	for _, delivery := range deliveries {
		if delivery != nil && delivery.Correlation != nil && delivery.Correlation.Phase == phase {
			return true
		}
	}
	return false
}

func validateRenderedRunProgressAcknowledgement(value string, maximumSentences, maximumCharacters int) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || maximumSentences < 1 || maximumCharacters < 1 || len(value) > maximumCharacters || strings.ContainsAny(value, "\r\n") || sentenceCount(value) > maximumSentences {
		return "", errors.New("rendered Run progress acknowledgement exceeds its sentence or character bound")
	}
	return value, nil
}

const runProgressAcknowledgementSystemInstruction = "Produce only a brief progress acknowledgement in the Agent's configured personality. Use only the verified facts in the user message. Write at most two short sentences and 100 characters. Do not claim completion, expose hidden reasoning, propose actions, call tools, or add unrelated explanation."

// modelStepProgressAcknowledgementRenderer asks the assigned Agent's model for
// one bounded, tool-free sentence through a single sessionless model step. The
// step writes no agent_turns row and no session entry.
type modelStepProgressAcknowledgementRenderer struct {
	host              agentrun.StepHost
	agentID           string
	definitionID      string
	definitionVersion string
	modelCredential   *capability.CredentialReference
	// system is the Agent's instructions followed by
	// runProgressAcknowledgementSystemInstruction.
	system []string
	now    func() time.Time
}

// NewProgressAcknowledgementRenderer renders through host as the Agent
// agentID at its definition version, with its model credential and
// instructions (the system prompt adds the acknowledgement rules).
func NewProgressAcknowledgementRenderer(host agentrun.StepHost, agentID, definitionID, definitionVersion string, modelCredential *capability.CredentialReference, instructions []string, opts ...models.ServiceOption) agentrun.RunProgressAcknowledgementRenderer {
	return &modelStepProgressAcknowledgementRenderer{
		host: host, agentID: agentID, definitionID: definitionID, definitionVersion: definitionVersion,
		modelCredential: modelCredential,
		system:          append(append([]string(nil), instructions...), runProgressAcknowledgementSystemInstruction),
		now:             models.ClockFrom(opts),
	}
}

func (r *modelStepProgressAcknowledgementRenderer) RenderRunProgressAcknowledgement(ctx context.Context, request agentrun.RunProgressAcknowledgementRequest) (string, error) {
	snapshot := request.Snapshot
	if r == nil || r.host == nil || snapshot.Scope.Validate() != nil || snapshot.RunID == "" || snapshot.AgentID == "" ||
		(r.agentID != "" && r.agentID != snapshot.AgentID) {
		return "", errors.New("Run progress acknowledgement renderer is unavailable")
	}
	facts, err := json.Marshal(map[string]interface{}{
		"goal": snapshot.Goal, "status": snapshot.Status,
		"activityType": snapshot.ActivityType, "activitySummary": snapshot.ActivitySummary,
	})
	if err != nil {
		return "", err
	}
	now := time.Now
	if r.now != nil {
		now = r.now
	}
	stepID := "progress-ack-" + models.SHA256DigestString(snapshot.RunID + "\x00" + snapshot.ActivityID + "\x00" + string(snapshot.Status))[:16]
	req := models.ModelStepRequest{
		APIVersion: models.ModelStepAPIVersion, Purpose: models.ModelStepPurposeAcknowledgement,
		Scope: snapshot.Scope, RunID: snapshot.RunID, StepID: stepID, SessionID: "",
		AgentID: snapshot.AgentID, DefinitionID: r.definitionID, DefinitionVersion: r.definitionVersion,
		ModelCredential: r.modelCredential,
		System:          models.ModelStepSystem{AgentInstructions: append([]string(nil), r.system...)},
		Tools:           nil,
		Entries: []models.SessionEntryView{{
			ID: stepID + "-facts", Sequence: 1, Type: models.SessionEntryUser, CreatedAt: now().UTC(),
			User: &models.UserEntryView{Content: []models.ContentPart{{Type: models.ContentPartText, Text: string(facts)}}},
		}},
	}
	if err := req.Validate(); err != nil {
		return "", err
	}
	result, err := r.host.ExecuteModelStep(ctx, req)
	if err != nil {
		return "", err
	}
	if len(result.Assistant.ToolCalls) > 0 {
		return "", errors.New("Agent returned an actionable progress acknowledgement")
	}
	if err := modelstep.ValidateModelStepResult(req, result); err != nil {
		return "", err
	}
	return validateRenderedRunProgressAcknowledgement(result.Assistant.Text, request.MaxSentences, request.MaxCharacters)
}

func sentenceCount(value string) int {
	count, inSentence := 0, false
	for _, character := range value {
		if character == '.' || character == '!' || character == '?' {
			if inSentence {
				count++
				inSentence = false
			}
			continue
		}
		if !strings.ContainsRune(" \t,;:-—()[]{}\"'", character) {
			inSentence = true
		}
	}
	if inSentence {
		count++
	}
	return count
}

// TurnRunnerProgressAcknowledgementRenderer asks the assigned Agent for a
// tool-free, bounded phrase. The resolved runner carries the Agent definition,
// model credential, and personality; OpenSeal accepts no proposals from this
// auxiliary invocation.
type TurnRunnerProgressAcknowledgementRenderer struct {
	resolver runtimeconversation.TurnRunnerResolver
}

func NewTurnRunnerProgressAcknowledgementRenderer(resolver runtimeconversation.TurnRunnerResolver) (*TurnRunnerProgressAcknowledgementRenderer, error) {
	if resolver == nil {
		return nil, errors.New("Run progress acknowledgement Turn resolver is required")
	}
	return &TurnRunnerProgressAcknowledgementRenderer{resolver: resolver}, nil
}

func (r *TurnRunnerProgressAcknowledgementRenderer) RenderRunProgressAcknowledgement(ctx context.Context, request agentrun.RunProgressAcknowledgementRequest) (string, error) {
	if r == nil || r.resolver == nil || request.Snapshot.RunID == "" || request.Snapshot.AgentID == "" || request.Snapshot.Scope.Validate() != nil {
		return "", errors.New("Run progress acknowledgement rendering is unavailable")
	}
	run := &models.AgentRun{
		ID: request.Snapshot.RunID, Kind: models.RunKindAgentWork,
		Scope: request.Snapshot.Scope,
		Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: request.Snapshot.AgentID}, AssignedAgentID: request.Snapshot.AgentID, ActingUser: "",
		Goal:   request.Snapshot.Goal,
		Source: models.RunSourceChat, Status: models.AgentRunStatusRunning,
		Revision: request.Snapshot.Revision,
	}
	binding, err := r.resolver.ResolveTurnRunner(ctx, run)
	if err != nil {
		return "", err
	}
	if binding == nil || binding.ProgressAcknowledgementRenderer == nil {
		return "", errors.New("assigned Agent does not provide progress acknowledgement rendering")
	}
	return binding.ProgressAcknowledgementRenderer.RenderRunProgressAcknowledgement(ctx, request)
}
