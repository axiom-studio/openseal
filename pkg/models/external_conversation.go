package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/capability"
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
	ExternalConversationHandlerAgent ExternalConversationHandlerKind = "agent"
	ExternalConversationHandlerTeam  ExternalConversationHandlerKind = "team"
)

// ExternalConversationHandler selects the portable cognitive handler, the
// endpoint's owning Agent or Team. Provider-specific behavior never belongs in
// the handler.
type ExternalConversationHandler struct {
	Kind ExternalConversationHandlerKind `json:"kind"`
	ID   string                          `json:"id"`
}

func (h ExternalConversationHandler) Validate(owner ObjectiveOwner) error {
	if !IsValidAgentReference(strings.TrimSpace(h.ID), 256) {
		return fmt.Errorf("%w: handler id is invalid", ErrInvalidExternalConversation)
	}
	switch h.Kind {
	case ExternalConversationHandlerAgent:
		if owner.Type != OwnerTypeAgent || h.ID != owner.ID {
			return fmt.Errorf("%w: Agent handler must identify the endpoint owner", ErrInvalidExternalConversation)
		}
	case ExternalConversationHandlerTeam:
		if owner.Type != OwnerTypeTeam || h.ID != owner.ID {
			return fmt.Errorf("%w: Team handler must identify the endpoint owner", ErrInvalidExternalConversation)
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
	if p.ReplyMode == ExternalConversationReplyThread && !HasConversationFeature(features, capability.ConversationFeatureThreads) {
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
		if !IsValidOpaqueIdentifier(strings.TrimSpace(value), 256) {
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
	ID               string                               `json:"id"`
	IngressRoute     string                               `json:"ingressRoute"`
	Scope            Scope                                `json:"scope"`
	Owner            ObjectiveOwner                       `json:"owner"`
	DeploymentID     string                               `json:"deploymentId"`
	Name             string                               `json:"name"`
	Adapter          ExternalConversationAdapterReference `json:"adapter"`
	Provider         string                               `json:"provider"`
	Mode             capability.ConversationEndpointMode  `json:"mode"`
	InstallationID   string                               `json:"installationId,omitempty"`
	ApplicationID    string                               `json:"applicationId,omitempty"`
	Address          string                               `json:"address,omitempty"`
	InstallationWide bool                                 `json:"installationWide,omitempty"`
	Handler          ExternalConversationHandler          `json:"handler"`
	Policy           ExternalConversationPolicy           `json:"policy"`
	Configuration    map[string]interface{}               `json:"configuration,omitempty"`
	Status           ExternalConversationEndpointStatus   `json:"status"`
	// CreatedBy is the person who created the endpoint, when the host names
	// one. Its conversations go to their default space while the owner is in
	// no space.
	CreatedBy string     `json:"createdBy,omitempty"`
	Revision  int64      `json:"revision"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
	RetiredAt *time.Time `json:"retiredAt,omitempty"`
}

func (e *ExternalConversationEndpoint) Validate() error {
	if e == nil || e.Scope.Validate() != nil || e.Owner.Validate() != nil ||
		!IsValidOpaqueIdentifier(strings.TrimSpace(e.ID), 256) ||
		!IsValidOpaqueIdentifier(strings.TrimSpace(e.IngressRoute), 128) ||
		!IsValidAgentReference(strings.TrimSpace(e.DeploymentID), 256) ||
		strings.TrimSpace(e.Name) == "" || len(e.Name) > 160 ||
		!IsValidOpaqueIdentifier(e.Provider, 128) ||
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
	if err := ValidateExternalConversationConfiguration(e.Configuration); err != nil {
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

type ExternalConversationEndpointStore interface {
	CreateExternalConversationEndpoint(context.Context, *ExternalConversationEndpoint) error
	GetExternalConversationEndpoint(context.Context, Scope, string) (*ExternalConversationEndpoint, error)
	GetExternalConversationEndpointByIngressRoute(context.Context, string) (*ExternalConversationEndpoint, error)
	ListExternalConversationEndpoints(context.Context, ExternalConversationEndpointFilter) ([]*ExternalConversationEndpoint, error)
	ListExternalConversationEndpointsByVerifiedRoute(context.Context, ExternalConversationVerifiedRoute) ([]*ExternalConversationEndpoint, error)
	UpdateExternalConversationEndpoint(context.Context, *ExternalConversationEndpoint, int64) error
}

// NormalizedExternalConversationEvent is the only ingress payload accepted by
// the kernel. The Skill verifies provider requests and converts them into this
// credential-free protocol before durable ingestion.
type NormalizedExternalConversationEvent struct {
	Attachments            []ExternalConversationAttachment `json:"attachments,omitempty"`
	Source                 *ExternalMessageSource           `json:"source,omitempty"`
	ID                     string                           `json:"id"`
	Type                   string                           `json:"type"`
	ExternalConversationID string                           `json:"externalConversationId"`
	ExternalThreadID       string                           `json:"externalThreadId,omitempty"`
	ExternalMessageID      string                           `json:"externalMessageId,omitempty"`
	// ReplyToExternalMessageID identifies an immediate provider parent using the
	// same endpoint-scoped external ID contract as message mappings. It links a
	// new opaque provider thread alias to an existing canonical root; explicit
	// provider topics can omit it to retain their independent thread identity.
	ReplyToExternalMessageID string                 `json:"replyToExternalMessageId,omitempty"`
	ExternalParticipantID    string                 `json:"externalParticipantId,omitempty"`
	ParticipantDisplayName   string                 `json:"participantDisplayName,omitempty"`
	ParticipantIsBot         bool                   `json:"participantIsBot,omitempty"`
	Text                     string                 `json:"text,omitempty"`
	Emoji                    string                 `json:"emoji,omitempty"` // reaction events: Unicode emoji, see ValidateReactionEmoji
	MentionsEndpoint         bool                   `json:"mentionsEndpoint,omitempty"`
	Direct                   bool                   `json:"direct,omitempty"`
	OrderingKey              string                 `json:"orderingKey"`
	Cursor                   string                 `json:"cursor,omitempty"`
	OccurredAt               time.Time              `json:"occurredAt"`
	Attributes               map[string]interface{} `json:"attributes,omitempty"`
}

func (e *NormalizedExternalConversationEvent) Validate() error {
	if e == nil || !IsValidExternalConversationReference(e.ID, 512) ||
		!IsValidExternalConversationReference(e.ExternalConversationID, 1024) ||
		!IsValidExternalConversationReference(e.OrderingKey, 1024) || e.OccurredAt.IsZero() ||
		len(e.Cursor) > 4096 || strings.ContainsAny(e.Cursor, "\r\n") ||
		len(e.ParticipantDisplayName) > 160 || strings.ContainsAny(e.ParticipantDisplayName, "\r\n") {
		return ErrInvalidExternalConversation
	}
	switch e.Type {
	case capability.ConversationEventMessageReceived:
		if !IsValidExternalConversationReference(e.ExternalMessageID, 1024) || !IsValidExternalConversationReference(e.ExternalParticipantID, 1024) ||
			(strings.TrimSpace(e.Text) == "" && len(e.Attachments) == 0) || len(e.Text) > 64*1024 {
			return ErrInvalidExternalConversation
		}
	case capability.ConversationEventMessageUpdated, capability.ConversationEventMessageDeleted:
		if !IsValidExternalConversationReference(e.ExternalMessageID, 1024) {
			return ErrInvalidExternalConversation
		}
	case capability.ConversationEventReactionAdded, capability.ConversationEventReactionRemoved:
		if !IsValidExternalConversationReference(e.ExternalMessageID, 1024) || !IsValidExternalConversationReference(e.ExternalParticipantID, 1024) ||
			ValidateReactionEmoji(e.Emoji) != nil {
			return ErrInvalidExternalConversation
		}
	case capability.ConversationEventParticipantJoined, capability.ConversationEventParticipantLeft:
		if !IsValidExternalConversationReference(e.ExternalParticipantID, 1024) {
			return ErrInvalidExternalConversation
		}
	case capability.ConversationEventApprovalDecided:
		if !IsValidExternalConversationReference(e.ExternalParticipantID, 1024) ||
			!IsValidExternalConversationReference(e.ExternalMessageID, 1024) {
			return ErrInvalidExternalConversation
		}
	default:
		return ErrInvalidExternalConversation
	}
	if e.ExternalThreadID != "" && !IsValidExternalConversationReference(e.ExternalThreadID, 1024) {
		return ErrInvalidExternalConversation
	}
	if e.ReplyToExternalMessageID != "" && !IsValidExternalConversationReference(e.ReplyToExternalMessageID, 1024) {
		return ErrInvalidExternalConversation
	}
	if err := ValidateExternalAttachments(e.Attachments); err != nil {
		return err
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
	// Reply projection bookkeeping for applied items. The reply worker sets
	// ReplyState once the canonical reply is projected (or none is due) and
	// backs off failed projections via ReplyAvailableAt, so applied items are
	// not re-projected forever. Status stays "applied" for every other reader.
	ReplyState       ExternalConversationReplyState `json:"replyState,omitempty"`
	ReplyAttempts    int                            `json:"replyAttempts,omitempty"`
	ReplyAvailableAt *time.Time                     `json:"replyAvailableAt,omitempty"`
	ReplyError       string                         `json:"replyError,omitempty"`
}

// ExternalConversationReplyState is terminal once set.
type ExternalConversationReplyState string

const (
	// ExternalConversationReplyProjected: the reply (or a deliberate no-op,
	// such as a paused endpoint or a superseded failure) is resolved.
	ExternalConversationReplyProjected ExternalConversationReplyState = "projected"
	// ExternalConversationReplyFailed: projection kept failing and stopped.
	ExternalConversationReplyFailed ExternalConversationReplyState = "failed"
)

// ExternalConversationReplyProgress is the only mutation the reply worker
// may make to an applied inbox item.
type ExternalConversationReplyProgress struct {
	State       ExternalConversationReplyState
	Attempts    int
	AvailableAt *time.Time
	Error       string
	At          time.Time
}

// ExternalConversationReplyStateStore records reply projection progress with
// a revision compare-and-set on applied items only.
type ExternalConversationReplyStateStore interface {
	SaveExternalConversationReplyProgress(ctx context.Context, scope Scope, id string, expectedRevision int64, progress ExternalConversationReplyProgress) error
}

// ApplyExternalConversationReplyProgress validates and applies progress to a
// copy of current, touching nothing but the reply fields and revision.
func ApplyExternalConversationReplyProgress(current *ExternalConversationInboxItem, expectedRevision int64, progress ExternalConversationReplyProgress) (*ExternalConversationInboxItem, error) {
	if current == nil {
		return nil, ErrExternalConversationInboxNotFound
	}
	if current.Status != ExternalConversationInboxApplied || current.Revision != expectedRevision || current.ReplyState != "" {
		return nil, ErrExternalConversationConflict
	}
	if progress.At.IsZero() || progress.Attempts < 0 {
		return nil, ErrInvalidExternalConversation
	}
	next := CloneExternalConversationInboxItem(current)
	next.ReplyState, next.ReplyAttempts, next.ReplyError = progress.State, progress.Attempts, truncateExternalReplyError(progress.Error)
	next.ReplyAvailableAt = nil
	if progress.AvailableAt != nil {
		at := progress.AvailableAt.UTC()
		next.ReplyAvailableAt = &at
	}
	next.Revision++
	if progress.At.After(next.UpdatedAt) {
		next.UpdatedAt = progress.At.UTC()
	}
	if err := next.Validate(); err != nil {
		return nil, err
	}
	return next, nil
}

func truncateExternalReplyError(value string) string {
	if len(value) <= 512 {
		return value
	}
	value = value[:512]
	for len(value) > 0 && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func (i *ExternalConversationInboxItem) Validate() error {
	if i == nil || i.Scope.Validate() != nil || !IsValidOpaqueIdentifier(i.ID, 256) ||
		!IsValidOpaqueIdentifier(i.EndpointID, 256) || i.EndpointRevision < 1 ||
		i.Adapter.Validate() != nil || i.Event.Validate() != nil ||
		i.Attempt < 0 || i.MaximumAttempts < 1 || i.MaximumAttempts > MaximumExternalConversationDeliveryAttempts ||
		i.AvailableAt.IsZero() || i.Revision < 1 ||
		i.CreatedAt.IsZero() || i.UpdatedAt.IsZero() || i.UpdatedAt.Before(i.CreatedAt) ||
		len(i.ErrorCode) > 128 || len(i.Summary) > 1024 || len(i.ReplyError) > 1024 || i.ReplyAttempts < 0 {
		return ErrInvalidExternalConversation
	}
	switch i.ReplyState {
	case "", ExternalConversationReplyProjected, ExternalConversationReplyFailed:
	default:
		return ErrInvalidExternalConversation
	}
	if (i.ReplyState != "" || i.ReplyAttempts > 0 || i.ReplyAvailableAt != nil) && i.Status != ExternalConversationInboxApplied {
		return ErrInvalidExternalConversation
	}
	for _, value := range []string{i.ConversationID, i.ChannelMessageID, i.RunID} {
		if value != "" && !IsValidOpaqueIdentifier(value, 256) {
			return ErrInvalidExternalConversation
		}
	}
	switch i.Status {
	case ExternalConversationInboxPending, ExternalConversationInboxRetry:
		if i.LeaseOwner != "" || !i.LeaseExpiresAt.IsZero() || !i.AppliedAt.IsZero() {
			return ErrInvalidExternalConversation
		}
	case ExternalConversationInboxLeased:
		if !IsValidOpaqueIdentifier(i.LeaseOwner, 256) || i.LeaseExpiresAt.IsZero() || !i.AppliedAt.IsZero() {
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
	if m == nil || m.Scope.Validate() != nil || !IsValidOpaqueIdentifier(m.EndpointID, 256) ||
		!IsValidExternalConversationReference(m.ExternalConversationID, 1024) ||
		!IsValidOpaqueIdentifier(m.ConversationID, 256) || m.Revision < 1 ||
		m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() || m.UpdatedAt.Before(m.CreatedAt) {
		return ErrInvalidExternalConversation
	}
	for _, value := range []string{m.ExternalThreadID, m.ThreadRootMessageID} {
		if value != "" && !IsValidExternalConversationReference(value, 1024) {
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
	if m == nil || m.Scope.Validate() != nil || !IsValidOpaqueIdentifier(m.EndpointID, 256) ||
		!IsValidExternalConversationReference(m.ExternalParticipantID, 1024) || m.Participant.Validate() != nil ||
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
	if m == nil || m.Scope.Validate() != nil || !IsValidOpaqueIdentifier(m.EndpointID, 256) ||
		!IsValidExternalConversationReference(m.ExternalMessageID, 1024) || !IsValidOpaqueIdentifier(m.ConversationID, 256) ||
		!IsValidOpaqueIdentifier(m.ChannelMessageID, 256) || m.Revision < 1 ||
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
	if !IsValidOpaqueIdentifier(strings.TrimSpace(c.Kind), 64) ||
		!IsValidOpaqueIdentifier(strings.TrimSpace(c.ID), 256) ||
		!IsValidOpaqueIdentifier(strings.TrimSpace(c.Phase), 64) {
		return ErrInvalidExternalConversation
	}
	return nil
}

// ExternalConversationDelivery is the durable provider-neutral outbox. Content
// is loaded from the canonical ChannelMessage at dispatch time, so this record
// cannot drift into a second source of conversational truth.
type ExternalConversationDelivery struct {
	Progress               map[string]interface{}                   `json:"progress,omitempty"`
	ID                     string                                   `json:"id"`
	Scope                  Scope                                    `json:"scope"`
	EndpointID             string                                   `json:"endpointId"`
	EndpointRevision       int64                                    `json:"endpointRevision"`
	Adapter                ExternalConversationAdapterReference     `json:"adapter"`
	Operation              capability.ConversationDeliveryOperation `json:"operation"`
	ConversationID         string                                   `json:"conversationId"`
	ChannelMessageID       string                                   `json:"channelMessageId"`
	ExternalConversationID string                                   `json:"externalConversationId,omitempty"`
	ExternalThreadID       string                                   `json:"externalThreadId,omitempty"`
	OrderingKey            string                                   `json:"orderingKey"`
	Parameters             map[string]interface{}                   `json:"parameters,omitempty"`
	Correlation            *ExternalConversationDeliveryCorrelation `json:"correlation,omitempty"`
	IdempotencyKey         string                                   `json:"idempotencyKey"`
	Status                 ExternalConversationDeliveryStatus       `json:"status"`
	Attempt                int                                      `json:"attempt"`
	MaximumAttempts        int                                      `json:"maximumAttempts"`
	AvailableAt            time.Time                                `json:"availableAt"`
	LeaseOwner             string                                   `json:"leaseOwner,omitempty"`
	LeaseExpiresAt         time.Time                                `json:"leaseExpiresAt,omitempty"`
	ProviderMessageID      string                                   `json:"providerMessageId,omitempty"`
	ErrorCode              string                                   `json:"errorCode,omitempty"`
	Summary                string                                   `json:"summary,omitempty"`
	Revision               int64                                    `json:"revision"`
	CreatedAt              time.Time                                `json:"createdAt"`
	UpdatedAt              time.Time                                `json:"updatedAt"`
	DeliveredAt            time.Time                                `json:"deliveredAt,omitempty"`
}

func (d *ExternalConversationDelivery) Validate() error {
	if d == nil || d.Scope.Validate() != nil || !IsValidOpaqueIdentifier(d.ID, 256) ||
		!IsValidOpaqueIdentifier(d.EndpointID, 256) || d.EndpointRevision < 1 ||
		d.Adapter.Validate() != nil || !validConversationDeliveryOperation(d.Operation) ||
		!IsValidOpaqueIdentifier(d.ConversationID, 256) || !IsValidOpaqueIdentifier(d.ChannelMessageID, 256) ||
		!IsValidOpaqueIdentifier(d.OrderingKey, 256) || !IsValidOpaqueIdentifier(d.IdempotencyKey, 512) || d.Attempt < 0 ||
		d.MaximumAttempts < 1 || d.MaximumAttempts > MaximumExternalConversationDeliveryAttempts ||
		d.AvailableAt.IsZero() || d.Revision < 1 || d.CreatedAt.IsZero() || d.UpdatedAt.IsZero() ||
		d.UpdatedAt.Before(d.CreatedAt) || len(d.ErrorCode) > 128 || len(d.Summary) > 1024 {
		return ErrInvalidExternalConversation
	}
	for _, value := range []string{d.ExternalConversationID, d.ExternalThreadID, d.ProviderMessageID} {
		if value != "" && !IsValidExternalConversationReference(value, 1024) {
			return ErrInvalidExternalConversation
		}
	}
	if err := ValidateExternalConversationConfiguration(d.Progress); err != nil {
		return err
	}
	if err := ValidateExternalConversationConfiguration(d.Parameters); err != nil {
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
		if !IsValidOpaqueIdentifier(d.LeaseOwner, 256) || d.LeaseExpiresAt.IsZero() || !d.DeliveredAt.IsZero() {
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
	// ReplyDueAt, when set, keeps only items whose reply is unresolved and
	// whose reply backoff has elapsed at that instant.
	ReplyDueAt *time.Time
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

func ValidateExternalConversationConfiguration(value map[string]interface{}) error {
	if err := ValidateCredentialFreeContext(value); err != nil {
		return fmt.Errorf("%w: configuration: %v", ErrInvalidExternalConversation, err)
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > MaximumExternalConversationConfigurationBytes {
		return fmt.Errorf("%w: configuration must be valid JSON no larger than 64 KiB", ErrInvalidExternalConversation)
	}
	return nil
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
func IsValidExternalConversationReference(value string, maximum int) bool {
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

func HasConversationFeature(values []capability.ConversationAdapterFeature, wanted capability.ConversationAdapterFeature) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func CloneExternalConversationEndpoint(value *ExternalConversationEndpoint) *ExternalConversationEndpoint {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Configuration = maps.Clone(value.Configuration)
	if value.RetiredAt != nil {
		retiredAt := *value.RetiredAt
		copy.RetiredAt = &retiredAt
	}
	return &copy
}

func CloneExternalConversationInboxItem(value *ExternalConversationInboxItem) *ExternalConversationInboxItem {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Event.Attachments = append([]ExternalConversationAttachment(nil), value.Event.Attachments...)
	copy.Event.Attributes = maps.Clone(value.Event.Attributes)
	copy.Event.Source = CloneExternalMessageSource(value.Event.Source)
	return &copy
}

func CloneExternalConversationMapping(value *ExternalConversationMapping) *ExternalConversationMapping {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func CloneExternalParticipantMapping(value *ExternalParticipantMapping) *ExternalParticipantMapping {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func CloneExternalConversationDelivery(value *ExternalConversationDelivery) *ExternalConversationDelivery {
	if value == nil {
		return nil
	}
	copy := *value
	copy.Parameters = maps.Clone(value.Parameters)
	copy.Progress = maps.Clone(value.Progress)
	if value.Correlation != nil {
		correlation := *value.Correlation
		copy.Correlation = &correlation
	}
	return &copy
}
