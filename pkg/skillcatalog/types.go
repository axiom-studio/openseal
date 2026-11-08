// Package skillcatalog holds the credential-free Skill capability catalog a
// host projects for discovery, setup and authority decisions. It carries no
// credentials, executor details or model output.
package skillcatalog

import (
	"github.com/axiom-studio/openseal/pkg/capability"
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

type CapabilityCatalog struct {
	Skills               map[string]SkillCapability `json:"skills,omitempty"`
	AvailableCredentials map[string]bool            `json:"availableCredentials,omitempty"`
	// AvailableCredentialGrants contains credential-free host attestations
	// used only for deterministic OAuth 2 readiness. Opaque references and
	// external account identifiers remain outside the model-visible catalog.
	AvailableCredentialGrants map[string][]capability.OAuth2GrantSummary `json:"availableCredentialGrants,omitempty"`
	AuthorityConstraint       *AuthorityConstraint                       `json:"authorityConstraint,omitempty"`
	Diagnostics               []CatalogDiagnostic                        `json:"diagnostics,omitempty"`
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
