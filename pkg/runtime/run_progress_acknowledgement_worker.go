package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const runProgressAcknowledgementCorrelationKind = "run_acknowledgement"

// RunProgressSnapshot contains only verified, operator-visible progress facts.
// It deliberately excludes action arguments, provider payloads, credentials,
// hidden reasoning, and checkpoint internals.
type RunProgressSnapshot struct {
	RunID           string         `json:"runId"`
	Scope           Scope          `json:"scope"`
	AgentID         string         `json:"agentId"`
	Goal            string         `json:"goal"`
	Status          AgentRunStatus `json:"status"`
	Revision        int64          `json:"revision"`
	ActivityID      string         `json:"activityId,omitempty"`
	ActivityType    string         `json:"activityType,omitempty"`
	ActivitySummary string         `json:"activitySummary,omitempty"`
}

type RunProgressAcknowledgementRequest struct {
	Snapshot      RunProgressSnapshot `json:"snapshot"`
	MaxSentences  int                 `json:"maxSentences"`
	MaxCharacters int                 `json:"maxCharacters"`
}

// RunProgressAcknowledgementRenderer owns wording. OpenSeal supplies facts and
// validates the result; embedding hosts may use the Agent's configured model
// and personality without teaching the kernel provider- or Skill-specific
// phrases.
type RunProgressAcknowledgementRenderer interface {
	RenderRunProgressAcknowledgement(context.Context, RunProgressAcknowledgementRequest) (string, error)
}

type RunProgressAcknowledgementRendererFunc func(context.Context, RunProgressAcknowledgementRequest) (string, error)

func (f RunProgressAcknowledgementRendererFunc) RenderRunProgressAcknowledgement(ctx context.Context, request RunProgressAcknowledgementRequest) (string, error) {
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

type RunProgressAcknowledgementStore interface {
	ExternalConversationStore
	PortfolioStore
	ListActivity(context.Context, ActivityFilter) ([]*ActivityEvent, error)
}

// RunProgressAcknowledgementWorker renders meaningful Run progress and lets
// the bound conversation adapter choose its declared presentation capability.
type RunProgressAcknowledgementWorker struct {
	store         RunProgressAcknowledgementStore
	resolver      ExternalConversationAdapterResolver
	renderer      RunProgressAcknowledgementRenderer
	conversations *ConversationService
	transport     *ExternalConversationTransportService
	config        RunProgressAcknowledgementWorkerConfig
	now           func() time.Time
}

func NewRunProgressAcknowledgementWorker(
	store RunProgressAcknowledgementStore,
	resolver ExternalConversationAdapterResolver,
	renderer RunProgressAcknowledgementRenderer,
	config RunProgressAcknowledgementWorkerConfig,
) (*RunProgressAcknowledgementWorker, error) {
	if store == nil || resolver == nil || renderer == nil {
		return nil, errors.New("Run progress acknowledgement store, adapter resolver, and renderer are required")
	}
	normalized, err := config.normalize()
	if err != nil {
		return nil, err
	}
	return &RunProgressAcknowledgementWorker{
		store: store, resolver: resolver, renderer: renderer, conversations: NewConversationService(store),
		transport: NewExternalConversationTransportService(store, resolver), config: normalized, now: time.Now,
	}, nil
}

func (w *RunProgressAcknowledgementWorker) ProcessScope(ctx context.Context, scope Scope) ([]*ExternalConversationDelivery, error) {
	if w == nil || w.store == nil || w.resolver == nil || w.renderer == nil || w.conversations == nil || w.transport == nil {
		return nil, errors.New("Run progress acknowledgement worker is not configured")
	}
	items, err := w.store.ListExternalConversationInbox(ctx, ExternalConversationInboxFilter{
		Scope: scope, Statuses: []ExternalConversationInboxStatus{ExternalConversationInboxApplied}, Limit: w.config.PageSize,
	})
	if err != nil {
		return nil, err
	}
	result := make([]*ExternalConversationDelivery, 0)
	var processErrors []error
	for _, item := range items {
		delivery, processErr := w.process(ctx, item)
		if processErr != nil {
			processErrors = append(processErrors, fmt.Errorf("project Run acknowledgement for inbox item %s: %w", item.ID, processErr))
			continue
		}
		if delivery != nil {
			result = append(result, delivery)
		}
	}
	return result, errors.Join(processErrors...)
}

func (w *RunProgressAcknowledgementWorker) process(ctx context.Context, item *ExternalConversationInboxItem) (*ExternalConversationDelivery, error) {
	if item == nil || item.Status != ExternalConversationInboxApplied || item.RunID == "" || item.ConversationID == "" || item.ChannelMessageID == "" {
		return nil, nil
	}
	endpoint, adapter, err := w.resolveEndpoint(ctx, item)
	if err != nil || endpoint == nil {
		return nil, err
	}
	run, err := w.store.GetAgentRun(ctx, item.Scope, item.RunID)
	if err != nil {
		return nil, err
	}
	now := w.now().UTC()
	if run == nil || strings.TrimSpace(run.AssignedAgentID) == "" || isTerminalAgentRunStatus(run.Status) || now.Sub(run.CreatedAt) < w.config.MinimumRunAge {
		return nil, nil
	}
	events, err := w.store.ListActivity(ctx, ActivityFilter{Scope: item.Scope, RunID: run.ID, Descending: true, Limit: 1})
	if err != nil {
		return nil, err
	}
	var latest *ActivityEvent
	if len(events) > 0 {
		latest = events[0]
	}
	snapshot := runProgressSnapshot(run, latest)
	phase := runProgressPhase(snapshot)
	prior, err := w.store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{
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
	text, err := w.renderer.RenderRunProgressAcknowledgement(ctx, RunProgressAcknowledgementRequest{
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
	if threadID == "" && endpoint.Policy.ReplyMode == ExternalConversationReplyThread {
		threadID = strings.TrimSpace(item.Event.ExternalMessageID)
	}
	operation := capability.ConversationDeliveryMessageSend
	messageID := item.ChannelMessageID
	parameters := map[string]interface{}(nil)
	if threadID != "" && containsConversationDeliveryOperation(adapter.Adapter.Delivery.Operations, capability.ConversationDeliveryTypingIndicator) {
		operation = capability.ConversationDeliveryTypingIndicator
		parameters = map[string]interface{}{"status": acknowledgement.Text}
	} else {
		message, postErr := w.post(ctx, item, endpoint, acknowledgement)
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
		Correlation:    &ExternalConversationDeliveryCorrelation{Kind: runProgressAcknowledgementCorrelationKind, ID: run.ID, Phase: phase},
		IdempotencyKey: "run-acknowledgement-delivery:" + item.ID + ":" + phase,
	})
	if err != nil {
		return nil, err
	}
	return enqueued.Delivery, nil
}

func (w *RunProgressAcknowledgementWorker) resolveEndpoint(ctx context.Context, item *ExternalConversationInboxItem) (*ExternalConversationEndpoint, *skill.BoundConversationAdapter, error) {
	endpoint, err := w.store.GetExternalConversationEndpoint(ctx, item.Scope, item.EndpointID)
	if err != nil {
		return nil, nil, err
	}
	if endpoint == nil || endpoint.Status != ExternalConversationEndpointActive || endpoint.Revision != item.EndpointRevision ||
		!externalConversationAdapterBelongsToEndpoint(endpoint.Adapter, item.Adapter) {
		return nil, nil, nil
	}
	adapter, err := w.resolver.ResolveConversationAdapterBinding(
		ctx, skill.ScopeReference{Kind: item.Scope.Kind, ID: item.Scope.ID}, endpoint.DeploymentID,
		endpoint.Adapter.BindingID, endpoint.Adapter.AdapterID,
	)
	if err != nil || adapter == nil || adapter.Binding == nil || adapter.Adapter.Provider != endpoint.Provider {
		return nil, nil, fmt.Errorf("%w: current conversation adapter is unavailable", ErrExternalConversationConflict)
	}
	return endpoint, adapter, nil
}

func (w *RunProgressAcknowledgementWorker) post(ctx context.Context, item *ExternalConversationInboxItem, endpoint *ExternalConversationEndpoint, acknowledgement RunProgressAcknowledgement) (*ChannelMessage, error) {
	for range 3 {
		conversation, err := w.conversations.GetConversation(ctx, item.Scope, item.ConversationID)
		if err != nil {
			return nil, err
		}
		references := []ConversationReference{{Kind: ConversationReferenceRun, ID: acknowledgement.RunID, Version: acknowledgement.SourceRevision}}
		if acknowledgement.SourceEventID != "" {
			references = append(references, ConversationReference{Kind: ConversationReferenceActivity, ID: acknowledgement.SourceEventID})
		}
		posted, err := w.conversations.PostChannelMessage(ctx, PostChannelMessageRequest{
			Scope: item.Scope, ConversationID: item.ConversationID, ExpectedRevision: conversation.Revision,
			Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: endpoint.DeploymentID},
			Intent: MessageIntentAcknowledgment, Content: acknowledgement.Text,
			Audience: ConversationAudience{Kind: ConversationAudienceChannel}, ReplyToMessageID: item.ChannelMessageID,
			References: references, IdempotencyKey: "run-acknowledgement-message:" + item.ID + ":" + acknowledgement.Phase,
		})
		if err == nil {
			return posted.Message, nil
		}
		if !errors.Is(err, ErrRevisionConflict) && !errors.Is(err, ErrMessageConflict) {
			return nil, err
		}
	}
	return nil, ErrRevisionConflict
}

func runProgressSnapshot(run *AgentRun, event *ActivityEvent) RunProgressSnapshot {
	result := RunProgressSnapshot{
		RunID: run.ID, Scope: run.Scope, AgentID: run.AssignedAgentID, Goal: run.Goal, Status: run.Status, Revision: run.Revision,
	}
	if event != nil {
		result.ActivityID, result.ActivityType, result.ActivitySummary = event.ID, event.EventType, event.Summary
	}
	return result
}

func runProgressPhase(snapshot RunProgressSnapshot) string {
	source := string(snapshot.Status) + "\x00" + snapshot.ActivityID + "\x00" + snapshot.ActivityType
	digest := hashString(source)
	return "progress-" + digest[:16]
}

func runProgressPhaseDelivered(deliveries []*ExternalConversationDelivery, phase string) bool {
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

const runProgressAcknowledgementSystemInstruction = "Produce only a brief progress acknowledgement in the Agent's configured personality. Use only the verified facts in inputContext.progress. Write at most two short sentences and 100 characters. Do not claim completion, expose hidden reasoning, propose actions, call tools, or add unrelated explanation."

type hostedTurnProgressAcknowledgementRenderer struct {
	runner TurnRunner
}

func (r *hostedTurnProgressAcknowledgementRenderer) RenderRunProgressAcknowledgement(ctx context.Context, request RunProgressAcknowledgementRequest) (string, error) {
	if r == nil || r.runner == nil || request.Snapshot.Scope.Validate() != nil || request.Snapshot.RunID == "" || request.Snapshot.AgentID == "" {
		return "", errors.New("hosted Run progress acknowledgement renderer is unavailable")
	}
	now := time.Now().UTC()
	run := &AgentRun{
		ID: request.Snapshot.RunID, Kind: RunKindAgentWork, Scope: request.Snapshot.Scope,
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: request.Snapshot.AgentID}, AssignedAgentID: request.Snapshot.AgentID,
		Goal: "Acknowledge the current verified progress", Source: RunSourceChat, Status: AgentRunStatusRunning,
		Context: map[string]interface{}{"progress": map[string]interface{}{
			"goal": request.Snapshot.Goal, "status": request.Snapshot.Status,
			"activityType": request.Snapshot.ActivityType, "activitySummary": request.Snapshot.ActivitySummary,
		}},
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	turnID := "progress-ack-" + hashString(request.Snapshot.RunID + "\x00" + request.Snapshot.ActivityID + "\x00" + string(request.Snapshot.Status))[:16]
	turn := &AgentTurn{
		ID: turnID, Scope: request.Snapshot.Scope, RunID: run.ID, Sequence: 0,
		Status: AgentTurnStatusRunning, Revision: 1, CreatedAt: now, UpdatedAt: now, StartedAt: now,
	}
	outcome, err := r.runner.RunTurn(ctx, TurnExecutionContext{Run: run, Turn: turn})
	if err != nil {
		return "", err
	}
	if outcome == nil || outcome.NextRunStatus != AgentRunStatusCompleted || len(outcome.ProposedActions) > 0 ||
		outcome.ProposedFork != nil || outcome.ProposedDelegation != nil || outcome.ProposedRunbook != nil {
		return "", errors.New("Agent returned a non-terminal or actionable progress acknowledgement")
	}
	content := strings.TrimSpace(outcome.OutputSummary)
	if reply, ok := outcome.RunOutput["reply"].(string); ok && strings.TrimSpace(reply) != "" {
		content = strings.TrimSpace(reply)
	}
	return validateRenderedRunProgressAcknowledgement(content, request.MaxSentences, request.MaxCharacters)
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
	resolver TurnRunnerResolver
}

func NewTurnRunnerProgressAcknowledgementRenderer(resolver TurnRunnerResolver) (*TurnRunnerProgressAcknowledgementRenderer, error) {
	if resolver == nil {
		return nil, errors.New("Run progress acknowledgement Turn resolver is required")
	}
	return &TurnRunnerProgressAcknowledgementRenderer{resolver: resolver}, nil
}

func (r *TurnRunnerProgressAcknowledgementRenderer) RenderRunProgressAcknowledgement(ctx context.Context, request RunProgressAcknowledgementRequest) (string, error) {
	if r == nil || r.resolver == nil || request.Snapshot.RunID == "" || request.Snapshot.AgentID == "" || request.Snapshot.Scope.Validate() != nil {
		return "", errors.New("Run progress acknowledgement rendering is unavailable")
	}
	run := &AgentRun{
		ID: request.Snapshot.RunID, Kind: RunKindAgentWork,
		Scope: request.Snapshot.Scope,
		Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: request.Snapshot.AgentID}, AssignedAgentID: request.Snapshot.AgentID,
		Goal:   request.Snapshot.Goal,
		Source: RunSourceChat, Status: AgentRunStatusRunning,
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
