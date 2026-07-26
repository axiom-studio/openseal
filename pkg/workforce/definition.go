// Package workforce contains definition primitives shared by first-class
// Agents and Teams. It deliberately contains no runtime placement, tenant
// policy, credentials, or provider-specific behavior.
package workforce

import (
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type ObjectiveTemplate struct {
	ID              string                 `json:"id"`
	Title           string                 `json:"title"`
	Goal            string                 `json:"goal"`
	Priority        int                    `json:"priority,omitempty"`
	Cadence         map[string]interface{} `json:"cadence,omitempty"`
	EventRules      map[string]interface{} `json:"eventRules,omitempty"`
	SuccessCriteria map[string]interface{} `json:"successCriteria,omitempty"`
	Constraints     map[string]interface{} `json:"constraints,omitempty"`
}

type EvaluationCriterion struct {
	ID          string  `json:"id"`
	Description string  `json:"description"`
	Weight      float64 `json:"weight,omitempty"`
	Required    bool    `json:"required,omitempty"`
}

type AmendmentPolicy struct {
	AgentMayPropose    bool     `json:"agentMayPropose,omitempty"`
	AllowedFields      []string `json:"allowedFields,omitempty"`
	RequiresApproval   bool     `json:"requiresApproval,omitempty"`
	ApproverPrincipals []string `json:"approverPrincipals,omitempty"`
}

type DefinitionProvenance struct {
	Source      string `json:"source,omitempty"`
	Reference   string `json:"reference,omitempty"`
	CreatedBy   string `json:"createdBy,omitempty"`
	DerivedFrom string `json:"derivedFrom,omitempty"`
}

// ActivationContinuation points a non-executing deployment back to the exact
// reviewed Change Set that created it. Hosts use this opaque, durable reference
// to resume the aggregate activation workflow after navigation or restart;
// activating one resource imperatively would leave the rest of the workforce
// (including Skill bindings, objectives, and conversation endpoints) inactive.
type ActivationContinuation struct {
	ChangeSetID string `json:"changeSetId"`
}

type DeploymentChangeKind string

const (
	DeploymentChangeConfigurationUpdated DeploymentChangeKind = "configuration-updated"
)

type SharedContextPolicy struct {
	Retention        time.Duration `json:"retention,omitempty"`
	MaximumBytes     int64         `json:"maximumBytes,omitempty"`
	AllowMemberRead  bool          `json:"allowMemberRead,omitempty"`
	AllowMemberWrite bool          `json:"allowMemberWrite,omitempty"`
}

type DefinitionActivation struct {
	ID                 string                    `json:"id"`
	Scope              capability.ScopeReference `json:"scope"`
	DeploymentID       string                    `json:"deploymentId"`
	DefinitionID       string                    `json:"definitionId"`
	FromVersion        string                    `json:"fromVersion,omitempty"`
	ToVersion          string                    `json:"toVersion"`
	DeploymentRevision int64                     `json:"deploymentRevision"`
	ChangeKind         DeploymentChangeKind      `json:"changeKind,omitempty"`
	Reason             string                    `json:"reason,omitempty"`
	ActorType          string                    `json:"actorType"`
	ActorID            string                    `json:"actorId"`
	CreatedAt          time.Time                 `json:"createdAt"`
}
