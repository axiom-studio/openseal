// Package authoring compiles natural-language intent into reviewable,
// product-neutral workforce candidates. Compilation never activates state.
package authoring

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skillcatalog"
	"github.com/axiom-studio/openseal/pkg/source"
	"github.com/axiom-studio/openseal/pkg/team"
)

type Mode string

const (
	ModeCreate Mode = "create"
	ModeAmend  Mode = "amend"
)

// The capability catalog types live in pkg/skillcatalog.
type (
	SkillCapability                  = skillcatalog.SkillCapability
	SkillActionContract              = skillcatalog.SkillActionContract
	ConversationAdapterCapability    = skillcatalog.ConversationAdapterCapability
	CallbackAdapterCapability        = skillcatalog.CallbackAdapterCapability
	SkillReadiness                   = skillcatalog.SkillReadiness
	SkillCompatibility               = skillcatalog.SkillCompatibility
	SkillCredential                  = skillcatalog.SkillCredential
	SourcePolicyCapability           = skillcatalog.SourcePolicyCapability
	SourcePolicySourceCapability     = skillcatalog.SourcePolicySourceCapability
	CapabilityCatalog                = skillcatalog.CapabilityCatalog
	HostedExecutionCapability        = skillcatalog.HostedExecutionCapability
	RuntimeCompositionCapability     = skillcatalog.RuntimeCompositionCapability
	ConversationRuntimeCapability    = skillcatalog.ConversationRuntimeCapability
	RunbookRuntimeCapability         = skillcatalog.RunbookRuntimeCapability
	AgentCredentialRequirement       = skillcatalog.AgentCredentialRequirement
	AuthorityConstraint              = skillcatalog.AuthorityConstraint
	CapabilityNeed                   = skillcatalog.CapabilityNeed
	CapabilitySourcePolicyProposal   = skillcatalog.CapabilitySourcePolicyProposal
	CapabilitySourceScopeRequirement = skillcatalog.CapabilitySourceScopeRequirement
	CatalogDiagnostic                = skillcatalog.CatalogDiagnostic
)

const (
	SkillReadinessReady             = skillcatalog.SkillReadinessReady
	SkillReadinessNeedsBinding      = skillcatalog.SkillReadinessNeedsBinding
	SkillReadinessNeedsInstallation = skillcatalog.SkillReadinessNeedsInstallation
	SkillReadinessUnavailable       = skillcatalog.SkillReadinessUnavailable

	HostedExecutionProtocolV1    = skillcatalog.HostedExecutionProtocolV1
	RuntimeCompositionProtocolV1 = skillcatalog.RuntimeCompositionProtocolV1

	MaximumCapabilityNeeds            = skillcatalog.MaximumCapabilityNeeds
	MaximumCapabilityNeedSkillChoices = skillcatalog.MaximumCapabilityNeedSkillChoices
	MaximumCatalogDiagnostics         = skillcatalog.MaximumCatalogDiagnostics

	CatalogDiagnosticNoCompatibleCapability = skillcatalog.CatalogDiagnosticNoCompatibleCapability
	CatalogDiagnosticDiscoveryUnavailable   = skillcatalog.CatalogDiagnosticDiscoveryUnavailable
	CatalogDiagnosticDiscoveryTimeout       = skillcatalog.CatalogDiagnosticDiscoveryTimeout
	CatalogDiagnosticDiscoveryStale         = skillcatalog.CatalogDiagnosticDiscoveryStale
	CatalogDiagnosticExecutionTargetMissing = skillcatalog.CatalogDiagnosticExecutionTargetMissing
)

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
	Activation WorkforceActivationIntent `json:"activation,omitempty"`
}

type ConversationEndpointOwnerType string

const (
	ConversationEndpointOwnerAgent ConversationEndpointOwnerType = "agent"
	ConversationEndpointOwnerTeam  ConversationEndpointOwnerType = "team"
)

type ConversationHandlerKind = skillcatalog.ConversationHandlerKind

const (
	ConversationHandlerAgent   = skillcatalog.ConversationHandlerAgent
	ConversationHandlerTeam    = skillcatalog.ConversationHandlerTeam
	ConversationHandlerRunbook = skillcatalog.ConversationHandlerRunbook
)

type ConversationMessageSelection string

const (
	ConversationSelectAllMessages     ConversationMessageSelection = "all_messages"
	ConversationSelectMentions        ConversationMessageSelection = "mentions"
	ConversationSelectDirectOrMention ConversationMessageSelection = "direct_or_mentions"
)

type ConversationReplyMode = skillcatalog.ConversationReplyMode

const (
	ConversationReplyProviderDefault = skillcatalog.ConversationReplyProviderDefault
	ConversationReplyThread          = skillcatalog.ConversationReplyThread
	ConversationReplyChannel         = skillcatalog.ConversationReplyChannel
)

// ConversationEndpointPurpose is authoring intent, not runtime authority. It
// is deliberately small enough to render as a checkbox/select in any authoring
// surface. The deterministic compiler turns selected purposes into the exact
// kernel policy and bindings required at apply time.
type ConversationEndpointPurpose string

const (
	ConversationEndpointPurposeConversation ConversationEndpointPurpose = "conversation"
	ConversationEndpointPurposeApprovals    ConversationEndpointPurpose = "approvals"
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
	ID           string                    `json:"id"`
	Name         string                    `json:"name"`
	Owner        ConversationEndpointOwner `json:"owner"`
	SkillID      string                    `json:"skillId"`
	SkillVersion string                    `json:"skillVersion"`
	AdapterID    string                    `json:"adapterId"`
	// CallbackAdapterID is compiler-owned. It is present only when a reviewed
	// endpoint purpose requires signed provider callbacks (for example an
	// approval decision), and atomically materializes the internal callback
	// registration during apply.
	CallbackAdapterID  string                              `json:"callbackAdapterId,omitempty"`
	Mode               capability.ConversationEndpointMode `json:"mode"`
	Address            string                              `json:"address,omitempty"`
	Handler            ConversationHandlerBlueprint        `json:"handler"`
	Policy             ConversationEndpointPolicyBlueprint `json:"policy"`
	CanonicalReply     bool                                `json:"canonicalReply"`
	Purposes           []ConversationEndpointPurpose       `json:"purposes,omitempty"`
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
	// ProfileOnly creates one identity without selecting Skills or planning work.
	// Capabilities and connections are discovered when the user talks to it.
	ProfileOnly             bool                            `json:"profileOnly,omitempty"`
	AgentName               string                          `json:"agentName,omitempty"`
	ExistingAgentNames      []string                        `json:"existingAgentNames,omitempty"`
	Mode                    Mode                            `json:"mode"`
	Prompt                  string                          `json:"prompt"`
	Existing                *WorkforceCandidate             `json:"existing,omitempty"`
	Catalog                 CapabilityCatalog               `json:"catalog"`
	Form                    AuthoringForm                   `json:"authoringForm"`
	CompositionRequirements *RuntimeCompositionRequirements `json:"compositionRequirements,omitempty"`
	Refinement              *RefinementContext              `json:"refinement,omitempty"`
	InvocationKey           string                          `json:"invocationKey,omitempty"`
}

const AuthoringFormVersionV1 = "openseal.authoring-form/v1"

type AuthoringInputKind string

const (
	AuthoringInputText         AuthoringInputKind = "text"
	AuthoringInputBoolean      AuthoringInputKind = "boolean"
	AuthoringInputSingleSelect AuthoringInputKind = "single_select"
	AuthoringInputMultiSelect  AuthoringInputKind = "multi_select"
)

// AuthoringForm is the model-safe equivalent of a product form. A planner
// supplies only these semantic inputs. CompilerOutput documents the immutable
// runtime field produced by trusted compilation and is never model-writable.
type AuthoringForm struct {
	Version string               `json:"version"`
	Fields  []AuthoringFormField `json:"fields,omitempty"`
	Values  []AuthoringFormValue `json:"values,omitempty"`
}

type AuthoringFormField struct {
	ID             string                   `json:"id"`
	Path           string                   `json:"path"`
	Label          string                   `json:"label"`
	Help           string                   `json:"help"`
	Input          AuthoringInputKind       `json:"input"`
	Required       bool                     `json:"required,omitempty"`
	Options        []AuthoringFormOption    `json:"options,omitempty"`
	EnableWhen     []AuthoringFormCondition `json:"enableWhen,omitempty"`
	CompilerOutput string                   `json:"compilerOutput,omitempty"`
}

// AuthoringFormValue is deliberately a typed union rather than arbitrary JSON.
// SubjectID identifies one item in a repeated form section, such as an
// endpoint. Exactly the value member appropriate for the field input is set.
type AuthoringFormValue struct {
	FieldID   string   `json:"fieldId"`
	SubjectID string   `json:"subjectId,omitempty"`
	Text      *string  `json:"text,omitempty"`
	Boolean   *bool    `json:"boolean,omitempty"`
	OptionIDs []string `json:"optionIds,omitempty"`
}

type AuthoringFormSubmission struct {
	Version string               `json:"version"`
	Values  []AuthoringFormValue `json:"values,omitempty"`
}

type AuthoringFormOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

type AuthoringFormCondition struct {
	Path     string   `json:"path"`
	Operator string   `json:"operator"`
	Values   []string `json:"values"`
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

// IntentGenerator is the preferred probabilistic boundary. It returns only a
// semantic answer sheet; OpenSeal deterministically compiles the canonical
// resource and runtime contracts. Generator remains temporarily accepted for
// deterministic fixtures while embedders move to this safer boundary.
type IntentGenerator interface {
	GenerateIntent(context.Context, GenerateRequest) (AuthoringIntent, error)
}

type IntentRepairGenerator interface {
	RepairIntent(context.Context, GenerateRequest, AuthoringIntent, error) (AuthoringIntent, error)
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

const AuthoringResultSchemaVersion = "openseal.authoring-result/v1"

// AuthoringResult is the canonical model-produced proposal envelope. OpenSeal
// generates the provider-facing JSON Schema from this type and validates the
// raw provider document against that schema before strict Go decoding.
type GenerationResponse struct {
	SchemaVersion       string                  `json:"schemaVersion" jsonschema:"Version of the OpenSeal authoring-result contract used by this proposal."`
	Candidate           WorkforceCandidate      `json:"candidate"`
	Authoring           AuthoringFormSubmission `json:"authoring"`
	Commitments         PromptCommitments       `json:"commitments"`
	Assumptions         []string                `json:"assumptions,omitempty"`
	UnresolvedQuestions []RefinementQuestion    `json:"unresolvedQuestions,omitempty"`
}

// AuthoringResult is the public name for the canonical result. The legacy Go
// name remains an alias so existing embedders do not need a parallel shape.
type AuthoringResult = GenerationResponse

func (r GenerationResponse) MarshalJSON() ([]byte, error) {
	type wire GenerationResponse
	if r.SchemaVersion == "" {
		r.SchemaVersion = AuthoringResultSchemaVersion
	}
	return json.Marshal(wire(r))
}

// PromptCommitments is the generator's typed, reviewable account of concrete
// facts it claims to have preserved from the user's prompt. The Compiler
// validates these facts against the candidate and independently extracts only
// a deliberately small grammar of unambiguous count, placement, inactivity,
// and approval clauses. Open-ended semantic equivalence remains outside this
// deterministic contract.
type PromptCommitments struct {
	AgentCount           *int                        `json:"agentCount,omitempty"`
	TeamCount            *int                        `json:"teamCount,omitempty"`
	ObjectiveCounts      []ObjectiveCountCommitment  `json:"objectiveCounts,omitempty"`
	Activation           ActivationCommitment        `json:"activation,omitempty"`
	ApprovalRequirements []ApprovalCommitment        `json:"approvalRequirements,omitempty"`
	ApprovalTimeouts     []ApprovalTimeoutCommitment `json:"approvalTimeouts,omitempty"`
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

type ApprovalTimeoutCommitment struct {
	OwnerType    CommitmentOwnerType `json:"ownerType"`
	OwnerID      string              `json:"ownerId,omitempty"`
	AfterSeconds int64               `json:"afterSeconds"`
	Decision     string              `json:"decision"`
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
