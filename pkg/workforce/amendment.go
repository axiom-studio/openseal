package workforce

import "time"

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
