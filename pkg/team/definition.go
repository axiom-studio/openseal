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

type RoleSlot struct {
	ID                    string                   `json:"id"`
	DisplayName           string                   `json:"displayName"`
	Purpose               string                   `json:"purpose"`
	MinimumMembers        int                      `json:"minimumMembers,omitempty"`
	MaximumMembers        int                      `json:"maximumMembers,omitempty"`
	RequiredSkillIDs      []string                 `json:"requiredSkillIds,omitempty"`
	RequiredDefinitionIDs []string                 `json:"requiredDefinitionIds,omitempty"`
	ChannelParticipation  RoleChannelParticipation `json:"channelParticipation,omitempty"`
}

type RoleChannelParticipation string

const (
	RoleChannelActive      RoleChannelParticipation = "active"
	RoleChannelObserveOnly RoleChannelParticipation = "observe_only"
	RoleChannelDisabled    RoleChannelParticipation = "disabled"
)

type CoordinationMode string

const (
	CoordinationDynamic           CoordinationMode = "dynamic"
	CoordinationPeer              CoordinationMode = "peer"
	CoordinationLeaderFacilitated CoordinationMode = "leader_facilitated"
)

type CoordinationPolicy struct {
	Mode                     CoordinationMode `json:"mode"`
	MaximumSpeakersPerRound  int              `json:"maximumSpeakersPerRound,omitempty"`
	QuietByDefault           bool             `json:"quietByDefault,omitempty"`
	RequireRoleRelevance     bool             `json:"requireRoleRelevance,omitempty"`
	SuppressDuplicateContent bool             `json:"suppressDuplicateContent,omitempty"`
}

type DelegationPolicy struct {
	MaximumDepth            int  `json:"maximumDepth,omitempty"`
	MaximumConcurrent       int  `json:"maximumConcurrent,omitempty"`
	AllowPeerDelegation     bool `json:"allowPeerDelegation,omitempty"`
	RequireAcceptance       bool `json:"requireAcceptance,omitempty"`
	RequireCompletionReview bool `json:"requireCompletionReview,omitempty"`
}

type ApprovalPolicy struct {
	MaximumRisk        capability.RiskLevel `json:"maximumRisk"`
	ApproverRoleIDs    []string             `json:"approverRoleIds,omitempty"`
	ApproverPrincipals []string             `json:"approverPrincipals,omitempty"`
}

// Definition is immutable Team behavior. Roster assignments and active
// versions belong to Deployment so the same Team design remains portable.
type Definition struct {
	ID                  string                          `json:"id"`
	Version             string                          `json:"version"`
	DisplayName         string                          `json:"displayName"`
	Purpose             string                          `json:"purpose"`
	OperatingPrinciples []string                        `json:"operatingPrinciples,omitempty"`
	Roles               []RoleSlot                      `json:"roles"`
	Coordination        CoordinationPolicy              `json:"coordination"`
	Delegation          DelegationPolicy                `json:"delegation,omitempty"`
	SharedContext       workforce.SharedContextPolicy   `json:"sharedContext,omitempty"`
	Approvals           ApprovalPolicy                  `json:"approvals"`
	ObjectiveTemplates  []workforce.ObjectiveTemplate   `json:"objectiveTemplates,omitempty"`
	Evaluations         []workforce.EvaluationCriterion `json:"evaluations,omitempty"`
	Amendments          workforce.AmendmentPolicy       `json:"amendments,omitempty"`
	Provenance          workforce.DefinitionProvenance  `json:"provenance,omitempty"`
	Digest              string                          `json:"digest"`
	CreatedAt           time.Time                       `json:"createdAt"`
}

func (d *Definition) Validate() error {
	if d == nil {
		return errors.New("team definition is required")
	}
	if strings.TrimSpace(d.ID) == "" || !versionPattern.MatchString(strings.TrimSpace(d.Version)) ||
		strings.TrimSpace(d.DisplayName) == "" || strings.TrimSpace(d.Purpose) == "" || len(d.Roles) == 0 {
		return errors.New("team definition id, valid version, display name, purpose, and roles are required")
	}
	if !validCoordinationMode(d.Coordination.Mode) || d.Coordination.MaximumSpeakersPerRound < 0 ||
		d.Delegation.MaximumDepth < 0 || d.Delegation.MaximumConcurrent < 0 ||
		d.SharedContext.Retention < 0 || d.SharedContext.MaximumBytes < 0 || !validRisk(d.Approvals.MaximumRisk) {
		return errors.New("team definition policies are invalid")
	}
	if d.Amendments.RequiresApproval && len(d.Amendments.ApproverPrincipals) == 0 {
		return errors.New("team definition amendment approval requires eligible principals")
	}
	roles := make(map[string]bool, len(d.Roles))
	for _, role := range d.Roles {
		id := strings.TrimSpace(role.ID)
		if id == "" || strings.TrimSpace(role.DisplayName) == "" || strings.TrimSpace(role.Purpose) == "" ||
			role.MinimumMembers < 0 || role.MaximumMembers < 0 || role.MaximumMembers > 0 && role.MaximumMembers < role.MinimumMembers ||
			!validRoleChannelParticipation(role.ChannelParticipation) || roles[id] {
			return errors.New("team roles require unique ids, names, purposes, and valid member bounds")
		}
		roles[id] = true
	}
	for _, roleID := range d.Approvals.ApproverRoleIDs {
		if !roles[strings.TrimSpace(roleID)] {
			return errors.New("team approval roles must reference declared roles")
		}
	}
	return nil
}

func validRoleChannelParticipation(value RoleChannelParticipation) bool {
	switch value {
	case "", RoleChannelActive, RoleChannelObserveOnly, RoleChannelDisabled:
		return true
	default:
		return false
	}
}

func validCoordinationMode(mode CoordinationMode) bool {
	switch mode {
	case CoordinationDynamic, CoordinationPeer, CoordinationLeaderFacilitated:
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
