package agent

import (
	"errors"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type AmendmentStatus string

const (
	AmendmentEvaluating       AmendmentStatus = "evaluating"
	AmendmentAwaitingApproval AmendmentStatus = "awaiting_approval"
	AmendmentReady            AmendmentStatus = "ready"
	AmendmentApproved         AmendmentStatus = "approved"
	AmendmentRejected         AmendmentStatus = "rejected"
	AmendmentEvaluationFailed AmendmentStatus = "evaluation_failed"
	AmendmentActivated        AmendmentStatus = "activated"
)

type DefinitionFieldChange struct {
	Field        string `json:"field"`
	BeforeDigest string `json:"beforeDigest,omitempty"`
	AfterDigest  string `json:"afterDigest,omitempty"`
}

type AmendmentEvaluation struct {
	CriterionID  string   `json:"criterionId"`
	Passed       bool     `json:"passed"`
	Score        float64  `json:"score,omitempty"`
	Summary      string   `json:"summary"`
	EvidenceRefs []string `json:"evidenceRefs,omitempty"`
}

type AmendmentDecision struct {
	Approved  bool      `json:"approved"`
	ActorType string    `json:"actorType"`
	ActorID   string    `json:"actorId"`
	Reason    string    `json:"reason,omitempty"`
	DecidedAt time.Time `json:"decidedAt"`
}

// DefinitionAmendment is an auditable proposal, never a mutable live prompt.
// It stores concise rationale and evidence, not private chain-of-thought.
type DefinitionAmendment struct {
	ID           string                    `json:"id"`
	Scope        capability.ScopeReference `json:"scope"`
	DeploymentID string                    `json:"deploymentId"`
	DefinitionID string                    `json:"definitionId"`
	BaseVersion  string                    `json:"baseVersion"`
	BaseDigest   string                    `json:"baseDigest"`
	Candidate    AgentDefinition           `json:"candidate"`
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
	Candidate    *AgentDefinition
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
	if a == nil || strings.TrimSpace(a.ID) == "" || strings.TrimSpace(a.Scope.Kind) == "" || strings.TrimSpace(a.Scope.ID) == "" || strings.TrimSpace(a.DeploymentID) == "" || strings.TrimSpace(a.DefinitionID) == "" || strings.TrimSpace(a.BaseVersion) == "" || strings.TrimSpace(a.BaseDigest) == "" || strings.TrimSpace(a.ProposerType) == "" || strings.TrimSpace(a.ProposerID) == "" || strings.TrimSpace(a.Rationale) == "" || a.Revision < 1 {
		return errors.New("amendment identity, scope, base, proposer, rationale, and revision are required")
	}
	if len(a.Changes) == 0 {
		return errors.New("amendment must change behavior")
	}
	if err := a.Candidate.Validate(); err != nil {
		return err
	}
	return nil
}
