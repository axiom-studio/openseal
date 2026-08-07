package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

type ExternalConversationStore interface {
	ExternalConversationEndpointStore
	ExternalConversationGatewayStore
	ExternalConversationTransportStore
	ConversationStore
}

type ReceiveExternalConversationEventRequest struct {
	Scope           Scope
	EndpointID      string
	Event           NormalizedExternalConversationEvent
	MaximumAttempts int
}

type ReceiveExternalConversationEventResult struct {
	Item     *ExternalConversationInboxItem `json:"item"`
	Approval *ApprovalResolutionResult      `json:"approval,omitempty"`
	Accepted bool                           `json:"accepted"`
	Replayed bool                           `json:"replayed"`
}

type EnqueueExternalConversationDeliveryRequest struct {
	Scope            Scope
	EndpointID       string
	Operation        capability.ConversationDeliveryOperation
	ConversationID   string
	ChannelMessageID string
	ExternalThreadID string
	Parameters       map[string]interface{}
	Correlation      *ExternalConversationDeliveryCorrelation
	IdempotencyKey   string
	MaximumAttempts  int
}

type EnqueueExternalConversationDeliveryResult struct {
	Delivery *ExternalConversationDelivery `json:"delivery"`
	Replayed bool                          `json:"replayed"`
}

type ExternalConversationTransportService struct {
	store    ExternalConversationStore
	resolver ExternalConversationAdapterResolver
	now      func() time.Time
}

func NewExternalConversationTransportService(store ExternalConversationStore, resolver ExternalConversationAdapterResolver) *ExternalConversationTransportService {
	return &ExternalConversationTransportService{store: store, resolver: resolver, now: time.Now}
}

// Receive persists one already verified and normalized provider event. Policy
// filtering is itself durable, so retries of ignored bot or non-mention events
// remain idempotent and auditable without waking a cognitive handler.
func (s *ExternalConversationTransportService) Receive(ctx context.Context, req ReceiveExternalConversationEventRequest) (*ReceiveExternalConversationEventResult, error) {
	if s == nil || s.store == nil || s.resolver == nil {
		return nil, errors.New("external conversation transport service is not configured")
	}
	if err := req.Event.Validate(); err != nil {
		return nil, err
	}
	endpoint, resolved, err := s.resolveActiveEndpoint(ctx, req.Scope, req.EndpointID)
	if err != nil {
		return nil, err
	}
	if !containsConversationEventType(resolved.Adapter.InboundEventTypes, req.Event.Type) {
		return nil, fmt.Errorf("%w: adapter does not declare event type %q", ErrInvalidExternalConversation, req.Event.Type)
	}
	if req.Event.Type == capability.ConversationEventApprovalDecided {
		resolution, err := s.resolveExternalApprovalDecision(ctx, endpoint, req.Event)
		if err != nil {
			return nil, err
		}
		return &ReceiveExternalConversationEventResult{Approval: resolution, Accepted: true, Replayed: !resolution.Resolved}, nil
	}
	if endpoint.Mode == capability.ConversationEndpointDirect && !req.Event.Direct {
		return nil, fmt.Errorf("%w: direct endpoint received a non-direct event", ErrInvalidExternalConversation)
	}
	maximumAttempts := req.MaximumAttempts
	if maximumAttempts == 0 {
		maximumAttempts = 8
	}
	if maximumAttempts < 1 || maximumAttempts > MaximumExternalConversationDeliveryAttempts {
		return nil, ErrInvalidExternalConversation
	}
	now := s.now().UTC()
	accepted, summary := externalConversationPolicyAccepts(endpoint.Policy, req.Event)
	status := ExternalConversationInboxPending
	var appliedAt time.Time
	if !accepted {
		status, appliedAt = ExternalConversationInboxIgnored, now
	}
	item := &ExternalConversationInboxItem{
		ID:    stableExternalConversationID(req.Scope, endpoint.ID, "inbox", req.Event.ID),
		Scope: req.Scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision,
		Adapter: externalConversationResolvedAdapterReference(resolved),
		Event:   req.Event, Status: status, MaximumAttempts: maximumAttempts, AvailableAt: now,
		Summary: summary, Revision: 1, CreatedAt: now, UpdatedAt: now, AppliedAt: appliedAt,
	}
	stored, replayed, err := s.store.ReceiveExternalConversationEvent(ctx, item)
	if err != nil {
		return nil, err
	}
	return &ReceiveExternalConversationEventResult{
		Item: stored, Accepted: stored.Status != ExternalConversationInboxIgnored, Replayed: replayed,
	}, nil
}

func (s *ExternalConversationTransportService) resolveExternalApprovalDecision(
	ctx context.Context,
	endpoint *ExternalConversationEndpoint,
	event NormalizedExternalConversationEvent,
) (*ApprovalResolutionResult, error) {
	store, ok := s.store.(interface {
		PortfolioStore
		ActionStore
	})
	if !ok {
		return nil, errors.New("external approval coordination is unavailable")
	}
	approvalID, _ := event.Attributes["approvalId"].(string)
	actionCallID, _ := event.Attributes["actionCallId"].(string)
	invocationDigest, _ := event.Attributes["invocationDigest"].(string)
	decision, _ := event.Attributes["decision"].(string)
	principalType, _ := event.Attributes["principalType"].(string)
	principalID, _ := event.Attributes["principalId"].(string)
	reason, _ := event.Attributes["reason"].(string)
	revision, ok := integerAttribute(event.Attributes["approvalRevision"])
	if strings.TrimSpace(approvalID) == "" || strings.TrimSpace(actionCallID) == "" ||
		strings.TrimSpace(invocationDigest) == "" || revision < 1 || !ok ||
		(decision != "approve" && decision != "reject" && decision != "request_changes") ||
		strings.TrimSpace(principalType) == "" || strings.TrimSpace(principalID) == "" {
		return nil, fmt.Errorf("%w: approval decision attributes are invalid", ErrInvalidExternalConversation)
	}
	approval, err := store.GetApproval(ctx, endpoint.Scope, approvalID)
	if err != nil {
		return nil, err
	}
	call, err := store.GetActionCall(ctx, endpoint.Scope, approval.ActionCallID)
	if err != nil {
		return nil, err
	}
	replay := approval.Status != ApprovalStatusPending && approval.DecisionID == event.ID
	if approval.ActionCallID != actionCallID || call.InvocationDigest != invocationDigest ||
		(!replay && approval.Revision != revision) || !approvalHasDestination(approval, endpoint.ID) {
		return nil, fmt.Errorf("%w: approval decision does not match the reviewed action", ErrInvalidExternalConversation)
	}
	coordinator := NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{})
	resolution, err := coordinator.Resolve(ctx, ResolveApprovalRequest{
		Scope: endpoint.Scope, ApprovalID: approval.ID, ExpectedRevision: revision,
		DecisionID: event.ID, Decision: ApprovalDecision(decision),
		Principal: ApprovalPrincipal{Type: principalType, ID: principalID}, Reason: reason,
		CorrelationID: event.ExternalMessageID,
	})
	if err != nil {
		return nil, err
	}
	notificationStore, ok := s.store.(ApprovalNotificationStore)
	if !ok {
		return nil, errors.New("external approval notification coordination is unavailable")
	}
	providerApproverID, _ := event.Attributes["providerUserId"].(string)
	if err := enqueueApprovalCardUpdate(
		ctx, notificationStore, s, resolution.Approval, resolution.Call,
		ApprovalDestination{EndpointID: endpoint.ID}, providerApproverID,
	); err != nil {
		return nil, fmt.Errorf("enqueue approval card update: %w", err)
	}
	return resolution, nil
}

func integerAttribute(value interface{}) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		return int64(typed), typed == float64(int64(typed))
	default:
		return 0, false
	}
}

func approvalHasDestination(approval *ApprovalCheckpoint, endpointID string) bool {
	for _, destination := range approval.Destinations {
		if destination.EndpointID == endpointID {
			return true
		}
	}
	return false
}

// Enqueue projects one canonical ChannelMessage into the durable outbox. The
// body is intentionally not copied; the adapter worker reloads the canonical
// message immediately before delivery.
func (s *ExternalConversationTransportService) Enqueue(ctx context.Context, req EnqueueExternalConversationDeliveryRequest) (*EnqueueExternalConversationDeliveryResult, error) {
	if s == nil || s.store == nil || s.resolver == nil {
		return nil, errors.New("external conversation transport service is not configured")
	}
	endpoint, resolved, err := s.resolveActiveEndpoint(ctx, req.Scope, req.EndpointID)
	if err != nil {
		return nil, err
	}
	if !containsConversationDeliveryOperation(resolved.Adapter.Delivery.Operations, req.Operation) {
		return nil, fmt.Errorf("%w: adapter does not declare delivery operation %q", ErrInvalidExternalConversation, req.Operation)
	}
	message, err := s.store.GetChannelMessage(ctx, req.Scope, strings.TrimSpace(req.ConversationID), strings.TrimSpace(req.ChannelMessageID))
	if err != nil {
		return nil, err
	}
	if message == nil || message.ConversationID != strings.TrimSpace(req.ConversationID) || message.Historical ||
		message.Audience.Kind != ConversationAudienceChannel {
		return nil, fmt.Errorf("%w: delivery requires a current channel-visible canonical message", ErrInvalidExternalConversation)
	}
	if req.Operation != capability.ConversationDeliveryMessageSend && len(req.Parameters) == 0 {
		return nil, fmt.Errorf("%w: non-send delivery requires operation parameters", ErrInvalidExternalConversation)
	}
	// A thread-oriented endpoint may still originate a new channel message (for
	// example, an approval card). Only canonical replies require an existing
	// provider thread mapping; the provider response to a root send establishes
	// the mapping used by subsequent replies.
	if endpoint.Policy.ReplyMode == ExternalConversationReplyThread && message.ThreadRootID != "" && strings.TrimSpace(req.ExternalThreadID) == "" {
		return nil, fmt.Errorf("%w: thread reply requires an external thread mapping", ErrInvalidExternalConversation)
	}
	if err := validateExternalConversationConfiguration(req.Parameters); err != nil {
		return nil, err
	}
	maximumAttempts := req.MaximumAttempts
	if maximumAttempts == 0 {
		maximumAttempts = 8
	}
	if maximumAttempts < 1 || maximumAttempts > MaximumExternalConversationDeliveryAttempts {
		return nil, ErrInvalidExternalConversation
	}
	idempotencyKey := strings.TrimSpace(req.IdempotencyKey)
	if idempotencyKey == "" {
		idempotencyKey = "conversation-delivery:" + endpoint.ID + ":" + string(req.Operation) + ":" + message.ID
	}
	if !validOpaqueIdentifier(idempotencyKey, 512) {
		return nil, ErrInvalidExternalConversation
	}
	now := s.now().UTC()
	orderingKey := externalConversationDeliveryOrderingKey(
		req.Scope, endpoint.ID, resolved.Adapter.Delivery.Ordering, message.ConversationID, req.ExternalThreadID,
	)
	delivery := &ExternalConversationDelivery{
		ID:    stableExternalConversationID(req.Scope, endpoint.ID, "delivery", idempotencyKey),
		Scope: req.Scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision,
		Adapter:   externalConversationResolvedAdapterReference(resolved),
		Operation: req.Operation, ConversationID: message.ConversationID, ChannelMessageID: message.ID,
		ExternalThreadID: strings.TrimSpace(req.ExternalThreadID), OrderingKey: orderingKey,
		Parameters: cloneMap(req.Parameters), IdempotencyKey: idempotencyKey,
		Correlation: cloneExternalConversationDeliveryCorrelation(req.Correlation),
		Status:      ExternalConversationDeliveryPending, MaximumAttempts: maximumAttempts, AvailableAt: now,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	stored, replayed, err := s.store.EnqueueExternalConversationDelivery(ctx, delivery)
	if err != nil {
		return nil, err
	}
	return &EnqueueExternalConversationDeliveryResult{Delivery: stored, Replayed: replayed}, nil
}

func externalConversationDeliveryOrderingKey(
	scope Scope,
	endpointID string,
	ordering capability.ConversationDeliveryOrdering,
	conversationID, externalThreadID string,
) string {
	key := endpointID
	switch ordering {
	case capability.ConversationDeliveryOrderConversation:
		key += "\x00" + conversationID
	case capability.ConversationDeliveryOrderThread:
		key += "\x00" + conversationID + "\x00" + strings.TrimSpace(externalThreadID)
	}
	return stableExternalConversationID(scope, endpointID, "delivery-order", key)
}

func (s *ExternalConversationTransportService) resolveActiveEndpoint(ctx context.Context, scope Scope, endpointID string) (*ExternalConversationEndpoint, *skill.BoundConversationAdapter, error) {
	if err := scope.Validate(); err != nil {
		return nil, nil, err
	}
	endpoint, err := s.store.GetExternalConversationEndpoint(ctx, scope, strings.TrimSpace(endpointID))
	if err != nil {
		return nil, nil, err
	}
	if endpoint == nil {
		return nil, nil, ErrExternalConversationEndpointNotFound
	}
	if endpoint.Status != ExternalConversationEndpointActive {
		return nil, nil, ErrExternalConversationConflict
	}
	ref := endpoint.Adapter
	resolved, err := s.resolver.ResolveConversationAdapterBinding(
		ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, endpoint.DeploymentID,
		ref.BindingID, ref.AdapterID,
	)
	if err != nil || resolved == nil || resolved.Binding == nil ||
		resolved.Adapter.Provider != endpoint.Provider ||
		!containsConversationEndpointMode(resolved.Adapter.EndpointModes, endpoint.Mode) {
		return nil, nil, fmt.Errorf("%w: current Skill adapter binding is unavailable or incompatible", ErrExternalConversationConflict)
	}
	return endpoint, resolved, nil
}

// Live endpoints own the stable binding and adapter identities. Every inbox
// item and delivery captures the exact resolved Skill identity and binding
// revision used for that operation, so a compatible credential or policy
// amendment does not strand the endpoint while historical work remains exact.
func externalConversationResolvedAdapterReference(resolved *skill.BoundConversationAdapter) ExternalConversationAdapterReference {
	if resolved == nil || resolved.Definition == nil || resolved.Binding == nil {
		return ExternalConversationAdapterReference{}
	}
	return ExternalConversationAdapterReference{
		SkillID: resolved.Definition.ID, SkillVersion: resolved.Definition.Version,
		SourceIdentity: skill.DefinitionSourceIdentity(resolved.Definition),
		BindingID:      resolved.Binding.ID, BindingRevision: resolved.Binding.Revision,
		AdapterID: resolved.AdapterID,
	}
}

func externalConversationAdapterBelongsToEndpoint(endpoint, captured ExternalConversationAdapterReference) bool {
	return endpoint.BindingID == captured.BindingID && endpoint.AdapterID == captured.AdapterID
}

func externalConversationPolicyAccepts(policy ExternalConversationPolicy, event NormalizedExternalConversationEvent) (bool, string) {
	if policy.IgnoreBots && event.ParticipantIsBot {
		return false, "Ignored a provider bot message."
	}
	switch policy.MessageSelection {
	case ExternalConversationSelectMentions:
		if !event.MentionsEndpoint {
			return false, "Ignored a message that did not mention this endpoint."
		}
	case ExternalConversationSelectDirectOrMention:
		if !event.Direct && !event.MentionsEndpoint {
			return false, "Ignored a message that was neither direct nor an endpoint mention."
		}
	}
	return true, ""
}

func containsConversationEventType(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsConversationDeliveryOperation(values []capability.ConversationDeliveryOperation, wanted capability.ConversationDeliveryOperation) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func stableExternalConversationID(scope Scope, endpointID, kind, key string) string {
	seed := strings.Join([]string{"openseal", "external-conversation", scope.key(), endpointID, kind, key}, ":")
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(seed)).String()
}
