// Package authoring compiles natural-language intent into reviewable,
// product-neutral workforce candidates. Compilation never activates state.
package authoring

import (
	"context"

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
	CredentialKinds []string             `json:"credentialKinds,omitempty"`
	MaximumRisk     capability.RiskLevel `json:"maximumRisk,omitempty"`
}

type CapabilityCatalog struct {
	Skills               map[string]SkillCapability `json:"skills,omitempty"`
	AvailableCredentials map[string]bool            `json:"availableCredentials,omitempty"`
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
}

type GenerateRequest struct {
	Mode     Mode                `json:"mode"`
	Prompt   string              `json:"prompt"`
	Existing *WorkforceCandidate `json:"existing,omitempty"`
	Catalog  CapabilityCatalog   `json:"catalog"`
}

// Generator is the only probabilistic boundary. Implementations may use an
// LLM, a test fixture, or another planner, but must return one strict JSON
// GenerationResponse. The Compiler independently verifies its output.
type Generator interface {
	Generate(context.Context, GenerateRequest) ([]byte, error)
}

type GenerationResponse struct {
	Candidate   WorkforceCandidate `json:"candidate"`
	Assumptions []string           `json:"assumptions,omitempty"`
	Questions   []string           `json:"questions,omitempty"`
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
	Assumptions         []string             `json:"assumptions,omitempty"`
	Questions           []string             `json:"questions,omitempty"`
	Validation          []ValidationIssue    `json:"validation,omitempty"`
	MissingRequirements []MissingRequirement `json:"missingRequirements,omitempty"`
	RiskChanges         []RiskChange         `json:"riskChanges,omitempty"`
	Diff                []FieldDiff          `json:"diff,omitempty"`
}
