// Package capability defines portable, dependency-free skill contracts.
package capability

import "time"

type RiskLevel string

const (
	RiskLevelRead        RiskLevel = "read"
	RiskLevelWrite       RiskLevel = "write"
	RiskLevelExternal    RiskLevel = "external"
	RiskLevelProduction  RiskLevel = "production"
	RiskLevelDestructive RiskLevel = "destructive"
)

type SideEffect string

const (
	SideEffectNone        SideEffect = "none"
	SideEffectRead        SideEffect = "read"
	SideEffectWrite       SideEffect = "write"
	SideEffectExternal    SideEffect = "external"
	SideEffectDestructive SideEffect = "destructive"
)

type IdempotencyMode string

const (
	IdempotencyNone      IdempotencyMode = "none"
	IdempotencySupported IdempotencyMode = "supported"
	IdempotencyRequired  IdempotencyMode = "required"
)

type CredentialRequirement struct {
	Name     string             `json:"name"`
	Kind     string             `json:"kind"`
	Optional bool               `json:"optional,omitempty"`
	OAuth2   *OAuth2Requirement `json:"oauth2,omitempty"`
}

// CredentialReference is an opaque binding identifier. Resolved values must
// never be placed in definitions, model catalogs, prompts, or activity events.
type CredentialReference struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// CredentialBindingChoice is a secret-safe, operator-facing option for one
// authorized opaque credential reference. Hosts resolve the reference out of
// band; neither the reference nor its display label is model catalog input.
type CredentialBindingChoice struct {
	Reference   CredentialReference `json:"reference"`
	DisplayName string              `json:"displayName"`
	// BindingKeys identifies deployment credential slots this choice may fill.
	// An empty list preserves the canonical Skill behavior where the reference
	// kind itself is the binding key.
	BindingKeys []string `json:"bindingKeys,omitempty"`
	// OAuth2 is a non-secret host attestation about the connection identified
	// by Reference. Tokens, client credentials, and provider payloads never
	// cross this contract.
	OAuth2 *OAuth2GrantSummary `json:"oauth2,omitempty"`
	// ExternalIdentity contains only operator-visible routing identity that a
	// host has verified for this credential. It lets clients compose placement
	// without asking people to copy opaque provider identifiers. It is context
	// data and must never become model catalog or prompt input.
	ExternalIdentity *CredentialExternalIdentity `json:"externalIdentity,omitempty"`
}

// CredentialExternalIdentity is the minimal provider-neutral identity needed
// to place an integration backed by an authorized credential.
type CredentialExternalIdentity struct {
	InstallationID string `json:"installationId,omitempty"`
	ApplicationID  string `json:"applicationId,omitempty"`
	DisplayName    string `json:"displayName,omitempty"`
}

type ActionRetryPolicy struct {
	MaxAttempts    int      `json:"maxAttempts"`
	InitialBackoff Duration `json:"initialBackoff,omitempty"`
	MaxBackoff     Duration `json:"maxBackoff,omitempty"`
}

// ActionEvidenceRequirement declares a successful preparatory action that
// must already exist in the same Run before this action may be proposed. The
// kernel verifies durable action history; model-authored summaries and review
// facts never satisfy the requirement.
type ActionEvidenceRequirement struct {
	Action            string   `json:"action"`
	MatchingArguments []string `json:"matchingArguments,omitempty"`
}

// ExternalOperationPolicy controls whether an Action may claim a durable,
// cross-Run receipt for an externally observable business operation. Empty is
// the legacy-compatible optional policy for external side effects.
type ExternalOperationPolicy string

const (
	ExternalOperationForbidden ExternalOperationPolicy = "forbidden"
	ExternalOperationOptional  ExternalOperationPolicy = "optional"
	ExternalOperationRequired  ExternalOperationPolicy = "required"
)

type Duration time.Duration

func (d Duration) Duration() time.Duration { return time.Duration(d) }

type Action struct {
	Name                    string                  `json:"name"`
	Description             string                  `json:"description"`
	InputSchema             map[string]interface{}  `json:"inputSchema"`
	OutputSchema            map[string]interface{}  `json:"outputSchema,omitempty"`
	SideEffect              SideEffect              `json:"sideEffect"`
	Risk                    RiskLevel               `json:"risk"`
	Permissions             []string                `json:"permissions,omitempty"`
	Credentials             []CredentialRequirement `json:"credentials,omitempty"`
	Timeout                 Duration                `json:"timeout,omitempty"`
	Retry                   ActionRetryPolicy       `json:"retry"`
	Idempotency             IdempotencyMode         `json:"idempotency"`
	ExternalOperationPolicy ExternalOperationPolicy `json:"externalOperationPolicy,omitempty"`
	DryRunAction            string                  `json:"dryRunAction,omitempty"`
	CompensationAction      string                  `json:"compensationAction,omitempty"`
	// FinalizerAction releases temporary resources acquired by this action
	// when its owning Run reaches any terminal state. The finalizer receives
	// same-named values projected from the succeeded action output first and
	// its persisted non-secret arguments second.
	FinalizerAction      string              `json:"finalizerAction,omitempty"`
	EmittedArtifactTypes []string            `json:"emittedArtifactTypes,omitempty"`
	EmittedEventTypes    []string            `json:"emittedEventTypes,omitempty"`
	Transport            *TransportReference `json:"transport,omitempty"`
	// SemanticArguments maps portable roles such as "target", "body", or
	// "artifact" to exact input-schema property names. Product surfaces use
	// these roles to compose actions without guessing connector-specific fields.
	SemanticArguments map[string]string           `json:"semanticArguments,omitempty"`
	RequiredEvidence  []ActionEvidenceRequirement `json:"requiredEvidence,omitempty"`
}

type TransportReference struct {
	Kind      string                       `json:"kind"`
	Endpoint  string                       `json:"endpoint,omitempty"`
	Arguments map[string]TransportArgument `json:"arguments,omitempty"`
}

// TransportArgument deterministically projects a model-visible action input
// or a compiler-provided literal into the transport-specific call envelope.
// Credentials are deliberately resolved and passed out of band.
type TransportArgument struct {
	SourceArgument string      `json:"sourceArgument,omitempty"`
	Literal        interface{} `json:"literal,omitempty"`
}

type PromptModule struct {
	Instructions           string                  `json:"instructions"`
	AlwaysActive           bool                    `json:"alwaysActive,omitempty"`
	UserInvocable          bool                    `json:"userInvocable"`
	DisableModelInvocation bool                    `json:"disableModelInvocation,omitempty"`
	AllowedTools           []string                `json:"allowedTools,omitempty"`
	Credentials            []CredentialRequirement `json:"credentials,omitempty"`
}

type ConversationEndpointMode string

const (
	ConversationEndpointChannel ConversationEndpointMode = "channel"
	ConversationEndpointDirect  ConversationEndpointMode = "direct"
)

type ConversationAdapterFeature string

const (
	ConversationFeatureThreads     ConversationAdapterFeature = "threads"
	ConversationFeatureMentions    ConversationAdapterFeature = "mentions"
	ConversationFeatureAttachments ConversationAdapterFeature = "attachments"
	ConversationFeatureReactions   ConversationAdapterFeature = "reactions"
	ConversationFeatureEdits       ConversationAdapterFeature = "edits"
	ConversationFeatureDeletes     ConversationAdapterFeature = "deletes"
	ConversationFeatureTyping      ConversationAdapterFeature = "typing"
)

const ConversationAdapterProtocolV1 = "openseal.conversation.adapter/v1"

const (
	ConversationEventMessageReceived   = "conversation.message.received"
	ConversationEventMessageUpdated    = "conversation.message.updated"
	ConversationEventMessageDeleted    = "conversation.message.deleted"
	ConversationEventReactionAdded     = "conversation.reaction.added"
	ConversationEventReactionRemoved   = "conversation.reaction.removed"
	ConversationEventParticipantJoined = "conversation.participant.joined"
	ConversationEventParticipantLeft   = "conversation.participant.left"
	ConversationEventApprovalDecided   = "conversation.approval.decided"
)

type ConversationDeliveryOperation string

const (
	ConversationDeliveryMessageSend     ConversationDeliveryOperation = "message.send"
	ConversationDeliveryMessageUpdate   ConversationDeliveryOperation = "message.update"
	ConversationDeliveryMessageDelete   ConversationDeliveryOperation = "message.delete"
	ConversationDeliveryReactionAdd     ConversationDeliveryOperation = "reaction.add"
	ConversationDeliveryReactionRemove  ConversationDeliveryOperation = "reaction.remove"
	ConversationDeliveryTypingIndicator ConversationDeliveryOperation = "typing.set"
)

type ConversationDeliveryOrdering string

const (
	ConversationDeliveryOrderEndpoint     ConversationDeliveryOrdering = "endpoint"
	ConversationDeliveryOrderConversation ConversationDeliveryOrdering = "conversation"
	ConversationDeliveryOrderThread       ConversationDeliveryOrdering = "thread"
)

// ConversationDeliveryCapabilities declares the guarantees supplied by the
// Skill-owned delivery implementation. OpenSeal uses these facts to choose a
// safe outbox strategy instead of inferring provider behavior.
type ConversationDeliveryCapabilities struct {
	Operations                    []ConversationDeliveryOperation `json:"operations"`
	Ordering                      ConversationDeliveryOrdering    `json:"ordering"`
	Idempotency                   IdempotencyMode                 `json:"idempotency"`
	SupportsAcknowledgementLookup bool                            `json:"supportsAcknowledgementLookup,omitempty"`
	SupportsRetryAfter            bool                            `json:"supportsRetryAfter,omitempty"`
}

// ConversationAdapterTransport identifies Skill-owned executable adapter
// entrypoints. A generic host invokes these entrypoints; provider-specific code
// remains part of the Skill package rather than the host or kernel.
type ConversationAdapterTransport struct {
	Kind                string   `json:"kind"`
	IngressEndpoint     string   `json:"ingressEndpoint"`
	DeliveryEndpoint    string   `json:"deliveryEndpoint"`
	IngressCredentials  []string `json:"ingressCredentials,omitempty"`
	DeliveryCredentials []string `json:"deliveryCredentials,omitempty"`
}

// ConversationDestinationDiscovery declares how a host can enumerate one
// class of destinations through a read-only Skill action. The action remains
// governed by the normal Skill binding and credential contract; this mapping
// only gives product surfaces a deterministic, provider-neutral projection of
// its result. Paths use dot-separated object keys and are relative to either
// the action output (ItemsPath, NextCursorPath) or one item (the remaining
// paths).
type ConversationDestinationDiscovery struct {
	Action                    string                   `json:"action"`
	Mode                      ConversationEndpointMode `json:"mode"`
	ItemsPath                 string                   `json:"itemsPath"`
	IDPath                    string                   `json:"idPath"`
	DisplayNamePath           string                   `json:"displayNamePath"`
	DescriptionPath           string                   `json:"descriptionPath,omitempty"`
	InstallationIDPath        string                   `json:"installationIdPath,omitempty"`
	ApplicationIDPath         string                   `json:"applicationIdPath,omitempty"`
	ConnectionDisplayNamePath string                   `json:"connectionDisplayNamePath,omitempty"`
	CursorArgument            string                   `json:"cursorArgument,omitempty"`
	LimitArgument             string                   `json:"limitArgument,omitempty"`
	QueryArgument             string                   `json:"queryArgument,omitempty"`
	NextCursorPath            string                   `json:"nextCursorPath,omitempty"`
}

// ConversationAdapter declares one provider adapter supplied by a Skill.
// Inbound payload normalization and outbound delivery are not model tools.
// Credentials are resolved out of band from the exact Skill binding.
type ConversationAdapter struct {
	ProtocolVersion      string                             `json:"protocolVersion"`
	Name                 string                             `json:"name"`
	Description          string                             `json:"description"`
	Provider             string                             `json:"provider"`
	EndpointModes        []ConversationEndpointMode         `json:"endpointModes"`
	InboundEventTypes    []string                           `json:"inboundEventTypes"`
	Features             []ConversationAdapterFeature       `json:"features,omitempty"`
	Credentials          []CredentialRequirement            `json:"credentials,omitempty"`
	DestinationDiscovery []ConversationDestinationDiscovery `json:"destinationDiscovery,omitempty"`
	Delivery             ConversationDeliveryCapabilities   `json:"delivery"`
	Transport            ConversationAdapterTransport       `json:"transport"`
}

const CallbackAdapterProtocolV1 = "openseal.callback.adapter/v1"

const CallbackEventApprovalDecided = "approval.decided"

// CallbackAdapterTransport identifies the Skill-owned verifier and
// normalizer for an inbound provider callback. Credential values are resolved
// by the trusted host and never enter the callback registration or event.
type CallbackAdapterTransport struct {
	Kind               string   `json:"kind"`
	IngressEndpoint    string   `json:"ingressEndpoint"`
	IngressCredentials []string `json:"ingressCredentials,omitempty"`
}

// CallbackAdapter declares one provider-neutral inbound callback surface.
// EventTypes are the credential-free EventEnvelope types the adapter may emit
// after it authenticates the raw provider request.
type CallbackAdapter struct {
	ProtocolVersion string                   `json:"protocolVersion"`
	Name            string                   `json:"name"`
	Description     string                   `json:"description"`
	Provider        string                   `json:"provider"`
	EventTypes      []string                 `json:"eventTypes"`
	Credentials     []CredentialRequirement  `json:"credentials,omitempty"`
	Transport       CallbackAdapterTransport `json:"transport"`
}

type Requirements struct {
	OperatingSystems []string             `json:"operatingSystems,omitempty"`
	Executables      []string             `json:"executables,omitempty"`
	AnyExecutables   []string             `json:"anyExecutables,omitempty"`
	Environment      []string             `json:"environment,omitempty"`
	Configuration    []string             `json:"configuration,omitempty"`
	Compatibility    string               `json:"compatibility,omitempty"`
	AlwaysAvailable  bool                 `json:"alwaysAvailable,omitempty"`
	Storage          []StorageRequirement `json:"storage,omitempty"`
	Compute          *ComputeRequirements `json:"compute,omitempty"`
}

// StorageDurability describes whether a hosted Skill may lose workspace data
// when its process or host is replaced. It is deliberately host-neutral: a
// Kubernetes host may materialize persistent storage as a PVC while another
// host can use an encrypted volume or another durable implementation.
type StorageDurability string

const (
	StorageDurabilityEphemeral  StorageDurability = "ephemeral"
	StorageDurabilityPersistent StorageDurability = "persistent"
)

// StorageRetention controls what happens to persistent data when the Skill is
// no longer deployed. Retained data requires an explicit administrative
// lifecycle action to remove; delete permits the host to garbage-collect it.
type StorageRetention string

const (
	StorageRetentionDelete StorageRetention = "delete"
	StorageRetentionRetain StorageRetention = "retain"
)

type StorageRequirement struct {
	Name            string            `json:"name"`
	MountPath       string            `json:"mountPath"`
	Durability      StorageDurability `json:"durability"`
	MinimumCapacity string            `json:"minimumCapacity,omitempty"`
	Retention       StorageRetention  `json:"retention,omitempty"`
	// WritableGroup is the numeric group that must be able to write the
	// mounted storage. Hosts may satisfy this with a container group, a volume
	// ownership policy, or another equivalent mechanism.
	WritableGroup *int64 `json:"writableGroup,omitempty"`
}

// ComputeRequirements communicate minimum scheduling intent without exposing
// a host-specific pod, VM, or container contract.
type ComputeRequirements struct {
	Requests ComputeResources `json:"requests,omitempty"`
	Limits   ComputeResources `json:"limits,omitempty"`
}

type ComputeResources struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

type Installer struct {
	ID               string   `json:"id,omitempty"`
	Kind             string   `json:"kind"`
	Label            string   `json:"label,omitempty"`
	OperatingSystems []string `json:"operatingSystems,omitempty"`
	Executables      []string `json:"executables,omitempty"`
	Package          string   `json:"package,omitempty"`
	Module           string   `json:"module,omitempty"`
	Formula          string   `json:"formula,omitempty"`
	URL              string   `json:"url,omitempty"`
	Archive          string   `json:"archive,omitempty"`
	Extract          *bool    `json:"extract,omitempty"`
	StripComponents  *int     `json:"stripComponents,omitempty"`
	TargetDirectory  string   `json:"targetDirectory,omitempty"`
}

type Resource struct {
	Path      string `json:"path"`
	Kind      string `json:"kind"`
	MediaType string `json:"mediaType,omitempty"`
	Digest    string `json:"digest,omitempty"`
	Size      int64  `json:"size,omitempty"`
}

// Resource kinds are deliberately closed so a host can apply predictable
// permissions without guessing whether an imported artifact is executable.
const (
	ResourceKindFile      = "resource"
	ResourceKindScript    = "script"
	ResourceKindReference = "reference"
	ResourceKindAsset     = "asset"
)

type SourceProvenance struct {
	Identity        string                 `json:"identity,omitempty"`
	Format          string                 `json:"format"`
	Registry        string                 `json:"registry,omitempty"`
	Publisher       string                 `json:"publisher,omitempty"`
	Reference       string                 `json:"reference,omitempty"`
	ResolvedVersion string                 `json:"resolvedVersion,omitempty"`
	Digest          string                 `json:"digest,omitempty"`
	License         string                 `json:"license,omitempty"`
	Homepage        string                 `json:"homepage,omitempty"`
	Trust           map[string]interface{} `json:"trust,omitempty"`
}

type Definition struct {
	ID               string   `json:"id"`
	Version          string   `json:"version"`
	Name             string   `json:"name"`
	Description      string   `json:"description,omitempty"`
	Icon             string   `json:"icon,omitempty"`
	Category         string   `json:"category,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	ConfigurationKey string   `json:"configurationKey,omitempty"`
	// BindingConfigSchema defines non-secret, host-owned configuration that is
	// fixed when a Skill is bound. It is never part of model-visible action
	// input and is delivered to tool hosts separately from action arguments.
	BindingConfigSchema  map[string]interface{}         `json:"bindingConfigSchema,omitempty"`
	Actions              map[string]Action              `json:"actions"`
	Transport            TransportReference             `json:"transport"`
	Prompt               *PromptModule                  `json:"prompt,omitempty"`
	ConversationAdapters map[string]ConversationAdapter `json:"conversationAdapters,omitempty"`
	CallbackAdapters     map[string]CallbackAdapter     `json:"callbackAdapters,omitempty"`
	Requirements         Requirements                   `json:"requirements,omitempty"`
	Installers           []Installer                    `json:"installers,omitempty"`
	Resources            []Resource                     `json:"resources,omitempty"`
	Source               *SourceProvenance              `json:"source,omitempty"`
}

type ArgumentRule struct {
	Const interface{}   `json:"const,omitempty"`
	Enum  []interface{} `json:"enum,omitempty"`
}

type BindingArgumentSource string

const (
	// BindingArgumentLiteral fixes an action argument at binding time. The
	// value is host-owned, credential-free configuration and is never exposed
	// as a model-selectable input.
	BindingArgumentLiteral BindingArgumentSource = "literal"
	// BindingArgumentSessionID resolves the opaque durable session attached to
	// the current Run.
	BindingArgumentSessionID BindingArgumentSource = "session.id"
	// BindingArgumentVerifiedClaim resolves a claim that the embedding host
	// verified before creating the durable session. Query parameters and other
	// caller-provided context never populate this namespace.
	BindingArgumentVerifiedClaim BindingArgumentSource = "session.verified_claim"
)

// BindingArgumentValue binds one action input to trusted host context. Exactly
// one source is selected. Claim is required only for verified-claim sources;
// Literal is meaningful only for literal sources.
type BindingArgumentValue struct {
	Source  BindingArgumentSource `json:"source"`
	Literal interface{}           `json:"literal,omitempty"`
	Claim   string                `json:"claim,omitempty"`
}

type ScopeReference struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type Binding struct {
	ID                          string                                     `json:"id"`
	Scope                       ScopeReference                             `json:"scope"`
	DeploymentID                string                                     `json:"deploymentId"`
	SkillID                     string                                     `json:"skillId"`
	SkillVersion                string                                     `json:"skillVersion"`
	SourceIdentity              string                                     `json:"sourceIdentity,omitempty"`
	Disabled                    bool                                       `json:"disabled,omitempty"`
	AllowedActions              []string                                   `json:"allowedActions"`
	EnablePrompt                bool                                       `json:"enablePrompt,omitempty"`
	EnabledConversationAdapters []string                                   `json:"enabledConversationAdapters,omitempty"`
	EnabledCallbackAdapters     []string                                   `json:"enabledCallbackAdapters,omitempty"`
	MaximumRisk                 RiskLevel                                  `json:"maximumRisk"`
	ArgumentRestrictions        map[string]map[string]ArgumentRule         `json:"argumentRestrictions,omitempty"`
	ArgumentBindings            map[string]map[string]BindingArgumentValue `json:"argumentBindings,omitempty"`
	Credentials                 map[string]CredentialReference             `json:"credentials,omitempty"`
	Config                      map[string]interface{}                     `json:"config,omitempty"`
	Revision                    int64                                      `json:"revision"`
	CreatedAt                   time.Time                                  `json:"createdAt,omitempty"`
	UpdatedAt                   time.Time                                  `json:"updatedAt,omitempty"`
	Lifecycle                   []BindingLifecycleEntry                    `json:"lifecycle,omitempty"`
}

type BindingLifecycleAction string

const (
	BindingLifecycleCreated  BindingLifecycleAction = "created"
	BindingLifecycleUpdated  BindingLifecycleAction = "updated"
	BindingLifecycleEnabled  BindingLifecycleAction = "enabled"
	BindingLifecycleDisabled BindingLifecycleAction = "disabled"
)

// BindingActor identifies the principal responsible for a management change.
// Hosts authenticate and authorize this principal before invoking the portable
// contract; OpenSeal persists only the product-neutral attribution.
type BindingActor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type BindingLifecycleEntry struct {
	Revision int64                  `json:"revision"`
	Action   BindingLifecycleAction `json:"action"`
	Actor    BindingActor           `json:"actor"`
	Reason   string                 `json:"reason"`
	At       time.Time              `json:"at"`
}

type ModelAction struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	BindingID       string `json:"bindingId"`
	BindingRevision int64  `json:"bindingRevision"`
	// DeploymentID identifies the Agent or Team binding owner. It is supplied
	// by the kernel, never by the model, and prevents a mixed Team turn from
	// resolving an Agent action against Team credentials (or vice versa).
	DeploymentID            string                  `json:"deploymentId,omitempty"`
	SkillID                 string                  `json:"skillId"`
	Version                 string                  `json:"version"`
	Action                  string                  `json:"action"`
	InputSchema             map[string]interface{}  `json:"inputSchema"`
	SemanticArguments       map[string]string       `json:"semanticArguments,omitempty"`
	Risk                    RiskLevel               `json:"risk"`
	SideEffect              SideEffect              `json:"sideEffect"`
	ExternalOperationPolicy ExternalOperationPolicy `json:"externalOperationPolicy,omitempty"`
}

// BindingReference selects one exact, versioned binding. Persisting this
// reference on an ActionCall prevents execution from drifting to another
// account, credential set, configuration, or policy revision.
type BindingReference struct {
	ID       string `json:"id"`
	Revision int64  `json:"revision"`
}

type ModelPrompt struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	BindingID       string `json:"bindingId"`
	BindingRevision int64  `json:"bindingRevision"`
	SkillID         string `json:"skillId"`
	Version         string `json:"version"`
	AlwaysActive    bool   `json:"alwaysActive,omitempty"`
	UserInvocable   bool   `json:"userInvocable"`
	ModelInvocable  bool   `json:"modelInvocable"`
}

type BoundAction struct {
	Definition *Definition
	Action     Action
	Binding    *Binding
}

type BoundConversationAdapter struct {
	Definition *Definition
	AdapterID  string
	Adapter    ConversationAdapter
	Binding    *Binding
}

type BoundCallbackAdapter struct {
	Definition *Definition
	AdapterID  string
	Adapter    CallbackAdapter
	Binding    *Binding
}
