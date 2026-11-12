package activity

// ApprovalReviewContext is the operator-facing explanation of an action's
// effect. The model supplies the semantic facts it already knows; the kernel
// validates, bounds, persists, and presents them without asking delivery
// adapters to infer meaning from execution arguments.
type ApprovalReviewContext struct {
	Summary      string               `json:"summary"`
	Target       string               `json:"target,omitempty"`
	Audience     string               `json:"audience,omitempty"`
	Content      string               `json:"content,omitempty"`
	Purpose      string               `json:"purpose,omitempty"`
	Consequences []string             `json:"consequences,omitempty"`
	Facts        []ApprovalReviewFact `json:"facts,omitempty"`
}

type ApprovalReviewFact struct {
	Label string `json:"label"`
	Value string `json:"value"`
}
