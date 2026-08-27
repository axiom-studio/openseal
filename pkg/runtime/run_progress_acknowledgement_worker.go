package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

const (
	runProgressAcknowledgementCorrelationKind = "run_acknowledgement"
)

// RunProgressAcknowledgement is a short, evidence-backed text projection of a
// durable Run. Text is deliberately limited to two or three words so progress
// stays useful without flooding the originating conversation.
type RunProgressAcknowledgement struct {
	RunID          string `json:"runId"`
	Phase          string `json:"phase"`
	Text           string `json:"text"`
	SourceEventID  string `json:"sourceEventId,omitempty"`
	SourceRevision int64  `json:"sourceRevision"`
}

type RunProgressAcknowledgementWorkerConfig struct {
	MinimumRunAge   time.Duration
	MinimumInterval time.Duration
	PageSize        int
}

func (c RunProgressAcknowledgementWorkerConfig) normalize() (RunProgressAcknowledgementWorkerConfig, error) {
	if c.MinimumRunAge == 0 {
		c.MinimumRunAge = 2 * time.Second
	}
	if c.MinimumInterval == 0 {
		c.MinimumInterval = 4 * time.Second
	}
	if c.PageSize == 0 {
		c.PageSize = 100
	}
	if c.MinimumRunAge < 0 || c.MinimumInterval < 0 || c.PageSize < 1 || c.PageSize > 1000 {
		return RunProgressAcknowledgementWorkerConfig{}, errors.New("invalid Run progress acknowledgement worker configuration")
	}
	return c, nil
}

type RunProgressAcknowledgementStore interface {
	ExternalConversationStore
	PortfolioStore
	ListActivity(context.Context, ActivityFilter) ([]*ActivityEvent, error)
}

// RunProgressAcknowledgementWorker projects meaningful Run progress into the
// same external text thread that originated the Run.
type RunProgressAcknowledgementWorker struct {
	store         RunProgressAcknowledgementStore
	conversations *ConversationService
	transport     *ExternalConversationTransportService
	config        RunProgressAcknowledgementWorkerConfig
	now           func() time.Time
}

func NewRunProgressAcknowledgementWorker(
	store RunProgressAcknowledgementStore,
	resolver ExternalConversationAdapterResolver,
	config RunProgressAcknowledgementWorkerConfig,
) (*RunProgressAcknowledgementWorker, error) {
	if store == nil || resolver == nil {
		return nil, errors.New("Run progress acknowledgement store and adapter resolver are required")
	}
	normalized, err := config.normalize()
	if err != nil {
		return nil, err
	}
	return &RunProgressAcknowledgementWorker{
		store: store, conversations: NewConversationService(store),
		transport: NewExternalConversationTransportService(store, resolver),
		config:    normalized, now: time.Now,
	}, nil
}

// ProcessScope is a small reconciliation job. Reprocessing is safe: both the
// canonical message and provider delivery use a stable per-Run phase key.
func (w *RunProgressAcknowledgementWorker) ProcessScope(
	ctx context.Context,
	scope Scope,
) ([]*ExternalConversationDelivery, error) {
	if w == nil || w.store == nil || w.conversations == nil || w.transport == nil {
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

func (w *RunProgressAcknowledgementWorker) process(
	ctx context.Context,
	item *ExternalConversationInboxItem,
) (*ExternalConversationDelivery, error) {
	if item == nil || item.Status != ExternalConversationInboxApplied || item.RunID == "" ||
		item.ConversationID == "" || item.ChannelMessageID == "" {
		return nil, nil
	}
	endpoint, err := w.store.GetExternalConversationEndpoint(ctx, item.Scope, item.EndpointID)
	if err != nil {
		return nil, err
	}
	if endpoint == nil || endpoint.Status != ExternalConversationEndpointActive ||
		endpoint.Revision != item.EndpointRevision ||
		!externalConversationAdapterBelongsToEndpoint(endpoint.Adapter, item.Adapter) {
		return nil, nil
	}
	run, err := w.store.GetAgentRun(ctx, item.Scope, item.RunID)
	if err != nil {
		return nil, err
	}
	now := w.now().UTC()
	if run == nil || isTerminalAgentRunStatus(run.Status) || now.Sub(run.CreatedAt) < w.config.MinimumRunAge {
		return nil, nil
	}
	events, err := w.store.ListActivity(ctx, ActivityFilter{
		Scope: item.Scope, RunID: run.ID, Descending: true, Limit: 1,
	})
	if err != nil {
		return nil, err
	}
	var latest *ActivityEvent
	if len(events) > 0 {
		latest = events[0]
	}
	acknowledgement, ok := projectRunProgressAcknowledgement(run, latest)
	if !ok {
		return nil, nil
	}
	prior, err := w.store.ListExternalConversationDeliveries(ctx, ExternalConversationDeliveryFilter{
		Scope: item.Scope, EndpointID: endpoint.ID, ConversationID: item.ConversationID,
		CorrelationKind: runProgressAcknowledgementCorrelationKind, CorrelationID: run.ID, Limit: 100,
	})
	if err != nil {
		return nil, err
	}
	for _, delivery := range prior {
		if delivery.Correlation != nil && delivery.Correlation.Phase == acknowledgement.Phase {
			return nil, nil
		}
	}
	if len(prior) > 0 && now.Sub(prior[0].CreatedAt) < w.config.MinimumInterval {
		return nil, nil
	}
	message, err := w.post(ctx, item, endpoint, acknowledgement)
	if err != nil {
		return nil, err
	}
	externalThreadID := strings.TrimSpace(item.Event.ExternalThreadID)
	if externalThreadID == "" && endpoint.Policy.ReplyMode == ExternalConversationReplyThread {
		externalThreadID = strings.TrimSpace(item.Event.ExternalMessageID)
	}
	enqueued, err := w.transport.Enqueue(ctx, EnqueueExternalConversationDeliveryRequest{
		Scope: item.Scope, EndpointID: endpoint.ID,
		Operation:      capability.ConversationDeliveryMessageSend,
		ConversationID: item.ConversationID, ChannelMessageID: message.ID,
		ExternalThreadID: externalThreadID,
		Correlation: &ExternalConversationDeliveryCorrelation{
			Kind: runProgressAcknowledgementCorrelationKind, ID: run.ID, Phase: acknowledgement.Phase,
		},
		IdempotencyKey: "run-acknowledgement-delivery:" + item.ID + ":" + acknowledgement.Phase,
	})
	if err != nil {
		return nil, err
	}
	return enqueued.Delivery, nil
}

func (w *RunProgressAcknowledgementWorker) post(
	ctx context.Context,
	item *ExternalConversationInboxItem,
	endpoint *ExternalConversationEndpoint,
	acknowledgement RunProgressAcknowledgement,
) (*ChannelMessage, error) {
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
			Audience:         ConversationAudience{Kind: ConversationAudienceChannel},
			ReplyToMessageID: item.ChannelMessageID, References: references,
			IdempotencyKey: "run-acknowledgement-message:" + item.ID + ":" + acknowledgement.Phase,
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

func projectRunProgressAcknowledgement(run *AgentRun, event *ActivityEvent) (RunProgressAcknowledgement, bool) {
	if run == nil || isTerminalAgentRunStatus(run.Status) {
		return RunProgressAcknowledgement{}, false
	}
	phase, content := acknowledgementForRunStatus(run.Status)
	if run.Status == AgentRunStatusRunning || run.Status == AgentRunStatusQueued {
		if eventPhase, eventContent := acknowledgementForActivity(event); eventContent != "" {
			phase, content = eventPhase, eventContent
		}
	}
	if fields := strings.Fields(content); phase == "" || len(fields) < 2 || len(fields) > 3 {
		return RunProgressAcknowledgement{}, false
	}
	result := RunProgressAcknowledgement{RunID: run.ID, Phase: phase, Text: content, SourceRevision: run.Revision}
	if event != nil {
		result.SourceEventID = event.ID
	}
	return result, true
}

func acknowledgementForRunStatus(status AgentRunStatus) (string, string) {
	switch status {
	case AgentRunStatusQueued:
		return "queued", "Getting started"
	case AgentRunStatusPlanning:
		return "planning", "Planning next steps"
	case AgentRunStatusRunning:
		return "running", "Working on it"
	case AgentRunStatusPaused:
		return "paused", "Work is paused"
	case AgentRunStatusSleeping:
		return "sleeping", "Trying again soon"
	case AgentRunStatusWaitingForDependency:
		return "waiting-dependency", "Waiting on task"
	case AgentRunStatusWaitingForAgent:
		return "waiting-agent", "Waiting on teammate"
	case AgentRunStatusWaitingForApproval:
		return "waiting-approval", "Waiting for approval"
	case AgentRunStatusWaitingForEvent:
		return "waiting-event", "Waiting for update"
	default:
		return "", ""
	}
}

func acknowledgementForActivity(event *ActivityEvent) (string, string) {
	if event == nil {
		return "", ""
	}
	switch event.EventType {
	case "run.claimed":
		return "starting", "Starting work"
	case "run.forked":
		return "forking", "Splitting up work"
	case "action.retry_scheduled", "turn.retry_scheduled":
		return "retrying", "Trying that again"
	case "action.approval_requested":
		return "waiting-approval", "Waiting for approval"
	case "action.human_intervention_required":
		return "waiting-human", "Need your help"
	case "dependency.group_waiting":
		return "waiting-dependency", "Waiting on tasks"
	case "collaboration.requested", "handoff.requested", "escalation.requested":
		return "delegating", "Delegating some work"
	case "workforce.generation.started":
		return "generating", "Drafting the plan"
	case "workforce.generation.phase":
		phase, _ := event.Payload["phase"].(string)
		switch phase {
		case "capability_resolve":
			return "resolving-capabilities", "Checking available tools"
		case "provider_request":
			return "provider-request", "Thinking this through"
		case "schema_repair", "contract_repair":
			return "repairing-candidate", "Refining the plan"
		case "candidate_validate":
			return "validating-candidate", "Checking the plan"
		}
	case "action.proposed":
		label := acknowledgementSkillLabel(event.Payload)
		if label != "" {
			skillID, _ := event.Payload["skillId"].(string)
			digest := hashString(strings.TrimSpace(skillID))
			return "using-" + digest[:12], "Using " + label
		}
		return "using-tool", "Using a tool"
	}
	return "", ""
}

func acknowledgementSkillLabel(payload map[string]interface{}) string {
	value, _ := payload["skillId"].(string)
	value = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(value), "skill-"))
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == '-' || r == '_' || r == '.' })
	if len(parts) == 0 {
		return ""
	}
	if len(parts) > 2 {
		parts = parts[:2]
	}
	for index := range parts {
		if parts[index] == "github" {
			parts[index] = "GitHub"
		} else {
			parts[index] = strings.ToUpper(parts[index][:1]) + parts[index][1:]
		}
	}
	return strings.Join(parts, " ")
}
