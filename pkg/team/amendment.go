package team

import (
	"errors"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

type AmendmentStatus = workforce.AmendmentStatus
type DefinitionFieldChange = workforce.DefinitionFieldChange
type AmendmentEvaluation = workforce.AmendmentEvaluation
type AmendmentDecision = workforce.AmendmentDecision

const (
	AmendmentEvaluating       = workforce.AmendmentEvaluating
	AmendmentAwaitingApproval = workforce.AmendmentAwaitingApproval
	AmendmentReady            = workforce.AmendmentReady
	AmendmentApproved         = workforce.AmendmentApproved
	AmendmentRejected         = workforce.AmendmentRejected
	AmendmentEvaluationFailed = workforce.AmendmentEvaluationFailed
	AmendmentActivated        = workforce.AmendmentActivated
)

// DefinitionAmendment is a durable, auditable Team behavior proposal. It
// stores concise rationale and evidence references, never private reasoning.
type DefinitionAmendment struct {
	ID           string                    `json:"id"`
	Scope        capability.ScopeReference `json:"scope"`
	DeploymentID string                    `json:"deploymentId"`
	DefinitionID string                    `json:"definitionId"`
	BaseVersion  string                    `json:"baseVersion"`
	BaseDigest   string                    `json:"baseDigest"`
	Candidate    Definition                `json:"candidate"`
	Changes      []DefinitionFieldChange   `json:"changes"`
	RiskWidening bool                      `json:"riskWidening,omitempty"`
	ProposerType string                    `json:"proposerType"`
	ProposerID   string                    `json:"proposerId"`
	Rationale    string                    `json:"rationale"`
	EvidenceRefs []string                  `json:"evidenceRefs,omitempty"`
	Status       AmendmentStatus           `json:"status"`
	Evaluations  []AmendmentEvaluation     `json:"evaluations,omitempty"`
	Decision     *AmendmentDecision        `json:"decision,omitempty"`
	ActivationID string                    `json:"activationId,omitempty"`
	Revision     int64                     `json:"revision"`
	CreatedAt    time.Time                 `json:"createdAt"`
	UpdatedAt    time.Time                 `json:"updatedAt"`
}

type ProposeAmendmentRequest struct {
	Scope        capability.ScopeReference
	DeploymentID string
	Candidate    *Definition
	ProposerType string
	ProposerID   string
	Rationale    string
	EvidenceRefs []string
}

type SubmitAmendmentEvaluationRequest struct {
	Scope            capability.ScopeReference
	AmendmentID      string
	ExpectedRevision int64
	Evaluations      []AmendmentEvaluation
}

type ResolveAmendmentRequest struct {
	Scope            capability.ScopeReference
	AmendmentID      string
	ExpectedRevision int64
	Approved         bool
	ActorType        string
	ActorID          string
	Reason           string
}

func (a *DefinitionAmendment) Validate() error {
	if a == nil || strings.TrimSpace(a.ID) == "" || strings.TrimSpace(a.Scope.Kind) == "" || strings.TrimSpace(a.Scope.ID) == "" ||
		strings.TrimSpace(a.DeploymentID) == "" || strings.TrimSpace(a.DefinitionID) == "" || strings.TrimSpace(a.BaseVersion) == "" ||
		strings.TrimSpace(a.BaseDigest) == "" || strings.TrimSpace(a.ProposerType) == "" || strings.TrimSpace(a.ProposerID) == "" ||
		strings.TrimSpace(a.Rationale) == "" || a.Revision < 1 {
		return errors.New("team amendment identity, scope, base, proposer, rationale, and revision are required")
	}
	if len(a.Changes) == 0 {
		return errors.New("team amendment must change behavior")
	}
	return a.Candidate.Validate()
}
