// Package team defines portable, versioned Team behavior and scoped Team
// deployments. Teams coordinate Agent deployments; they never own or copy the
// Agents' credentials.
package team

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

var versionPattern = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,127}$`)

// ChannelParticipation is whether a roster member answers in the Team channel.
type ChannelParticipation string

const (
	ChannelActive      ChannelParticipation = "active"
	ChannelObserveOnly ChannelParticipation = "observe_only"
	ChannelDisabled    ChannelParticipation = "disabled"
)

// RoutingPolicy is the lead's view of who answers what in the Team channel.
// A message nobody addressed goes to one member chosen by the host's router;
// Notes (free text, e.g. "Maya owns pricing, Otto owns press") feed that
// router and the members' agent cards.
type RoutingPolicy struct {
	Notes string `json:"notes,omitempty"`
}

// MaximumRoutingNotesBytes bounds the routing notes a router prompt carries.
const MaximumRoutingNotesBytes = 4000

type DelegationPolicy struct {
	MaximumDepth            int  `json:"maximumDepth,omitempty"`
	MaximumConcurrent       int  `json:"maximumConcurrent,omitempty"`
	AllowPeerDelegation     bool `json:"allowPeerDelegation,omitempty"`
	RequireAcceptance       bool `json:"requireAcceptance,omitempty"`
	RequireCompletionReview bool `json:"requireCompletionReview,omitempty"`
	CompletionReviewQuorum  int  `json:"completionReviewQuorum,omitempty"`
	EscalateOnDisagreement  bool `json:"escalateOnDisagreement,omitempty"`
}

type ApprovalPolicy struct {
	MaximumRisk        capability.RiskLevel `json:"maximumRisk"`
	ApproverPrincipals []string             `json:"approverPrincipals,omitempty"`
}

// Definition is immutable Team behavior. The members and the active version
// belong to Deployment so the same Team design remains portable.
type Definition struct {
	// AuthoringKey is a stable form reference separate from the resource identity.
	AuthoringKey        string                          `json:"authoringKey,omitempty"`
	ID                  string                          `json:"id"`
	Version             string                          `json:"version"`
	DisplayName         string                          `json:"displayName"`
	Purpose             string                          `json:"purpose"`
	OperatingPrinciples []string                        `json:"operatingPrinciples,omitempty"`
	Routing             RoutingPolicy                   `json:"routing,omitempty"`
	Delegation          DelegationPolicy                `json:"delegation,omitempty"`
	SharedContext       workforce.SharedContextPolicy   `json:"sharedContext,omitempty"`
	Approvals           ApprovalPolicy                  `json:"approvals"`
	ObjectiveTemplates  []workforce.ObjectiveTemplate   `json:"objectiveTemplates,omitempty"`
	Evaluations         []workforce.EvaluationCriterion `json:"evaluations,omitempty"`
	Amendments          workforce.AmendmentPolicy       `json:"amendments,omitempty"`
	Provenance          workforce.DefinitionProvenance  `json:"provenance,omitempty"`
	Digest              string                          `json:"digest,omitempty"`
	CreatedAt           time.Time                       `json:"createdAt,omitempty"`
}

func (d *Definition) Validate() error {
	if d == nil {
		return errors.New("team definition is required")
	}
	if strings.TrimSpace(d.ID) == "" || !versionPattern.MatchString(strings.TrimSpace(d.Version)) ||
		strings.TrimSpace(d.DisplayName) == "" || strings.TrimSpace(d.Purpose) == "" {
		return errors.New("team definition id, valid version, display name, and purpose are required")
	}
	if len(d.Routing.Notes) > MaximumRoutingNotesBytes ||
		d.Delegation.MaximumDepth < 0 || d.Delegation.MaximumConcurrent < 0 || d.Delegation.CompletionReviewQuorum < 0 ||
		d.SharedContext.Retention < 0 || d.SharedContext.MaximumBytes < 0 || !validRisk(d.Approvals.MaximumRisk) {
		return errors.New("team definition policies are invalid")
	}
	if !d.Delegation.RequireCompletionReview && (d.Delegation.CompletionReviewQuorum > 0 || d.Delegation.EscalateOnDisagreement) {
		return errors.New("Team completion review quorum and disagreement escalation require completion review")
	}
	if d.Amendments.RequiresApproval && len(d.Amendments.ApproverPrincipals) == 0 {
		return errors.New("team definition amendment approval requires eligible principals")
	}
	return nil
}

func validChannelParticipation(value ChannelParticipation) bool {
	switch value {
	case "", ChannelActive, ChannelObserveOnly, ChannelDisabled:
		return true
	default:
		return false
	}
}

func validRisk(risk capability.RiskLevel) bool {
	switch risk {
	case capability.RiskLevelRead, capability.RiskLevelWrite, capability.RiskLevelExternal,
		capability.RiskLevelProduction, capability.RiskLevelDestructive:
		return true
	default:
		return false
	}
}
