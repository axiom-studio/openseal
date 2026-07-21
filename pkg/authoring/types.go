// Package authoring compiles natural-language intent into reviewable,
// product-neutral workforce candidates. Compilation never activates state.
package authoring

import (
	"context"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/team"
)

type Mode string

const (
	ModeCreate Mode = "create"
	ModeAmend  Mode = "amend"
)

type SkillCapability struct {
	ID              string               `json:"id"`
	Version         string               `json:"version,omitempty"`
	Name            string               `json:"name,omitempty"`
	Description     string               `json:"description,omitempty"`
	Actions         []string             `json:"actions,omitempty"`
	CredentialKinds []string             `json:"credentialKinds,omitempty"`
	Credentials     []SkillCredential    `json:"credentials,omitempty"`
	PromptAvailable bool                 `json:"promptAvailable,omitempty"`
	MaximumRisk     capability.RiskLevel `json:"maximumRisk,omitempty"`
	Readiness       SkillReadiness       `json:"readiness,omitempty"`
	Compatibility   []SkillCompatibility `json:"compatibility,omitempty"`
}

type SkillReadiness string

const (
	SkillReadinessReady             SkillReadiness = "ready"
	SkillReadinessNeedsBinding      SkillReadiness = "needs_binding"
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
	Name     string   `json:"name"`
	Kind     string   `json:"kind"`
	Actions  []string `json:"actions,omitempty"`
	Optional bool     `json:"optional,omitempty"`
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
	Skills               map[string]SkillCapability        `json:"skills,omitempty"`
	AvailableCredentials map[string]bool                   `json:"availableCredentials,omitempty"`
	SourcePolicies       map[string]SourcePolicyCapability `json:"sourcePolicies,omitempty"`
}

type Assignment struct {
	ID                string `json:"id"`
	RoleID            string `json:"roleId"`
	AgentDefinitionID string `json:"agentDefinitionId"`
	DisplayName       string `json:"displayName,omitempty"`
}

type WorkforceCandidate struct {
	Agents      []*agent.AgentDefinition `json:"agents"`
	Team        *team.Definition         `json:"team,omitempty"`
	Assignments []Assignment             `json:"assignments,omitempty"`
	Initiative  *InitiativeBlueprint     `json:"initiative,omitempty"`
	// Activation is the reviewed, digest-bound operating state that atomic
	// apply must materialize. The Compiler derives it from typed commitments;
	// apply never infers it from prompt prose.
	Activation WorkforceActivationIntent `json:"activation"`
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
	Mode          Mode                `json:"mode"`
	Prompt        string              `json:"prompt"`
	Existing      *WorkforceCandidate `json:"existing,omitempty"`
	Catalog       CapabilityCatalog   `json:"catalog"`
	Refinement    *RefinementContext  `json:"refinement,omitempty"`
	InvocationKey string              `json:"invocationKey,omitempty"`
}

// Generator is the only probabilistic boundary. Implementations may use an
// LLM, a test fixture, or another planner, but must return one strict JSON
// GenerationResponse. The Compiler independently verifies its output.
type Generator interface {
	Generate(context.Context, GenerateRequest) ([]byte, error)
}

type RepairGenerator interface {
	Repair(context.Context, GenerateRequest, []byte, error) ([]byte, error)
}

type GenerationResponse struct {
	Candidate           WorkforceCandidate   `json:"candidate"`
	Commitments         PromptCommitments    `json:"commitments"`
	Assumptions         []string             `json:"assumptions,omitempty"`
	Questions           []string             `json:"questions,omitempty"`
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
	Kind       string `json:"kind"`
	ID         string `json:"id"`
	RequiredBy string `json:"requiredBy"`
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
	Candidate           WorkforceCandidate   `json:"candidate"`
	Valid               bool                 `json:"valid"`
	Commitments         PromptCommitments    `json:"commitments"`
	Assumptions         []string             `json:"assumptions,omitempty"`
	Questions           []string             `json:"questions,omitempty"`
	UnresolvedQuestions []RefinementQuestion `json:"unresolvedQuestions,omitempty"`
	Validation          []ValidationIssue    `json:"validation,omitempty"`
	MissingRequirements []MissingRequirement `json:"missingRequirements,omitempty"`
	RiskChanges         []RiskChange         `json:"riskChanges,omitempty"`
	Diff                []FieldDiff          `json:"diff,omitempty"`
}
