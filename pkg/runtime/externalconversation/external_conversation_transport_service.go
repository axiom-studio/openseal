package externalconversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

type ReceiveExternalConversationEventRequest struct {
	Scope           models.Scope
	EndpointID      string
	Event           models.NormalizedExternalConversationEvent
	MaximumAttempts int
}

type ReceiveExternalConversationEventResult struct {
	Item     *models.ExternalConversationInboxItem `json:"item"`
	Approval *models.ApprovalResolutionResult      `json:"approval,omitempty"`
	Accepted bool                                  `json:"accepted"`
	Replayed bool                                  `json:"replayed"`
}

type EnqueueExternalConversationDeliveryRequest struct {
	Scope                  models.Scope
	EndpointID             string
	Operation              capability.ConversationDeliveryOperation
	ConversationID         string
	ChannelMessageID       string
	ExternalConversationID string
	ExternalThreadID       string
	Parameters             map[string]interface{}
	Correlation            *models.ExternalConversationDeliveryCorrelation
	IdempotencyKey         string
	MaximumAttempts        int
}

type EnqueueExternalConversationDeliveryResult struct {
	Delivery *models.ExternalConversationDelivery `json:"delivery"`
	Replayed bool                                 `json:"replayed"`
}

type TransportService struct {
	store    models.ExternalConversationStore
	resolver AdapterResolver
	routines RoutineEventOfferer
	now      func() time.Time
}

func NewTransportService(store models.ExternalConversationStore, resolver AdapterResolver, opts ...models.ServiceOption) *TransportService {
	return &TransportService{store: store, resolver: resolver, now: models.ClockFrom(opts)}
}

// SetRoutineEventOfferer lets verified Slack and Telegram messages start event
// routines. Set it before ingress starts; nil turns the routine path off.
func (s *TransportService) SetRoutineEventOfferer(offerer RoutineEventOfferer) {
	if s != nil {
		s.routines = offerer
	}
}

// Receive persists one already verified and normalized provider event. Policy
// filtering is itself durable, so retries of ignored bot or non-mention events
// remain idempotent and auditable without waking a cognitive handler.
func (s *TransportService) Receive(ctx context.Context, req ReceiveExternalConversationEventRequest) (*ReceiveExternalConversationEventResult, error) {
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
	if req.Event.Type == capability.ConversationEventApprovalDecided {
		resolution, err := s.resolveExternalApprovalDecision(ctx, endpoint, req.Event)
		if err != nil {
			return nil, err
		}
		return &ReceiveExternalConversationEventResult{Approval: resolution, Accepted: true, Replayed: !resolution.Resolved}, nil
	}
	// Approval decisions are authenticated control-plane responses to canonical
	// review cards, not messages routed to an Agent handler. Their authority is
	// checked against the exact invocation and origin above, independently of
	// the adapter's declared conversational inbound event types.
	if !containsConversationEventType(resolved.Adapter.InboundEventTypes, req.Event.Type) {
		return nil, fmt.Errorf("%w: adapter does not declare event type %q", models.ErrInvalidExternalConversation, req.Event.Type)
	}
	if endpoint.Mode == capability.ConversationEndpointDirect && !req.Event.Direct {
		return nil, fmt.Errorf("%w: direct endpoint received a non-direct event", models.ErrInvalidExternalConversation)
	}
	maximumAttempts := req.MaximumAttempts
	if maximumAttempts == 0 {
		maximumAttempts = 8
	}
	if maximumAttempts < 1 || maximumAttempts > models.MaximumExternalConversationDeliveryAttempts {
		return nil, models.ErrInvalidExternalConversation
	}
	now := s.now().UTC()
	accepted, summary := externalConversationPolicyAccepts(endpoint.Policy, req.Event)
	status := models.ExternalConversationInboxPending
	var appliedAt time.Time
	if !accepted {
		status, appliedAt = models.ExternalConversationInboxIgnored, now
	}
	item := &models.ExternalConversationInboxItem{
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
		Item: stored, Accepted: stored.Status != models.ExternalConversationInboxIgnored, Replayed: replayed,
	}, nil
}

func (s *TransportService) resolveExternalApprovalDecision(
	ctx context.Context,
	endpoint *models.ExternalConversationEndpoint,
	event models.NormalizedExternalConversationEvent,
) (*models.ApprovalResolutionResult, error) {
	store, ok := s.store.(interface {
		models.PortfolioStore
		models.ActionStore
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
		return nil, fmt.Errorf("%w: approval decision attributes are invalid", models.ErrInvalidExternalConversation)
	}
	approval, err := store.GetApproval(ctx, endpoint.Scope, approvalID)
	if err != nil {
		return nil, err
	}
	call, err := store.GetActionCall(ctx, endpoint.Scope, approval.ActionCallID)
	if err != nil {
		return nil, err
	}
	replay := approval.Status != models.ApprovalStatusPending && approval.DecisionID == event.ID
	originReview := false
	destination := approvalHasDestination(approval, endpoint.ID)
	if !destination || principalType == "external_participant" {
		originReview, err = s.approvalOriginDestination(ctx, approval, endpoint)
		if err != nil {
			return nil, err
		}
		destination = destination || originReview
	}
	if approval.ActionCallID != actionCallID || call.InvocationDigest != invocationDigest ||
		(!replay && approval.Revision != revision) || !destination {
		return nil, fmt.Errorf("%w: approval decision does not match the reviewed action", models.ErrInvalidExternalConversation)
	}
	var authorizer action.ApprovalAuthorizer = action.EligibleApprovalAuthorizer{}
	if originReview && principalType == "external_participant" {
		if strings.TrimSpace(event.ExternalParticipantID) == "" || principalID != event.ExternalParticipantID {
			return nil, fmt.Errorf("%w: approval participant does not match the source event", models.ErrInvalidExternalConversation)
		}
		// Source conversation members may decide without an app account mapping.
		// The adapter authenticates the provider identity; the durable imported
		// trigger above limits this authority to the originating conversation.
		authorizer = action.ApprovalAuthorizerFunc(func(context.Context, models.ApprovalPrincipal, *models.ApprovalCheckpoint) error { return nil })
	}
	// Neither authorizer above is an AgentBuilderApprovalAuthorizer, so an
	// external channel reply can reject but never approve an agent-builder
	// card (D11); those cards also get no external destinations.
	coordinator := action.NewApprovalCoordinator(store, store, authorizer, models.WithClock(s.now))
	resolution, err := coordinator.Resolve(ctx, action.ResolveApprovalRequest{
		Scope: endpoint.Scope, ApprovalID: approval.ID, ExpectedRevision: revision,
		DecisionID: event.ID, Decision: models.ApprovalDecision(decision),
		Principal: models.ApprovalPrincipal{Type: principalType, ID: principalID}, Reason: reason,
		CorrelationID: event.ExternalMessageID,
	})
	if err != nil {
		return nil, err
	}
	notificationStore, ok := s.store.(models.ApprovalNotificationStore)
	if !ok {
		return nil, errors.New("external approval notification coordination is unavailable")
	}
	providerApproverID, _ := event.Attributes["providerUserId"].(string)
	if err := enqueueApprovalCardUpdate(
		ctx, notificationStore, s, resolution.Approval, resolution.Call,
		models.ApprovalDestination{EndpointID: endpoint.ID}, providerApproverID,
	); err != nil {
		return nil, fmt.Errorf("enqueue approval card update: %w", err)
	}
	return resolution, nil
}

// jsonIntegerBounded refuses numbers whose text or exponent is too large to
// parse cheaply; no int64 needs more than this.
func jsonIntegerBounded(number json.Number) bool {
	data := string(number)
	if len(data) > 64 {
		return false
	}
	if at := strings.IndexAny(data, "eE"); at >= 0 {
		exponent, err := strconv.ParseInt(data[at+1:], 10, 32)
		if err != nil || exponent < -64 || exponent > 64 {
			return false
		}
	}
	return true
}

func integerAttribute(value interface{}) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		return int64(typed), typed == float64(int64(typed))
	case json.Number:
		if !jsonIntegerBounded(typed) {
			return 0, false
		}
		value, ok := new(big.Rat).SetString(string(typed))
		if !ok || !value.IsInt() || !value.Num().IsInt64() {
			return 0, false
		}
		return value.Num().Int64(), true
	default:
		return 0, false
	}
}

func approvalHasDestination(approval *models.ApprovalCheckpoint, endpointID string) bool {
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
func (s *TransportService) Enqueue(ctx context.Context, req EnqueueExternalConversationDeliveryRequest) (*EnqueueExternalConversationDeliveryResult, error) {
	if s == nil || s.store == nil || s.resolver == nil {
		return nil, errors.New("external conversation transport service is not configured")
	}
	endpoint, resolved, err := s.resolveActiveEndpoint(ctx, req.Scope, req.EndpointID)
	if err != nil {
		return nil, err
	}
	if !containsConversationDeliveryOperation(resolved.Adapter.Delivery.Operations, req.Operation) {
		return nil, fmt.Errorf("%w: adapter does not declare delivery operation %q", models.ErrInvalidExternalConversation, req.Operation)
	}
	message, err := s.store.GetChannelMessage(ctx, req.Scope, strings.TrimSpace(req.ConversationID), strings.TrimSpace(req.ChannelMessageID))
	if err != nil {
		return nil, err
	}
	if message == nil || message.ConversationID != strings.TrimSpace(req.ConversationID) || message.Historical ||
		message.Audience.Kind != models.ConversationAudienceChannel {
		return nil, fmt.Errorf("%w: delivery requires a current channel-visible canonical message", models.ErrInvalidExternalConversation)
	}
	if req.Operation != capability.ConversationDeliveryMessageSend && len(req.Parameters) == 0 {
		return nil, fmt.Errorf("%w: non-send delivery requires operation parameters", models.ErrInvalidExternalConversation)
	}
	if req.ExternalConversationID != "" && !models.IsValidExternalConversationReference(strings.TrimSpace(req.ExternalConversationID), 1024) {
		return nil, models.ErrInvalidExternalConversation
	}
	// A thread-oriented endpoint may still originate a new channel message (for
	// example, an approval card). Only canonical replies require an existing
	// provider thread mapping; the provider response to a root send establishes
	// the mapping used by subsequent replies.
	if endpoint.Policy.ReplyMode == models.ExternalConversationReplyThread && message.ThreadRootID != "" && strings.TrimSpace(req.ExternalThreadID) == "" {
		return nil, fmt.Errorf("%w: thread reply requires an external thread mapping", models.ErrInvalidExternalConversation)
	}
	if err := models.ValidateExternalConversationConfiguration(req.Parameters); err != nil {
		return nil, err
	}
	maximumAttempts := req.MaximumAttempts
	if maximumAttempts == 0 {
		maximumAttempts = 8
	}
	if maximumAttempts < 1 || maximumAttempts > models.MaximumExternalConversationDeliveryAttempts {
		return nil, models.ErrInvalidExternalConversation
	}
	idempotencyKey := strings.TrimSpace(req.IdempotencyKey)
	if idempotencyKey == "" {
		idempotencyKey = "conversation-delivery:" + endpoint.ID + ":" + string(req.Operation) + ":" + message.ID
	}
	if !models.IsValidOpaqueIdentifier(idempotencyKey, 512) {
		return nil, models.ErrInvalidExternalConversation
	}
	now := s.now().UTC()
	orderingKey := externalConversationDeliveryOrderingKey(
		req.Scope, endpoint.ID, resolved.Adapter.Delivery.Ordering, message.ConversationID, req.ExternalThreadID,
	)
	delivery := &models.ExternalConversationDelivery{
		ID:    stableExternalConversationID(req.Scope, endpoint.ID, "delivery", idempotencyKey),
		Scope: req.Scope, EndpointID: endpoint.ID, EndpointRevision: endpoint.Revision,
		Adapter:   externalConversationResolvedAdapterReference(resolved),
		Operation: req.Operation, ConversationID: message.ConversationID, ChannelMessageID: message.ID,
		ExternalConversationID: strings.TrimSpace(req.ExternalConversationID),
		ExternalThreadID:       strings.TrimSpace(req.ExternalThreadID), OrderingKey: orderingKey,
		Parameters: maps.Clone(req.Parameters), IdempotencyKey: idempotencyKey,
		Correlation: cloneExternalConversationDeliveryCorrelation(req.Correlation),
		Status:      models.ExternalConversationDeliveryPending, MaximumAttempts: maximumAttempts, AvailableAt: now,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	stored, replayed, err := s.store.EnqueueExternalConversationDelivery(ctx, delivery)
	if err != nil {
		return nil, err
	}
	return &EnqueueExternalConversationDeliveryResult{Delivery: stored, Replayed: replayed}, nil
}

func externalConversationDeliveryOrderingKey(
	scope models.Scope,
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

func (s *TransportService) resolveActiveEndpoint(ctx context.Context, scope models.Scope, endpointID string) (*models.ExternalConversationEndpoint, *skill.BoundConversationAdapter, error) {
	if err := scope.Validate(); err != nil {
		return nil, nil, err
	}
	endpoint, err := s.store.GetExternalConversationEndpoint(ctx, scope, strings.TrimSpace(endpointID))
	if err != nil {
		return nil, nil, err
	}
	if endpoint == nil {
		return nil, nil, models.ErrExternalConversationEndpointNotFound
	}
	if endpoint.Status != models.ExternalConversationEndpointActive {
		return nil, nil, models.ErrExternalConversationConflict
	}
	ref := endpoint.Adapter
	resolved, err := s.resolver.ResolveConversationAdapterBinding(
		ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, endpoint.DeploymentID,
		ref.BindingID, ref.AdapterID,
	)
	if err != nil || resolved == nil || resolved.Binding == nil ||
		resolved.Binding.SkillID != ref.SkillID || resolved.Binding.SourceIdentity != ref.SourceIdentity ||
		resolved.Binding.Revision < ref.BindingRevision ||
		resolved.Adapter.Provider != endpoint.Provider ||
		!slices.Contains(resolved.Adapter.EndpointModes, endpoint.Mode) {
		return nil, nil, fmt.Errorf("%w: current Skill adapter binding is unavailable or incompatible", models.ErrExternalConversationConflict)
	}
	return endpoint, resolved, nil
}

// Live endpoints own the stable binding and adapter identities. Every inbox
// item and delivery captures the exact resolved Skill identity and binding
// revision used for that operation, so a compatible credential or policy
// amendment does not strand the endpoint while historical work remains exact.
func externalConversationResolvedAdapterReference(resolved *skill.BoundConversationAdapter) models.ExternalConversationAdapterReference {
	if resolved == nil || resolved.Definition == nil || resolved.Binding == nil {
		return models.ExternalConversationAdapterReference{}
	}
	return models.ExternalConversationAdapterReference{
		SkillID: resolved.Definition.ID, SkillVersion: resolved.Definition.Version,
		SourceIdentity: skill.DefinitionSourceIdentity(resolved.Definition),
		BindingID:      resolved.Binding.ID, BindingRevision: resolved.Binding.Revision,
		AdapterID: resolved.AdapterID,
	}
}

func externalConversationPolicyAccepts(policy models.ExternalConversationPolicy, event models.NormalizedExternalConversationEvent) (bool, string) {
	if event.Type == capability.ConversationEventReactionAdded || event.Type == capability.ConversationEventReactionRemoved {
		// Reactions never address the endpoint, so message selection does
		// not apply. Bot reactions include this endpoint's own mirrored
		// reactions and are never imported back.
		if event.ParticipantIsBot {
			return false, "Ignored a provider bot reaction."
		}
		return true, ""
	}
	if policy.IgnoreBots && event.ParticipantIsBot {
		return false, "Ignored a provider bot message."
	}
	switch policy.MessageSelection {
	case models.ExternalConversationSelectMentions:
		if !event.MentionsEndpoint {
			return false, "Ignored a message that did not mention this endpoint."
		}
	case models.ExternalConversationSelectDirectOrMention:
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

func stableExternalConversationID(scope models.Scope, endpointID, kind, key string) string {
	seed := strings.Join([]string{"openseal", "external-conversation", scope.Key(), endpointID, kind, key}, ":")
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(seed)).String()
}

// An originating chat is a review destination only when the durable provider
// inbox proves which endpoint started this run.
func (s *TransportService) approvalOriginDestination(ctx context.Context, approval *models.ApprovalCheckpoint, endpoint *models.ExternalConversationEndpoint) (bool, error) {
	store := s.store.(interface {
		models.ExternalConversationStore
		models.ActionStore
		models.PortfolioStore
	})
	run, err := store.GetAgentRun(ctx, approval.Scope, approval.RunID)
	if err != nil {
		return false, err
	}
	if run == nil || run.Owner != endpoint.Owner {
		return false, nil
	}
	rootID := run.RootRunID
	if rootID == "" {
		rootID = run.ID
	}
	root, err := store.GetAgentRun(ctx, approval.Scope, rootID)
	if err != nil {
		return false, err
	}
	if root == nil {
		return false, nil
	}
	conversationID, _ := root.Context["conversationId"].(string)
	triggerID, _ := root.Context["triggerMessageId"].(string)
	if conversationID == "" || triggerID == "" {
		return false, nil
	}
	conversation, err := store.GetConversation(ctx, approval.Scope, conversationID)
	if err != nil {
		return false, err
	}
	if conversation == nil || conversation.Owner != endpoint.Owner || conversation.Origin == nil || conversation.Origin.Kind != models.ConversationReferenceExternalSource || conversation.Origin.ID != endpoint.ID {
		return false, nil
	}
	for offset := 0; ; offset += 100 {
		items, err := store.ListExternalConversationInbox(ctx, models.ExternalConversationInboxFilter{Scope: approval.Scope, EndpointID: endpoint.ID, Statuses: []models.ExternalConversationInboxStatus{models.ExternalConversationInboxApplied}, Limit: 100, Offset: offset})
		if err != nil {
			return false, err
		}
		for _, item := range items {
			if item.ConversationID == conversationID && item.ChannelMessageID == triggerID && item.RunID == rootID {
				return true, nil
			}
		}
		if len(items) < 100 {
			return false, nil
		}
	}
}
