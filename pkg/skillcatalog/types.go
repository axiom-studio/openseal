// Package skillcatalog holds the credential-free Skill capability catalog a
// host projects for discovery, setup and authority decisions. It carries no
// credentials, executor details or model output.
package skillcatalog

import (
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/source"
)

type SkillCapability struct {
	ID             string `json:"id"`
	Version        string `json:"version,omitempty"`
	SourceIdentity string `json:"sourceIdentity,omitempty"`
	// RuntimeIdentity is the exact installed immutable definition selected by
	// the host for this catalog entry. It is persisted with authoring state but
	// removed from provider prompts: models select catalog ids, while OpenSeal
	// deterministically places runtime authority.
	RuntimeIdentity *capability.SkillIdentity       `json:"runtimeIdentity,omitempty"`
	Name            string                          `json:"name,omitempty"`
	Description     string                          `json:"description,omitempty"`
	Actions         []string                        `json:"actions,omitempty"`
	ActionRisks     map[string]capability.RiskLevel `json:"actionRisks,omitempty"`
	// ActionContracts are host-owned, credential-free action envelopes used for
	// deterministic validation. Provider prompt projections must strip them.
	ActionContracts map[string]SkillActionContract `json:"actionContracts,omitempty"`
	// HostedModelInputTokens is a host-attested conservative ceiling for this
	// Skill's complete per-Turn model projection: prompt instructions, action
	// descriptions and schemas, and bounded binding metadata. It contains no
	// prompt text or credential data and is removed from provider prompts.
	HostedModelInputTokens int64                           `json:"hostedModelInputTokens,omitempty"`
	CredentialKinds        []string                        `json:"credentialKinds,omitempty"`
	Credentials            []SkillCredential               `json:"credentials,omitempty"`
	ConversationAdapters   []ConversationAdapterCapability `json:"conversationAdapters,omitempty"`
	CallbackAdapters       []CallbackAdapterCapability     `json:"callbackAdapters,omitempty"`
	BindingConfigSchema    map[string]interface{}          `json:"bindingConfigSchema,omitempty"`
	PromptAvailable        bool                            `json:"promptAvailable,omitempty"`
	MaximumRisk            capability.RiskLevel            `json:"maximumRisk,omitempty"`
	Readiness              SkillReadiness                  `json:"readiness,omitempty"`
	Compatibility          []SkillCompatibility            `json:"compatibility,omitempty"`
}

// SkillActionContract is the exact model-callable envelope for one authorized
// Skill action. It contains no binding, credential, or executor details.
type SkillActionContract struct {
	Description string                 `json:"description,omitempty"`
	InputSchema map[string]interface{} `json:"inputSchema,omitempty"`
	// OutputSchema remains host-owned and is used only to prove that one
	// deterministic Runbook step can safely feed another. Provider catalog
	// projections remove the complete ActionContracts map.
	OutputSchema      map[string]interface{} `json:"outputSchema,omitempty"`
	SemanticArguments map[string]string      `json:"semanticArguments,omitempty"`
	Risk              capability.RiskLevel   `json:"risk,omitempty"`
	SideEffect        capability.SideEffect  `json:"sideEffect,omitempty"`
}

// ConversationAdapterCapability is the model-safe projection used to compose
// reactive conversation endpoints. It contains no executable entrypoint,
// opaque connection reference, or external account identity.
type ConversationAdapterCapability struct {
	ID                           string                                      `json:"id"`
	ProtocolVersion              string                                      `json:"protocolVersion"`
	Provider                     string                                      `json:"provider"`
	EndpointModes                []capability.ConversationEndpointMode       `json:"endpointModes"`
	InboundEventTypes            []string                                    `json:"inboundEventTypes"`
	Features                     []capability.ConversationAdapterFeature     `json:"features,omitempty"`
	DiscoverableDestinationModes []capability.ConversationEndpointMode       `json:"discoverableDestinationModes,omitempty"`
	Delivery                     capability.ConversationDeliveryCapabilities `json:"delivery"`
	Credentials                  []SkillCredential                           `json:"credentials,omitempty"`
}

// CallbackAdapterCapability is the credential-free authoring projection of a
// Skill-owned callback verifier. The compiler uses it to bind interactive
// workflow edges to one exact adapter; models never choose callback routes or
// executable entrypoints.
type CallbackAdapterCapability struct {
	ID              string            `json:"id"`
	ProtocolVersion string            `json:"protocolVersion"`
	Provider        string            `json:"provider"`
	EventTypes      []string          `json:"eventTypes"`
	Credentials     []SkillCredential `json:"credentials,omitempty"`
}

type SkillReadiness string

const (
	SkillReadinessReady        SkillReadiness = "ready"
	SkillReadinessNeedsBinding SkillReadiness = "needs_binding"
	// NeedsInstallation is safe to offer only with exact version/source
	// identity and referenced positive compatibility evidence. The reference
	// is opaque and host-owned (for example, a compilation preview receipt).
	SkillReadinessNeedsInstallation SkillReadiness = "needs_installation"
	SkillReadinessUnavailable       SkillReadiness = "unavailable"
)

// SkillCompatibility is host-supplied, credential-free evidence. Catalog
// consumers may rank it but must not infer compatibility absent this fact.
type SkillCompatibility struct {
	Requirement string `json:"requirement"`
	Compatible  bool   `json:"compatible"`
	Evidence    string `json:"evidence"`
	Reference   string `json:"reference,omitempty"`
}

type SkillCredential struct {
	Name     string                        `json:"name"`
	Kind     string                        `json:"kind"`
	Actions  []string                      `json:"actions,omitempty"`
	Optional bool                          `json:"optional,omitempty"`
	OAuth2   *capability.OAuth2Requirement `json:"oauth2,omitempty"`
}

// SourcePolicyCapability is the credential-free authoring projection of a
// host-governed source policy. Hosts retain policy storage and enforcement;
// planners can select only references and source scopes that actually exist.
type SourcePolicyCapability struct {
	Reference      string                         `json:"reference"`
	Sources        []SourcePolicySourceCapability `json:"sources"`
	MaximumItems   int                            `json:"maximumItems,omitempty"`
	RetentionDays  int                            `json:"retentionDays,omitempty"`
	ApprovalPolicy string                         `json:"approvalPolicy,omitempty"`
}

type SourcePolicySourceCapability struct {
	Host         string   `json:"host"`
	PathPrefixes []string `json:"pathPrefixes,omitempty"`
}

type CapabilityCatalog struct {
	Skills                      map[string]SkillCapability   `json:"skills,omitempty"`
	CapabilityNeeds             []CapabilityNeed             `json:"capabilityNeeds,omitempty"`
	AgentCredentialRequirements []AgentCredentialRequirement `json:"agentCredentialRequirements,omitempty"`
	AvailableCredentials        map[string]bool              `json:"availableCredentials,omitempty"`
	// AvailableCredentialGrants contains credential-free host attestations
	// used only for deterministic OAuth 2 readiness. Opaque references and
	// external account identifiers remain outside the model-visible catalog.
	AvailableCredentialGrants map[string][]capability.OAuth2GrantSummary `json:"availableCredentialGrants,omitempty"`
	SourcePolicies            map[string]SourcePolicyCapability          `json:"sourcePolicies,omitempty"`
	AuthorityConstraint       *AuthorityConstraint                       `json:"authorityConstraint,omitempty"`
	Diagnostics               []CatalogDiagnostic                        `json:"diagnostics,omitempty"`
	RuntimeComposition        *RuntimeCompositionCapability              `json:"runtimeComposition,omitempty"`
	HostedExecution           *HostedExecutionCapability                 `json:"hostedExecution,omitempty"`
}

const HostedExecutionProtocolV1 = "openseal.hosted-execution/v1"

// HostedExecutionCapability lets a host attest to the non-Skill portion of a
// hosted model exchange. Skill-specific ceilings live beside each exact Skill
// capability so OpenSeal can validate a reviewed composition without learning
// private prompt instructions or host binding details.
type HostedExecutionCapability struct {
	ProtocolVersion     string `json:"protocolVersion"`
	BaseInputTokens     int64  `json:"baseInputTokens"`
	MinimumOutputTokens int64  `json:"minimumOutputTokens"`
}

const RuntimeCompositionProtocolV1 = "openseal.runtime-composition/v1"

type RuntimeCompositionCapability struct {
	ProtocolVersion string                        `json:"protocolVersion"`
	Conversation    ConversationRuntimeCapability `json:"conversation"`
	Runbook         RunbookRuntimeCapability      `json:"runbook"`
}

type ConversationRuntimeCapability struct {
	EventTypes     []string                  `json:"eventTypes"`
	HandlerKinds   []ConversationHandlerKind `json:"handlerKinds"`
	ReplyModes     []ConversationReplyMode   `json:"replyModes"`
	CanonicalReply bool                      `json:"canonicalReply"`
}

type RunbookRuntimeCapability struct {
	TriggerKinds                   []runbook.TriggerKind  `json:"triggerKinds"`
	StepKinds                      []runbook.StepKind     `json:"stepKinds"`
	CanInvokeAgents                bool                   `json:"canInvokeAgents"`
	ConversationTriggerInputSchema map[string]interface{} `json:"conversationTriggerInputSchema"`
}

// AgentCredentialRequirement describes a deployment credential slot required
// by the portable runtime. Hosts advertise authorized opaque choices for the
// slot through CredentialBindingChoice.BindingKeys. Secret values and
// host-specific credential storage never cross this contract.
type AgentCredentialRequirement struct {
	BindingKey            string `json:"bindingKey"`
	DisplayName           string `json:"displayName"`
	Prompt                string `json:"prompt"`
	RequiredForActivation bool   `json:"requiredForActivation,omitempty"`
}

// AuthorityConstraint is the credential-free, versioned projection of the
// host policy that bounds authored Agent authority. The host remains the
// policy owner and enforcer; OpenSeal uses this projection to prevent a model
// candidate from reaching evaluation with authority that the host will reject.
//
// RequireApprovalAt is a ceiling on the approval threshold: an Agent capable
// of that risk (or a higher risk) must require approval at this threshold or
// earlier. MaximumRisk is optional; when present, candidates above it fail
// closed because silently reducing requested authority could change intent.
type AuthorityConstraint struct {
	ID                string               `json:"id"`
	Version           string               `json:"version"`
	MaximumRisk       capability.RiskLevel `json:"maximumRisk,omitempty"`
	RequireApprovalAt capability.RiskLevel `json:"requireApprovalAt,omitempty"`
}

// CapabilityNeed is a server-owned, prompt-matched choice between exact
// verified Skill catalog entries. It contains no registry query, credential,
// or model-authored inference. The compiler turns it into one deterministic
// refinement question when more than one viable Skill can satisfy the need,
// or when the host explicitly requires the operator to choose.
type CapabilityNeed struct {
	ID             string                            `json:"id"`
	Prompt         string                            `json:"prompt"`
	WhyNeeded      string                            `json:"whyNeeded"`
	SkillIDs       []string                          `json:"skillIds"`
	ChoiceRequired bool                              `json:"choiceRequired,omitempty"`
	Priority       int                               `json:"priority"`
	SourceScope    *CapabilitySourceScopeRequirement `json:"sourceScope,omitempty"`
	// SourcePolicyProposal is an exact, credential-free policy draft supplied by
	// the trusted capability catalog. It is never active authority: compilation
	// may surface it for review, while registration and activation remain
	// separate governed lifecycle operations.
	SourcePolicyProposal *CapabilitySourcePolicyProposal `json:"sourcePolicyProposal,omitempty"`
}

type CapabilitySourcePolicyProposal struct {
	Policy   source.Policy `json:"policy"`
	Reason   string        `json:"reason"`
	SkillIDs []string      `json:"skillIds"`
}

// CapabilitySourceScopeRequirement declares that a source-oriented capability
// cannot be authored safely until its concrete targets are supplied. The host
// derives this fact from deterministic intent matching; the provider cannot
// omit it or turn an empty monitoring plan into a ready ChangeSet.
type CapabilitySourceScopeRequirement struct {
	Prompt    string `json:"prompt"`
	WhyNeeded string `json:"whyNeeded"`
	Minimum   int    `json:"minimum"`
	Maximum   int    `json:"maximum"`
	Priority  int    `json:"priority"`
	// Targets are exact, non-secret source boundaries deterministically
	// extracted by the trusted host from the user's prompt. Their presence
	// satisfies the guided question and lets the compiler materialize the same
	// audited values into a durable capability invocation.
	Targets                  []string `json:"targets,omitempty"`
	MaterializationInputKeys []string `json:"materializationInputKeys,omitempty"`
}

const (
	MaximumCapabilityNeeds            = 16
	MaximumCapabilityNeedSkillChoices = 16
)

const MaximumCatalogDiagnostics = 16

const (
	CatalogDiagnosticNoCompatibleCapability = "capability_discovery_no_compatible_candidate"
	CatalogDiagnosticDiscoveryUnavailable   = "capability_discovery_unavailable"
	CatalogDiagnosticDiscoveryTimeout       = "capability_discovery_timeout"
	CatalogDiagnosticDiscoveryStale         = "capability_discovery_stale"
	CatalogDiagnosticExecutionTargetMissing = "execution_target_unavailable"
)

// CatalogDiagnostic is bounded, host-supplied availability guidance. It is a
// fact about discovery, never a Skill candidate or an instruction. Reference
// may identify a safe typed capability intent; it must never contain a raw
// query, registry artifact, provider error, credential reference, or secret.
type CatalogDiagnostic struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Reference string `json:"reference,omitempty"`
}

type ConversationHandlerKind string

const (
	ConversationHandlerAgent   ConversationHandlerKind = "agent"
	ConversationHandlerTeam    ConversationHandlerKind = "team"
	ConversationHandlerRunbook ConversationHandlerKind = "runbook"
)

type ConversationReplyMode string

const (
	ConversationReplyProviderDefault ConversationReplyMode = "provider_default"
	ConversationReplyThread          ConversationReplyMode = "thread"
	ConversationReplyChannel         ConversationReplyMode = "channel"
)
