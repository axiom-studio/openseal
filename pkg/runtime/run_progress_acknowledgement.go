package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
)

// RunProgressSnapshot contains only verified, operator-visible progress facts.
// It deliberately excludes action arguments, provider payloads, credentials,
// hidden reasoning, and checkpoint internals.
type RunProgressSnapshot struct {
	RunID           string                `json:"runId"`
	Scope           models.Scope          `json:"scope"`
	AgentID         string                `json:"agentId"`
	Goal            string                `json:"goal"`
	Status          models.AgentRunStatus `json:"status"`
	Revision        int64                 `json:"revision"`
	ActivityID      string                `json:"activityId,omitempty"`
	ActivityType    string                `json:"activityType,omitempty"`
	ActivitySummary string                `json:"activitySummary,omitempty"`
}

type RunProgressAcknowledgementRequest struct {
	Snapshot      RunProgressSnapshot `json:"snapshot"`
	MaxSentences  int                 `json:"maxSentences"`
	MaxCharacters int                 `json:"maxCharacters"`
}

// RunProgressAcknowledgementRenderer owns wording. OpenSeal supplies facts and
// validates the result; embedding hosts may use the Agent's configured model
// and personality without teaching the kernel provider- or Skill-specific
// phrases.
type RunProgressAcknowledgementRenderer interface {
	RenderRunProgressAcknowledgement(context.Context, RunProgressAcknowledgementRequest) (string, error)
}
