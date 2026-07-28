// Package authoring compiles natural-language intent into reviewable,
// product-neutral workforce candidates. Compilation never activates state.
package authoring

import (
	"context"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/source"
	"github.com/axiom-studio/openseal/pkg/team"
)

type Mode string

const (
	ModeCreate Mode = "create"
	ModeAmend  Mode = "amend"
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
	ActionContracts      map[string]SkillActionContract  `json:"actionContracts,omitempty"`
	CredentialKinds      []string                        `json:"credentialKinds,omitempty"`
	Credentials          []SkillCredential               `json:"credentials,omitempty"`
	ConversationAdapters []ConversationAdapterCapability `json:"conversationAdapters,omitempty"`
	BindingConfigSchema  map[string]interface{}          `json:"bindingConfigSchema,omitempty"`
	PromptAvailable      bool                            `json:"promptAvailable,omitempty"`
	MaximumRisk          capability.RiskLevel            `json:"maximumRisk,omitempty"`
	Readiness            SkillReadiness                  `json:"readiness,omitempty"`
	Compatibility        []SkillCompatibility            `json:"compatibility,omitempty"`
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

type Assignment struct {
	ID                string `json:"id"`
	RoleID            string `json:"roleId"`
	AgentDefinitionID string `json:"agentDefinitionId"`
	DisplayName       string `json:"displayName,omitempty"`
}

type WorkforceCandidate struct {
	Agents                []*agent.AgentDefinition        `json:"agents"`
	Team                  *team.Definition                `json:"team,omitempty"`
	Assignments           []Assignment                    `json:"assignments,omitempty"`
	Project               *ProjectBlueprint               `json:"project,omitempty"`
	ConversationEndpoints []ConversationEndpointBlueprint `json:"conversationEndpoints,omitempty"`
	// Activation is the reviewed, digest-bound operating state that atomic
	// apply must materialize. The Compiler derives it from typed commitments;
	// apply never infers it from prompt prose.
	Activation WorkforceActivationIntent `json:"activation"`
}

type ConversationEndpointOwnerType string

const (
	ConversationEndpointOwnerAgent ConversationEndpointOwnerType = "agent"
	ConversationEndpointOwnerTeam  ConversationEndpointOwnerType = "team"
)

type ConversationHandlerKind string

const (
	ConversationHandlerAgent   ConversationHandlerKind = "agent"
	ConversationHandlerTeam    ConversationHandlerKind = "team"
	ConversationHandlerRunbook ConversationHandlerKind = "runbook"
)

type ConversationMessageSelection string

const (
	ConversationSelectAllMessages     ConversationMessageSelection = "all_messages"
	ConversationSelectMentions        ConversationMessageSelection = "mentions"
	ConversationSelectDirectOrMention ConversationMessageSelection = "direct_or_mentions"
)

type ConversationReplyMode string

const (
	ConversationReplyProviderDefault ConversationReplyMode = "provider_default"
	ConversationReplyThread          ConversationReplyMode = "thread"
	ConversationReplyChannel         ConversationReplyMode = "channel"
)

type ConversationEndpointOwner struct {
	Type ConversationEndpointOwnerType `json:"type"`
	ID   string                        `json:"id"`
}

type ConversationHandlerBlueprint struct {
	Kind              ConversationHandlerKind `json:"kind"`
	AgentDefinitionID string                  `json:"agentDefinitionId,omitempty"`
	RunbookID         string                  `json:"runbookId,omitempty"`
	RunbookVersion    string                  `json:"runbookVersion,omitempty"`
	Trigger           string                  `json:"trigger,omitempty"`
}

type ConversationEndpointPolicyBlueprint struct {
	MessageSelection ConversationMessageSelection `json:"messageSelection"`
	ReplyMode        ConversationReplyMode        `json:"replyMode"`
	IgnoreBots       bool                         `json:"ignoreBots"`
}

// ConversationEndpointBlueprint is reviewable authoring intent. Exact Skill
// bindings, OAuth connections, provider installation identities, and external
// addresses are resolved later through governed placement.
type ConversationEndpointBlueprint struct {
	ID                 string                              `json:"id"`
	Name               string                              `json:"name"`
	Owner              ConversationEndpointOwner           `json:"owner"`
	SkillID            string                              `json:"skillId"`
	SkillVersion       string                              `json:"skillVersion"`
	AdapterID          string                              `json:"adapterId"`
	Mode               capability.ConversationEndpointMode `json:"mode"`
	Address            string                              `json:"address,omitempty"`
	Handler            ConversationHandlerBlueprint        `json:"handler"`
	Policy             ConversationEndpointPolicyBlueprint `json:"policy"`
	CanonicalReply     bool                                `json:"canonicalReply"`
	ArchitectureReason string                              `json:"architectureReason"`
}

type WorkforceActivationIntent string

const (
	WorkforceActivationActive   WorkforceActivationIntent = "active"
	WorkforceActivationInactive WorkforceActivationIntent = "inactive"
)

// EffectiveWorkforceActivationIntent preserves active apply for ChangeSets
// persisted before activation became an explicit candidate field. Every newly
// compiled candidate records one of the two concrete values above.
func EffectiveWorkforceActivationIntent(value WorkforceActivationIntent) (WorkforceActivationIntent, error) {
	switch value {
	case "", WorkforceActivationActive:
		return WorkforceActivationActive, nil
	case WorkforceActivationInactive:
		return WorkforceActivationInactive, nil
	default:
		return "", fmt.Errorf("workforce activation intent %q is invalid", value)
	}
}

type GenerateRequest struct {
	Mode                    Mode                            `json:"mode"`
	Prompt                  string                          `json:"prompt"`
	Existing                *WorkforceCandidate             `json:"existing,omitempty"`
	Catalog                 CapabilityCatalog               `json:"catalog"`
	CompositionRequirements *RuntimeCompositionRequirements `json:"compositionRequirements,omitempty"`
	Refinement              *RefinementContext              `json:"refinement,omitempty"`
	InvocationKey           string                          `json:"invocationKey,omitempty"`
}

// RuntimeCompositionRequirements are deterministic, server-owned obligations
// derived from user intent and the authorized runtime catalog before the
// probabilistic planner runs. They prevent a provider from replacing an
// executable event path with prose and tell it which smallest handler patterns
// are valid without exposing host implementation details.
type RuntimeCompositionRequirements struct {
	Conversation *ConversationCompositionRequirement `json:"conversation,omitempty"`
}

type ConversationCompositionRequirement struct {
	EventType        string                    `json:"eventType"`
	HandlerKinds     []ConversationHandlerKind `json:"handlerKinds"`
	CanonicalReply   bool                      `json:"canonicalReply"`
	ArchitectureRule string                    `json:"architectureRule"`
}

const (
	ConversationArchitectureDirect       = "smallest_direct_handler"
	ConversationArchitectureOrchestrated = "durable_runbook_orchestration"
)

// Generator is the only probabilistic boundary. Implementations may use an
// LLM, a test fixture, or another planner, but must return one strict JSON
// GenerationResponse. The Compiler independently verifies its output.
type Generator interface {
	Generate(context.Context, GenerateRequest) ([]byte, error)
}

type RepairGenerator interface {
	Repair(context.Context, GenerateRequest, []byte, error) ([]byte, error)
}

// CompilePhase identifies the current deterministic boundary of probabilistic
// workforce authoring. It is deliberately credential- and payload-free so a
// host can safely persist it as Run progress.
type CompilePhase string

const (
	CompilePhaseCapabilityResolve CompilePhase = "resolving_capabilities"
	CompilePhaseProviderRequest   CompilePhase = "requesting_provider"
	CompilePhaseSchemaRepair      CompilePhase = "repairing_schema"
	CompilePhaseCandidateValidate CompilePhase = "validating_candidate"
	CompilePhaseContractRepair    CompilePhase = "repairing_contract"
)

// CompileProgress is emitted immediately before each potentially long-running
// provider call and deterministic validation pass. Attempt is one-based within
// the named phase; MaximumAttempts is the bounded phase budget.
type CompileProgress struct {
	Phase           CompilePhase `json:"phase"`
	Attempt         int          `json:"attempt"`
	MaximumAttempts int          `json:"maximumAttempts"`
}

type CompileProgressObserver func(CompileProgress)

type GenerationResponse struct {
	Candidate           WorkforceCandidate   `json:"candidate"`
	Commitments         PromptCommitments    `json:"commitments"`
	Assumptions         []string             `json:"assumptions,omitempty"`
	UnresolvedQuestions []RefinementQuestion `json:"unresolvedQuestions,omitempty"`
}

// PromptCommitments is the generator's typed, reviewable account of concrete
// facts it claims to have preserved from the user's prompt. The Compiler
// validates these facts against the candidate and independently extracts only
// a deliberately small grammar of unambiguous count, placement, inactivity,
// and approval clauses. Open-ended semantic equivalence remains outside this
// deterministic contract.
type PromptCommitments struct {
	AgentCount           *int                       `json:"agentCount,omitempty"`
	TeamCount            *int                       `json:"teamCount,omitempty"`
	ObjectiveCounts      []ObjectiveCountCommitment `json:"objectiveCounts,omitempty"`
	Activation           ActivationCommitment       `json:"activation,omitempty"`
	ApprovalRequirements []ApprovalCommitment       `json:"approvalRequirements,omitempty"`
}

type CommitmentOwnerType string

const (
	CommitmentOwnerWorkforce CommitmentOwnerType = "workforce"
	CommitmentOwnerAgent     CommitmentOwnerType = "agent"
	CommitmentOwnerTeam      CommitmentOwnerType = "team"
)

type ObjectiveCountCommitment struct {
	OwnerType CommitmentOwnerType `json:"ownerType"`
	OwnerID   string              `json:"ownerId,omitempty"`
	Count     int                 `json:"count"`
}

type ActivationCommitment string

const ActivationCommitmentInactive ActivationCommitment = "inactive"

type ApprovalCommitment struct {
	OwnerType         CommitmentOwnerType  `json:"ownerType"`
	OwnerID           string               `json:"ownerId,omitempty"`
	RequireApprovalAt capability.RiskLevel `json:"requireApprovalAt"`
}

type ValidationIssue struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type MissingRequirement struct {
	Kind       string                        `json:"kind"`
	ID         string                        `json:"id"`
	RequiredBy string                        `json:"requiredBy"`
	OAuth2     *capability.OAuth2Requirement `json:"oauth2,omitempty"`
}

type RiskChange struct {
	Path     string `json:"path"`
	Before   string `json:"before,omitempty"`
	After    string `json:"after"`
	Widening bool   `json:"widening,omitempty"`
}

type FieldDiff struct {
	Path         string `json:"path"`
	BeforeDigest string `json:"beforeDigest,omitempty"`
	AfterDigest  string `json:"afterDigest,omitempty"`
}

type CompileResult struct {
	Candidate             WorkforceCandidate     `json:"candidate"`
	Valid                 bool                   `json:"valid"`
	Commitments           PromptCommitments      `json:"commitments"`
	Assumptions           []string               `json:"assumptions,omitempty"`
	UnresolvedQuestions   []RefinementQuestion   `json:"unresolvedQuestions,omitempty"`
	Validation            []ValidationIssue      `json:"validation,omitempty"`
	MissingRequirements   []MissingRequirement   `json:"missingRequirements,omitempty"`
	SourcePolicyProposals []SourcePolicyProposal `json:"sourcePolicyProposals,omitempty"`
	RiskChanges           []RiskChange           `json:"riskChanges,omitempty"`
	Diff                  []FieldDiff            `json:"diff,omitempty"`
}

// SourcePolicyProposal is a deterministic review artifact, not authority. A
// host must independently authorize registration and activation, after which
// authoring regenerates against the active catalog entry.
type SourcePolicyProposal struct {
	APIVersion       string        `json:"apiVersion"`
	CapabilityNeedID string        `json:"capabilityNeedId"`
	Reference        string        `json:"reference"`
	Policy           source.Policy `json:"policy"`
	Reason           string        `json:"reason"`
	RequiredBy       []string      `json:"requiredBy"`
	RequiresApproval bool          `json:"requiresApproval"`
}
