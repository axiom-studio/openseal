package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

const (
	MaximumExternalConversationConfigurationBytes = 64 * 1024
	MaximumExternalConversationDeliveryAttempts   = 100
)

var (
	ErrExternalConversationEndpointNotFound = errors.New("external conversation endpoint not found")
	ErrExternalConversationConflict         = errors.New("external conversation revision conflict")
	ErrExternalConversationInboxNotFound    = errors.New("external conversation inbox item not found")
	ErrExternalConversationDeliveryNotFound = errors.New("external conversation delivery not found")
	ErrInvalidExternalConversation          = errors.New("invalid external conversation state")
	ErrExternalConversationLeaseLost        = errors.New("external conversation lease was lost")
)

type ExternalConversationEndpointStatus string

const (
	ExternalConversationEndpointActive  ExternalConversationEndpointStatus = "active"
	ExternalConversationEndpointPaused  ExternalConversationEndpointStatus = "paused"
	ExternalConversationEndpointRetired ExternalConversationEndpointStatus = "retired"
)

type ExternalConversationHandlerKind string

const (
	ExternalConversationHandlerAgent   ExternalConversationHandlerKind = "agent"
	ExternalConversationHandlerTeam    ExternalConversationHandlerKind = "team"
	ExternalConversationHandlerRunbook ExternalConversationHandlerKind = "runbook"
)

// ExternalConversationHandler selects the portable cognitive handler. A
// Runbook may deterministically orchestrate one or more bounded Agent or Team
// runs; provider-specific behavior never belongs in the handler.
type ExternalConversationHandler struct {
	Kind            ExternalConversationHandlerKind `json:"kind"`
	ID              string                          `json:"id"`
	Version         string                          `json:"version,omitempty"`
	Trigger         string                          `json:"trigger,omitempty"`
	AssignedAgentID string                          `json:"assignedAgentId,omitempty"`
}

func (h ExternalConversationHandler) Validate(owner ObjectiveOwner) error {
	if !validAgentReference(strings.TrimSpace(h.ID), 256) {
		return fmt.Errorf("%w: handler id is invalid", ErrInvalidExternalConversation)
	}
	switch h.Kind {
	case ExternalConversationHandlerAgent:
		if h.Version != "" || h.Trigger != "" || h.AssignedAgentID != "" ||
			owner.Type != OwnerTypeAgent || h.ID != owner.ID {
			return fmt.Errorf("%w: Agent handler must identify the endpoint owner", ErrInvalidExternalConversation)
		}
	case ExternalConversationHandlerTeam:
		if h.Version != "" || h.Trigger != "" || h.AssignedAgentID != "" ||
			owner.Type != OwnerTypeTeam || h.ID != owner.ID {
			return fmt.Errorf("%w: Team handler must identify the endpoint owner", ErrInvalidExternalConversation)
		}
	case ExternalConversationHandlerRunbook:
		if strings.TrimSpace(h.Version) == "" || len(h.Version) > 128 ||
			!validOpaqueIdentifier(strings.TrimSpace(h.Trigger), 128) ||
			!validAgentReference(strings.TrimSpace(h.AssignedAgentID), 256) {
			return fmt.Errorf("%w: Runbook handler requires an exact version, trigger, and assigned Agent deployment", ErrInvalidExternalConversation)
		}
	default:
		return fmt.Errorf("%w: handler kind is invalid", ErrInvalidExternalConversation)
	}
	return nil
}

type ExternalConversationMessageSelection string

const (
	ExternalConversationSelectAllMessages     ExternalConversationMessageSelection = "all_messages"
	ExternalConversationSelectMentions        ExternalConversationMessageSelection = "mentions"
	ExternalConversationSelectDirectOrMention ExternalConversationMessageSelection = "direct_or_mentions"
)

type ExternalConversationReplyMode string

const (
	ExternalConversationReplyProviderDefault ExternalConversationReplyMode = "provider_default"
	ExternalConversationReplyThread          ExternalConversationReplyMode = "thread"
	ExternalConversationReplyChannel         ExternalConversationReplyMode = "channel"
)

type ExternalConversationPolicy struct {
	MessageSelection ExternalConversationMessageSelection `json:"messageSelection"`
	ReplyMode        ExternalConversationReplyMode        `json:"replyMode"`
	IgnoreBots       bool                                 `json:"ignoreBots"`
}

func (p ExternalConversationPolicy) Validate(mode capability.ConversationEndpointMode, features []capability.ConversationAdapterFeature) error {
	switch p.MessageSelection {
	case ExternalConversationSelectAllMessages, ExternalConversationSelectMentions, ExternalConversationSelectDirectOrMention:
	default:
		return fmt.Errorf("%w: message selection is invalid", ErrInvalidExternalConversation)
	}
	switch p.ReplyMode {
	case ExternalConversationReplyProviderDefault, ExternalConversationReplyThread, ExternalConversationReplyChannel:
	default:
		return fmt.Errorf("%w: reply mode is invalid", ErrInvalidExternalConversation)
	}
	if mode == capability.ConversationEndpointDirect && p.MessageSelection == ExternalConversationSelectMentions {
		return fmt.Errorf("%w: direct endpoints cannot require mentions", ErrInvalidExternalConversation)
	}
	if p.ReplyMode == ExternalConversationReplyThread && !hasConversationFeature(features, capability.ConversationFeatureThreads) {
		return fmt.Errorf("%w: selected adapter does not support threads", ErrInvalidExternalConversation)
	}
	return nil
}

// ExternalConversationAdapterReference pins desired endpoint state to the
// exact immutable Skill implementation and opaque binding revision reviewed by
// the operator. Credential values remain entirely out of this record.
type ExternalConversationAdapterReference struct {
	SkillID         string `json:"skillId"`
	SkillVersion    string `json:"skillVersion"`
	SourceIdentity  string `json:"sourceIdentity,omitempty"`
	BindingID       string `json:"bindingId"`
	BindingRevision int64  `json:"bindingRevision"`
	AdapterID       string `json:"adapterId"`
}

func (r ExternalConversationAdapterReference) Validate() error {
	for _, value := range []string{r.SkillID, r.SkillVersion, r.BindingID, r.AdapterID} {
		if !validOpaqueIdentifier(strings.TrimSpace(value), 256) {
			return fmt.Errorf("%w: exact Skill adapter reference is invalid", ErrInvalidExternalConversation)
		}
	}
	if r.SourceIdentity != "" && (len(r.SourceIdentity) > 1024 || strings.ContainsAny(r.SourceIdentity, "\r\n")) {
		return fmt.Errorf("%w: Skill source identity is invalid", ErrInvalidExternalConversation)
	}
	if r.BindingRevision < 1 {
		return fmt.Errorf("%w: exact Skill binding revision is required", ErrInvalidExternalConversation)
	}
	return nil
}

// ExternalConversationEndpoint is provider-neutral desired state. Address is
// an opaque provider resource such as a channel, inbox, number, or web-chat
// installation; Configuration is non-secret and interpreted only by the Skill.
type ExternalConversationEndpoint struct {
	ID             string                               `json:"id"`
	IngressRoute   string                               `json:"ingressRoute"`
	Scope          Scope                                `json:"scope"`
	Owner          ObjectiveOwner                       `json:"owner"`
	DeploymentID   string                               `json:"deploymentId"`
	Name           string                               `json:"name"`
	Adapter        ExternalConversationAdapterReference `json:"adapter"`
	Provider       string                               `json:"provider"`
	Mode           capability.ConversationEndpointMode  `json:"mode"`
	InstallationID string                               `json:"installationId,omitempty"`
	ApplicationID  string                               `json:"applicationId,omitempty"`
	Address        string                               `json:"address,omitempty"`
	Handler        ExternalConversationHandler          `json:"handler"`
	Policy         ExternalConversationPolicy           `json:"policy"`
	Configuration  map[string]interface{}               `json:"configuration,omitempty"`
	Status         ExternalConversationEndpointStatus   `json:"status"`
	Revision       int64                                `json:"revision"`
	CreatedAt      time.Time                            `json:"createdAt"`
	UpdatedAt      time.Time                            `json:"updatedAt"`
	RetiredAt      *time.Time                           `json:"retiredAt,omitempty"`
}

func (e *ExternalConversationEndpoint) Validate() error {
	if e == nil || e.Scope.Validate() != nil || e.Owner.Validate() != nil ||
		!validOpaqueIdentifier(strings.TrimSpace(e.ID), 256) ||
		!validOpaqueIdentifier(strings.TrimSpace(e.IngressRoute), 128) ||
		!validAgentReference(strings.TrimSpace(e.DeploymentID), 256) ||
		strings.TrimSpace(e.Name) == "" || len(e.Name) > 160 ||
		!validOpaqueIdentifier(e.Provider, 128) ||
		len(e.InstallationID) > 1024 || strings.ContainsAny(e.InstallationID, "\r\n") ||
		len(e.ApplicationID) > 1024 || strings.ContainsAny(e.ApplicationID, "\r\n") ||
		len(e.Address) > 1024 || strings.ContainsAny(e.Address, "\r\n") ||
		e.Revision < 1 || e.CreatedAt.IsZero() || e.UpdatedAt.IsZero() || e.UpdatedAt.Before(e.CreatedAt) {
		return ErrInvalidExternalConversation
	}
	if e.DeploymentID != e.Owner.ID {
		return fmt.Errorf("%w: endpoint deployment must be its owner", ErrInvalidExternalConversation)
	}
	if err := e.Adapter.Validate(); err != nil {
		return err
	}
	if err := e.Handler.Validate(e.Owner); err != nil {
		return err
	}
	if e.Mode != capability.ConversationEndpointChannel && e.Mode != capability.ConversationEndpointDirect {
		return fmt.Errorf("%w: endpoint mode is invalid", ErrInvalidExternalConversation)
	}
	switch e.Status {
	case ExternalConversationEndpointActive, ExternalConversationEndpointPaused:
		if e.RetiredAt != nil {
			return fmt.Errorf("%w: non-retired endpoint has retiredAt", ErrInvalidExternalConversation)
		}
	case ExternalConversationEndpointRetired:
		if e.RetiredAt == nil || e.RetiredAt.Before(e.CreatedAt) {
			return fmt.Errorf("%w: retired endpoint requires a valid retiredAt", ErrInvalidExternalConversation)
		}
	default:
		return fmt.Errorf("%w: endpoint status is invalid", ErrInvalidExternalConversation)
	}
	if err := validateExternalConversationConfiguration(e.Configuration); err != nil {
		return err
	}
	return nil
}

type ExternalConversationEndpointFilter struct {
	Scope    Scope
	Owner    *ObjectiveOwner
	Statuses []ExternalConversationEndpointStatus
	Provider string
	Limit    int
	Offset   int
}

type CreateExternalConversationEndpointRequest struct {
	ID            string
	Scope         Scope
	Owner         ObjectiveOwner
	DeploymentID  string
	Name          string
	Adapter       ExternalConversationAdapterReference
	Mode          capability.ConversationEndpointMode
	Address       string
	Handler       ExternalConversationHandler
	Policy        ExternalConversationPolicy
	Configuration map[string]interface{}
	Status        ExternalConversationEndpointStatus
}

type UpdateExternalConversationEndpointRequest struct {
	ExpectedRevision int64
	Adapter          *ExternalConversationAdapterReference
	Name             *string
	Address          *string
	Handler          *ExternalConversationHandler
	Policy           *ExternalConversationPolicy
	Configuration    map[string]interface{}
	ReplaceConfig    bool
	Status           *ExternalConversationEndpointStatus
}

type ExternalConversationEndpointStore interface {
	CreateExternalConversationEndpoint(context.Context, *ExternalConversationEndpoint) error
	GetExternalConversationEndpoint(context.Context, Scope, string) (*ExternalConversationEndpoint, error)
	GetExternalConversationEndpointByIngressRoute(context.Context, string) (*ExternalConversationEndpoint, error)
	ListExternalConversationEndpoints(context.Context, ExternalConversationEndpointFilter) ([]*ExternalConversationEndpoint, error)
	ListExternalConversationEndpointsByVerifiedRoute(context.Context, ExternalConversationVerifiedRoute) ([]*ExternalConversationEndpoint, error)
	UpdateExternalConversationEndpoint(context.Context, *ExternalConversationEndpoint, int64) error
}

type ExternalConversationAdapterResolver interface {
	ResolveConversationAdapter(context.Context, skill.ScopeReference, string, string, string, string, ...skill.BindingReference) (*skill.BoundConversationAdapter, error)
	ResolveConversationAdapterBinding(context.Context, skill.ScopeReference, string, string, string) (*skill.BoundConversationAdapter, error)
}

type ExternalConversationEndpointService struct {
	store    ExternalConversationEndpointStore
	resolver ExternalConversationAdapterResolver
	now      func() time.Time
	newID    func() string
}

func NewExternalConversationEndpointService(store ExternalConversationEndpointStore, resolver ExternalConversationAdapterResolver) *ExternalConversationEndpointService {
	return &ExternalConversationEndpointService{store: store, resolver: resolver, now: time.Now, newID: uuid.NewString}
}

func (s *ExternalConversationEndpointService) Create(ctx context.Context, req CreateExternalConversationEndpointRequest) (*ExternalConversationEndpoint, error) {
	if s == nil || s.store == nil || s.resolver == nil {
		return nil, errors.New("external conversation endpoint service is not configured")
	}
	status := req.Status
	if status == "" {
		status = ExternalConversationEndpointPaused
	}
	now := s.now().UTC()
	endpoint := &ExternalConversationEndpoint{
		ID: strings.TrimSpace(req.ID), Scope: req.Scope, Owner: req.Owner, DeploymentID: strings.TrimSpace(req.DeploymentID),
		Name: strings.TrimSpace(req.Name), Adapter: normalizeExternalConversationAdapterReference(req.Adapter),
		Mode: req.Mode, Address: strings.TrimSpace(req.Address), Handler: normalizeExternalConversationHandler(req.Handler),
		Policy: req.Policy, Configuration: cloneMap(req.Configuration), Status: status,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if endpoint.ID == "" {
		endpoint.ID = "conversation-endpoint:" + s.newID()
	}
	endpoint.IngressRoute = s.newID()
	resolved, err := s.resolveAdapter(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	endpoint.Provider = resolved.Adapter.Provider
	if err := endpoint.Policy.Validate(endpoint.Mode, resolved.Adapter.Features); err != nil {
		return nil, err
	}
	if err := endpoint.Validate(); err != nil {
		return nil, err
	}
	if err := s.store.CreateExternalConversationEndpoint(ctx, endpoint); err != nil {
		return nil, err
	}
	return cloneExternalConversationEndpoint(endpoint), nil
}

func (s *ExternalConversationEndpointService) Get(ctx context.Context, scope Scope, id string) (*ExternalConversationEndpoint, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("external conversation endpoint service is not configured")
	}
	endpoint, err := s.store.GetExternalConversationEndpoint(ctx, scope, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if endpoint == nil {
		return nil, ErrExternalConversationEndpointNotFound
	}
	return endpoint, nil
}

func (s *ExternalConversationEndpointService) List(ctx context.Context, filter ExternalConversationEndpointFilter) ([]*ExternalConversationEndpoint, error) {
	if s == nil || s.store == nil || filter.Scope.Validate() != nil || filter.Offset < 0 {
		return nil, ErrInvalidExternalConversation
	}
	if filter.Owner != nil && filter.Owner.Validate() != nil {
		return nil, ErrInvalidExternalConversation
	}
	for _, status := range filter.Statuses {
		if !validExternalConversationEndpointStatus(status) {
			return nil, ErrInvalidExternalConversation
		}
	}
	filter.Provider = strings.TrimSpace(filter.Provider)
	if filter.Provider != "" && !validOpaqueIdentifier(filter.Provider, 128) {
		return nil, ErrInvalidExternalConversation
	}
	if filter.Limit <= 0 || filter.Limit > 500 {
		filter.Limit = 50
	}
	return s.store.ListExternalConversationEndpoints(ctx, filter)
}

func (s *ExternalConversationEndpointService) Update(ctx context.Context, scope Scope, id string, req UpdateExternalConversationEndpointRequest) (*ExternalConversationEndpoint, error) {
	if s == nil || s.store == nil || s.resolver == nil || req.ExpectedRevision < 1 {
		return nil, ErrInvalidExternalConversation
	}
	current, err := s.Get(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if current.Revision != req.ExpectedRevision {
		return nil, ErrExternalConversationConflict
	}
	next := cloneExternalConversationEndpoint(current)
	if req.Adapter != nil {
		next.Adapter = normalizeExternalConversationAdapterReference(*req.Adapter)
	}
	if req.Name != nil {
		next.Name = strings.TrimSpace(*req.Name)
	}
	if req.Address != nil {
		next.Address = strings.TrimSpace(*req.Address)
	}
	if req.Handler != nil {
		next.Handler = normalizeExternalConversationHandler(*req.Handler)
	}
	if req.Policy != nil {
		next.Policy = *req.Policy
	}
	if req.ReplaceConfig {
		next.Configuration = cloneMap(req.Configuration)
	}
	if req.Status != nil {
		if current.Status == ExternalConversationEndpointRetired && *req.Status != ExternalConversationEndpointRetired {
			return nil, ErrInvalidExternalConversation
		}
		next.Status = *req.Status
	}
	now := s.now().UTC()
	if next.Status == ExternalConversationEndpointRetired && current.Status != ExternalConversationEndpointRetired {
		next.RetiredAt = &now
	}
	next.Revision++
	next.UpdatedAt = now
	resolved, err := s.resolveAdapter(ctx, next)
	if err != nil {
		return nil, err
	}
	if err := next.Policy.Validate(next.Mode, resolved.Adapter.Features); err != nil {
		return nil, err
	}
	if err := next.Validate(); err != nil {
		return nil, err
	}
	if err := s.store.UpdateExternalConversationEndpoint(ctx, next, req.ExpectedRevision); err != nil {
		return nil, err
	}
	return cloneExternalConversationEndpoint(next), nil
}

func (s *ExternalConversationEndpointService) resolveAdapter(ctx context.Context, endpoint *ExternalConversationEndpoint) (*skill.BoundConversationAdapter, error) {
	ref := endpoint.Adapter
	resolved, err := s.resolver.ResolveConversationAdapterBinding(
		ctx, skill.ScopeReference{Kind: endpoint.Scope.Kind, ID: endpoint.Scope.ID}, endpoint.DeploymentID,
		ref.BindingID, ref.AdapterID,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve exact Skill adapter: %v", ErrInvalidExternalConversation, err)
	}
	if (endpoint.Provider != "" && resolved.Adapter.Provider != endpoint.Provider) ||
		!containsConversationEndpointMode(resolved.Adapter.EndpointModes, endpoint.Mode) {
		return nil, fmt.Errorf("%w: exact Skill adapter does not match endpoint", ErrInvalidExternalConversation)
	}
	return resolved, nil
}

// NormalizedExternalConversationEvent is the only ingress payload accepted by
// the kernel. The Skill verifies provider requests and converts them into this
// credential-free protocol before durable ingestion.
type NormalizedExternalConversationEvent struct {
	ID                     string                 `json:"id"`
	Type                   string                 `json:"type"`
	ExternalConversationID string                 `json:"externalConversationId"`
	ExternalThreadID       string                 `json:"externalThreadId,omitempty"`
	ExternalMessageID      string                 `json:"externalMessageId,omitempty"`
	ExternalParticipantID  string                 `json:"externalParticipantId,omitempty"`
	ParticipantDisplayName string                 `json:"participantDisplayName,omitempty"`
	ParticipantIsBot       bool                   `json:"participantIsBot,omitempty"`
	Text                   string                 `json:"text,omitempty"`
	MentionsEndpoint       bool                   `json:"mentionsEndpoint,omitempty"`
	Direct                 bool                   `json:"direct,omitempty"`
	OrderingKey            string                 `json:"orderingKey"`
	Cursor                 string                 `json:"cursor,omitempty"`
	OccurredAt             time.Time              `json:"occurredAt"`
	Attributes             map[string]interface{} `json:"attributes,omitempty"`
}

func (e *NormalizedExternalConversationEvent) Validate() error {
	if e == nil || !validExternalConversationReference(e.ID, 512) ||
		!validExternalConversationReference(e.ExternalConversationID, 1024) ||
		!validExternalConversationReference(e.OrderingKey, 1024) || e.OccurredAt.IsZero() ||
		len(e.Cursor) > 4096 || strings.ContainsAny(e.Cursor, "\r\n") ||
		len(e.ParticipantDisplayName) > 160 || strings.ContainsAny(e.ParticipantDisplayName, "\r\n") {
		return ErrInvalidExternalConversation
	}
	switch e.Type {
	case capability.ConversationEventMessageReceived:
		if !validExternalConversationReference(e.ExternalMessageID, 1024) || !validExternalConversationReference(e.ExternalParticipantID, 1024) ||
			strings.TrimSpace(e.Text) == "" || len(e.Text) > 64*1024 {
			return ErrInvalidExternalConversation
		}
	case capability.ConversationEventMessageUpdated, capability.ConversationEventMessageDeleted:
		if !validExternalConversationReference(e.ExternalMessageID, 1024) {
			return ErrInvalidExternalConversation
		}
	case capability.ConversationEventReactionAdded, capability.ConversationEventReactionRemoved:
		if !validExternalConversationReference(e.ExternalMessageID, 1024) || !validExternalConversationReference(e.ExternalParticipantID, 1024) {
			return ErrInvalidExternalConversation
		}
	case capability.ConversationEventParticipantJoined, capability.ConversationEventParticipantLeft:
		if !validExternalConversationReference(e.ExternalParticipantID, 1024) {
			return ErrInvalidExternalConversation
		}
	case capability.ConversationEventApprovalDecided:
		if !validExternalConversationReference(e.ExternalParticipantID, 1024) ||
			!validExternalConversationReference(e.ExternalMessageID, 1024) {
			return ErrInvalidExternalConversation
		}
	default:
		return ErrInvalidExternalConversation
	}
	if e.ExternalThreadID != "" && !validExternalConversationReference(e.ExternalThreadID, 1024) {
		return ErrInvalidExternalConversation
	}
	if err := ValidateCredentialFreeContext(e.Attributes); err != nil {
		return fmt.Errorf("%w: event attributes: %v", ErrInvalidExternalConversation, err)
	}
	encoded, err := json.Marshal(e)
	if err != nil || len(encoded) > 1<<20 {
		return fmt.Errorf("%w: normalized event exceeds one MiB", ErrInvalidExternalConversation)
	}
	return nil
}

type ExternalConversationInboxStatus string

const (
	ExternalConversationInboxPending    ExternalConversationInboxStatus = "pending"
	ExternalConversationInboxLeased     ExternalConversationInboxStatus = "leased"
	ExternalConversationInboxRetry      ExternalConversationInboxStatus = "retry"
	ExternalConversationInboxApplied    ExternalConversationInboxStatus = "applied"
	ExternalConversationInboxIgnored    ExternalConversationInboxStatus = "ignored"
	ExternalConversationInboxDeadLetter ExternalConversationInboxStatus = "dead_letter"
)

type ExternalConversationInboxItem struct {
	ID               string                               `json:"id"`
	Scope            Scope                                `json:"scope"`
	EndpointID       string                               `json:"endpointId"`
	EndpointRevision int64                                `json:"endpointRevision"`
	Adapter          ExternalConversationAdapterReference `json:"adapter"`
	Event            NormalizedExternalConversationEvent  `json:"event"`
	Status           ExternalConversationInboxStatus      `json:"status"`
	Attempt          int                                  `json:"attempt"`
	MaximumAttempts  int                                  `json:"maximumAttempts"`
	AvailableAt      time.Time                            `json:"availableAt"`
	LeaseOwner       string                               `json:"leaseOwner,omitempty"`
	LeaseExpiresAt   time.Time                            `json:"leaseExpiresAt,omitempty"`
	ConversationID   string                               `json:"conversationId,omitempty"`
	ChannelMessageID string                               `json:"channelMessageId,omitempty"`
	RunID            string                               `json:"runId,omitempty"`
	ErrorCode        string                               `json:"errorCode,omitempty"`
	Summary          string                               `json:"summary,omitempty"`
	Revision         int64                                `json:"revision"`
	CreatedAt        time.Time                            `json:"createdAt"`
	UpdatedAt        time.Time                            `json:"updatedAt"`
	AppliedAt        time.Time                            `json:"appliedAt,omitempty"`
}

func (i *ExternalConversationInboxItem) Validate() error {
	if i == nil || i.Scope.Validate() != nil || !validOpaqueIdentifier(i.ID, 256) ||
		!validOpaqueIdentifier(i.EndpointID, 256) || i.EndpointRevision < 1 ||
		i.Adapter.Validate() != nil || i.Event.Validate() != nil ||
		i.Attempt < 0 || i.MaximumAttempts < 1 || i.MaximumAttempts > MaximumExternalConversationDeliveryAttempts ||
		i.AvailableAt.IsZero() || i.Revision < 1 ||
		i.CreatedAt.IsZero() || i.UpdatedAt.IsZero() || i.UpdatedAt.Before(i.CreatedAt) ||
		len(i.ErrorCode) > 128 || len(i.Summary) > 1024 {
		return ErrInvalidExternalConversation
	}
	for _, value := range []string{i.ConversationID, i.ChannelMessageID, i.RunID} {
		if value != "" && !validOpaqueIdentifier(value, 256) {
			return ErrInvalidExternalConversation
		}
	}
	switch i.Status {
	case ExternalConversationInboxPending, ExternalConversationInboxRetry:
		if i.LeaseOwner != "" || !i.LeaseExpiresAt.IsZero() || !i.AppliedAt.IsZero() {
			return ErrInvalidExternalConversation
		}
	case ExternalConversationInboxLeased:
		if !validOpaqueIdentifier(i.LeaseOwner, 256) || i.LeaseExpiresAt.IsZero() || !i.AppliedAt.IsZero() {
			return ErrInvalidExternalConversation
		}
	case ExternalConversationInboxApplied, ExternalConversationInboxIgnored:
		if i.LeaseOwner != "" || !i.LeaseExpiresAt.IsZero() || i.AppliedAt.IsZero() {
			return ErrInvalidExternalConversation
		}
		if i.Status == ExternalConversationInboxIgnored && i.Summary == "" {
			return ErrInvalidExternalConversation
		}
	case ExternalConversationInboxDeadLetter:
		if i.LeaseOwner != "" || !i.LeaseExpiresAt.IsZero() || !i.AppliedAt.IsZero() || i.ErrorCode == "" {
			return ErrInvalidExternalConversation
		}
	default:
		return ErrInvalidExternalConversation
	}
	return nil
}

type ExternalConversationMapping struct {
	Scope                  Scope     `json:"scope"`
	EndpointID             string    `json:"endpointId"`
	ExternalConversationID string    `json:"externalConversationId"`
	ExternalThreadID       string    `json:"externalThreadId,omitempty"`
	ConversationID         string    `json:"conversationId"`
	ThreadRootMessageID    string    `json:"threadRootMessageId,omitempty"`
	Revision               int64     `json:"revision"`
	CreatedAt              time.Time `json:"createdAt"`
	UpdatedAt              time.Time `json:"updatedAt"`
}

func (m *ExternalConversationMapping) Validate() error {
	if m == nil || m.Scope.Validate() != nil || !validOpaqueIdentifier(m.EndpointID, 256) ||
		!validExternalConversationReference(m.ExternalConversationID, 1024) ||
		!validOpaqueIdentifier(m.ConversationID, 256) || m.Revision < 1 ||
		m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() || m.UpdatedAt.Before(m.CreatedAt) {
		return ErrInvalidExternalConversation
	}
	for _, value := range []string{m.ExternalThreadID, m.ThreadRootMessageID} {
		if value != "" && !validExternalConversationReference(value, 1024) {
			return ErrInvalidExternalConversation
		}
	}
	return nil
}

type ExternalParticipantMapping struct {
	Scope                 Scope                   `json:"scope"`
	EndpointID            string                  `json:"endpointId"`
	ExternalParticipantID string                  `json:"externalParticipantId"`
	Participant           ConversationParticipant `json:"participant"`
	DisplayName           string                  `json:"displayName,omitempty"`
	Revision              int64                   `json:"revision"`
	CreatedAt             time.Time               `json:"createdAt"`
	UpdatedAt             time.Time               `json:"updatedAt"`
}

func (m *ExternalParticipantMapping) Validate() error {
	if m == nil || m.Scope.Validate() != nil || !validOpaqueIdentifier(m.EndpointID, 256) ||
		!validExternalConversationReference(m.ExternalParticipantID, 1024) || m.Participant.Validate() != nil ||
		len(m.DisplayName) > 160 || strings.ContainsAny(m.DisplayName, "\r\n") ||
		m.Revision < 1 || m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() || m.UpdatedAt.Before(m.CreatedAt) {
		return ErrInvalidExternalConversation
	}
	return nil
}

type ExternalMessageDirection string

const (
	ExternalMessageInbound  ExternalMessageDirection = "inbound"
	ExternalMessageOutbound ExternalMessageDirection = "outbound"
)

type ExternalMessageMapping struct {
	Scope             Scope                    `json:"scope"`
	EndpointID        string                   `json:"endpointId"`
	Direction         ExternalMessageDirection `json:"direction"`
	ExternalMessageID string                   `json:"externalMessageId"`
	ConversationID    string                   `json:"conversationId"`
	ChannelMessageID  string                   `json:"channelMessageId"`
	Revision          int64                    `json:"revision"`
	CreatedAt         time.Time                `json:"createdAt"`
	UpdatedAt         time.Time                `json:"updatedAt"`
}

func (m *ExternalMessageMapping) Validate() error {
	if m == nil || m.Scope.Validate() != nil || !validOpaqueIdentifier(m.EndpointID, 256) ||
		!validExternalConversationReference(m.ExternalMessageID, 1024) || !validOpaqueIdentifier(m.ConversationID, 256) ||
		!validOpaqueIdentifier(m.ChannelMessageID, 256) || m.Revision < 1 ||
		m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() || m.UpdatedAt.Before(m.CreatedAt) {
		return ErrInvalidExternalConversation
	}
	if m.Direction != ExternalMessageInbound && m.Direction != ExternalMessageOutbound {
		return ErrInvalidExternalConversation
	}
	return nil
}

type ExternalConversationDeliveryStatus string

const (
	ExternalConversationDeliveryPending   ExternalConversationDeliveryStatus = "pending"
	ExternalConversationDeliveryLeased    ExternalConversationDeliveryStatus = "leased"
	ExternalConversationDeliveryRetry     ExternalConversationDeliveryStatus = "retry"
	ExternalConversationDeliveryDelivered ExternalConversationDeliveryStatus = "delivered"
	ExternalConversationDeliveryFailed    ExternalConversationDeliveryStatus = "failed"
	ExternalConversationDeliveryCanceled  ExternalConversationDeliveryStatus = "canceled"
)

// ExternalConversationDeliveryCorrelation links one provider delivery to the
// canonical resource transition that caused it. It is intentionally
// provider-neutral and contains identifiers only; message content remains in
// the canonical conversation and credentials remain in the Skill binding.
type ExternalConversationDeliveryCorrelation struct {
	Kind  string `json:"kind"`
	ID    string `json:"id"`
	Phase string `json:"phase"`
}

func (c ExternalConversationDeliveryCorrelation) Validate() error {
	if !validOpaqueIdentifier(strings.TrimSpace(c.Kind), 64) ||
		!validOpaqueIdentifier(strings.TrimSpace(c.ID), 256) ||
		!validOpaqueIdentifier(strings.TrimSpace(c.Phase), 64) {
		return ErrInvalidExternalConversation
	}
	return nil
}

// ExternalConversationDelivery is the durable provider-neutral outbox. Content
// is loaded from the canonical ChannelMessage at dispatch time, so this record
// cannot drift into a second source of conversational truth.
type ExternalConversationDelivery struct {
	ID                string                                   `json:"id"`
	Scope             Scope                                    `json:"scope"`
	EndpointID        string                                   `json:"endpointId"`
	EndpointRevision  int64                                    `json:"endpointRevision"`
	Adapter           ExternalConversationAdapterReference     `json:"adapter"`
	Operation         capability.ConversationDeliveryOperation `json:"operation"`
	ConversationID    string                                   `json:"conversationId"`
	ChannelMessageID  string                                   `json:"channelMessageId"`
	ExternalThreadID  string                                   `json:"externalThreadId,omitempty"`
	OrderingKey       string                                   `json:"orderingKey"`
	Parameters        map[string]interface{}                   `json:"parameters,omitempty"`
	Correlation       *ExternalConversationDeliveryCorrelation `json:"correlation,omitempty"`
	IdempotencyKey    string                                   `json:"idempotencyKey"`
	Status            ExternalConversationDeliveryStatus       `json:"status"`
	Attempt           int                                      `json:"attempt"`
	MaximumAttempts   int                                      `json:"maximumAttempts"`
	AvailableAt       time.Time                                `json:"availableAt"`
	LeaseOwner        string                                   `json:"leaseOwner,omitempty"`
	LeaseExpiresAt    time.Time                                `json:"leaseExpiresAt,omitempty"`
	ProviderMessageID string                                   `json:"providerMessageId,omitempty"`
	ErrorCode         string                                   `json:"errorCode,omitempty"`
	Summary           string                                   `json:"summary,omitempty"`
	Revision          int64                                    `json:"revision"`
	CreatedAt         time.Time                                `json:"createdAt"`
	UpdatedAt         time.Time                                `json:"updatedAt"`
	DeliveredAt       time.Time                                `json:"deliveredAt,omitempty"`
}

func (d *ExternalConversationDelivery) Validate() error {
	if d == nil || d.Scope.Validate() != nil || !validOpaqueIdentifier(d.ID, 256) ||
		!validOpaqueIdentifier(d.EndpointID, 256) || d.EndpointRevision < 1 ||
		d.Adapter.Validate() != nil || !validConversationDeliveryOperation(d.Operation) ||
		!validOpaqueIdentifier(d.ConversationID, 256) || !validOpaqueIdentifier(d.ChannelMessageID, 256) ||
		!validOpaqueIdentifier(d.OrderingKey, 256) || !validOpaqueIdentifier(d.IdempotencyKey, 512) || d.Attempt < 0 ||
		d.MaximumAttempts < 1 || d.MaximumAttempts > MaximumExternalConversationDeliveryAttempts ||
		d.AvailableAt.IsZero() || d.Revision < 1 || d.CreatedAt.IsZero() || d.UpdatedAt.IsZero() ||
		d.UpdatedAt.Before(d.CreatedAt) || len(d.ErrorCode) > 128 || len(d.Summary) > 1024 {
		return ErrInvalidExternalConversation
	}
	for _, value := range []string{d.ExternalThreadID, d.ProviderMessageID} {
		if value != "" && !validExternalConversationReference(value, 1024) {
			return ErrInvalidExternalConversation
		}
	}
	if err := validateExternalConversationConfiguration(d.Parameters); err != nil {
		return err
	}
	if d.Correlation != nil {
		if err := d.Correlation.Validate(); err != nil {
			return err
		}
	}
	switch d.Status {
	case ExternalConversationDeliveryPending, ExternalConversationDeliveryRetry:
		if d.LeaseOwner != "" || !d.LeaseExpiresAt.IsZero() || !d.DeliveredAt.IsZero() {
			return ErrInvalidExternalConversation
		}
	case ExternalConversationDeliveryLeased:
		if !validOpaqueIdentifier(d.LeaseOwner, 256) || d.LeaseExpiresAt.IsZero() || !d.DeliveredAt.IsZero() {
			return ErrInvalidExternalConversation
		}
	case ExternalConversationDeliveryDelivered:
		if d.LeaseOwner != "" || !d.LeaseExpiresAt.IsZero() || d.DeliveredAt.IsZero() || d.ProviderMessageID == "" {
			return ErrInvalidExternalConversation
		}
	case ExternalConversationDeliveryFailed, ExternalConversationDeliveryCanceled:
		if d.LeaseOwner != "" || !d.LeaseExpiresAt.IsZero() || !d.DeliveredAt.IsZero() {
			return ErrInvalidExternalConversation
		}
		if d.Status == ExternalConversationDeliveryFailed && d.ErrorCode == "" {
			return ErrInvalidExternalConversation
		}
	default:
		return ErrInvalidExternalConversation
	}
	return nil
}

type ExternalConversationInboxFilter struct {
	Scope      Scope
	EndpointID string
	Statuses   []ExternalConversationInboxStatus
	Limit      int
	Offset     int
}

type ExternalConversationDeliveryFilter struct {
	Scope           Scope
	EndpointID      string
	ConversationID  string
	CorrelationKind string
	CorrelationID   string
	Statuses        []ExternalConversationDeliveryStatus
	Limit           int
	Offset          int
}

// ExternalConversationTransportStore is the durable inbox, mapping, and
// outbox contract. Claim operations must atomically lease one due item and
// reclaim expired leases so workers can recover after process loss.
type ExternalConversationTransportStore interface {
	ReceiveExternalConversationEvent(context.Context, *ExternalConversationInboxItem) (*ExternalConversationInboxItem, bool, error)
	GetExternalConversationInboxItem(context.Context, Scope, string) (*ExternalConversationInboxItem, error)
	ListExternalConversationInbox(context.Context, ExternalConversationInboxFilter) ([]*ExternalConversationInboxItem, error)
	ClaimExternalConversationInbox(context.Context, Scope, string, time.Time, time.Duration) (*ExternalConversationInboxItem, error)
	SaveExternalConversationInbox(context.Context, *ExternalConversationInboxItem, int64, string) error

	GetExternalConversationMapping(context.Context, Scope, string, string, string) (*ExternalConversationMapping, error)
	SaveExternalConversationMapping(context.Context, *ExternalConversationMapping, int64) error
	GetExternalParticipantMapping(context.Context, Scope, string, string) (*ExternalParticipantMapping, error)
	SaveExternalParticipantMapping(context.Context, *ExternalParticipantMapping, int64) error
	GetExternalMessageMapping(context.Context, Scope, string, ExternalMessageDirection, string) (*ExternalMessageMapping, error)
	SaveExternalMessageMapping(context.Context, *ExternalMessageMapping, int64) error

	EnqueueExternalConversationDelivery(context.Context, *ExternalConversationDelivery) (*ExternalConversationDelivery, bool, error)
	GetExternalConversationDelivery(context.Context, Scope, string) (*ExternalConversationDelivery, error)
	ListExternalConversationDeliveries(context.Context, ExternalConversationDeliveryFilter) ([]*ExternalConversationDelivery, error)
	ClaimExternalConversationDelivery(context.Context, Scope, string, time.Time, time.Duration) (*ExternalConversationDelivery, error)
	SaveExternalConversationDelivery(context.Context, *ExternalConversationDelivery, int64, string) error
}

func normalizeExternalConversationAdapterReference(value ExternalConversationAdapterReference) ExternalConversationAdapterReference {
	value.SkillID = strings.TrimSpace(value.SkillID)
	value.SkillVersion = strings.TrimSpace(value.SkillVersion)
	value.SourceIdentity = strings.TrimSpace(value.SourceIdentity)
	value.BindingID = strings.TrimSpace(value.BindingID)
	value.AdapterID = strings.TrimSpace(value.AdapterID)
	return value
}

func normalizeExternalConversationHandler(value ExternalConversationHandler) ExternalConversationHandler {
	value.ID = strings.TrimSpace(value.ID)
	value.Version = strings.TrimSpace(value.Version)
	value.Trigger = strings.TrimSpace(value.Trigger)
	value.AssignedAgentID = strings.TrimSpace(value.AssignedAgentID)
	return value
}

func validateExternalConversationConfiguration(value map[string]interface{}) error {
	if err := ValidateCredentialFreeContext(value); err != nil {
		return fmt.Errorf("%w: configuration: %v", ErrInvalidExternalConversation, err)
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > MaximumExternalConversationConfigurationBytes {
		return fmt.Errorf("%w: configuration must be valid JSON no larger than 64 KiB", ErrInvalidExternalConversation)
	}
	return nil
}

func validExternalConversationEndpointStatus(value ExternalConversationEndpointStatus) bool {
	return value == ExternalConversationEndpointActive || value == ExternalConversationEndpointPaused || value == ExternalConversationEndpointRetired
}

func validConversationDeliveryOperation(value capability.ConversationDeliveryOperation) bool {
	switch value {
	case capability.ConversationDeliveryMessageSend, capability.ConversationDeliveryMessageUpdate, capability.ConversationDeliveryMessageDelete,
		capability.ConversationDeliveryReactionAdd, capability.ConversationDeliveryReactionRemove, capability.ConversationDeliveryTypingIndicator:
		return true
	default:
		return false
	}
}

// Provider references are opaque data rather than transport paths. Common
// providers use slashes, plus signs, equals signs, and Unicode in stable IDs,
// so only surrounding whitespace and control characters are forbidden.
func validExternalConversationReference(value string, maximum int) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maximum {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func containsConversationEndpointMode(values []capability.ConversationEndpointMode, wanted capability.ConversationEndpointMode) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func hasConversationFeature(values []capability.ConversationAdapterFeature, wanted capability.ConversationAdapterFeature) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func cloneExternalConversationEndpoint(value *ExternalConversationEndpoint) *ExternalConversationEndpoint {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Configuration = cloneMap(value.Configuration)
	if value.RetiredAt != nil {
		retiredAt := *value.RetiredAt
		copy.RetiredAt = &retiredAt
	}
	return &copy
}

func cloneExternalConversationInboxItem(value *ExternalConversationInboxItem) *ExternalConversationInboxItem {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Event.Attributes = cloneMap(value.Event.Attributes)
	return &copy
}

func cloneExternalConversationMapping(value *ExternalConversationMapping) *ExternalConversationMapping {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneExternalParticipantMapping(value *ExternalParticipantMapping) *ExternalParticipantMapping {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneExternalMessageMapping(value *ExternalMessageMapping) *ExternalMessageMapping {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneExternalConversationDelivery(value *ExternalConversationDelivery) *ExternalConversationDelivery {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Parameters = cloneMap(value.Parameters)
	if value.Correlation != nil {
		correlation := *value.Correlation
		copy.Correlation = &correlation
	}
	return &copy
}

func cloneExternalConversationDeliveryCorrelation(value *ExternalConversationDeliveryCorrelation) *ExternalConversationDeliveryCorrelation {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func sortedExternalConversationEndpointStatuses(values []ExternalConversationEndpointStatus) []ExternalConversationEndpointStatus {
	result := append([]ExternalConversationEndpointStatus(nil), values...)
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}
