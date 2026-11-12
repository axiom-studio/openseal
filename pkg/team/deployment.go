package team

import (
	"errors"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type DeploymentStatus string

const (
	DeploymentDraft    DeploymentStatus = "draft"
	DeploymentActive   DeploymentStatus = "active"
	DeploymentPaused   DeploymentStatus = "paused"
	DeploymentArchived DeploymentStatus = "archived"
)

// RosterAssignment is one member of a Team: an Agent deployment, what it does
// on the Team and whether it answers in the Team channel.
type RosterAssignment struct {
	ID                   string               `json:"id"`
	AgentDeploymentID    string               `json:"agentDeploymentId"`
	DisplayName          string               `json:"displayName,omitempty"`
	Purpose              string               `json:"purpose,omitempty"`
	ChannelParticipation ChannelParticipation `json:"channelParticipation,omitempty"`
	// RequiredSkillIDs are Skills the member's Agent definition must declare
	// for the member to stay on the roster and take delegated work.
	RequiredSkillIDs []string `json:"requiredSkillIds,omitempty"`
	// Approver marks a member who may review other members' work. When no
	// member is marked, every member may.
	Approver bool `json:"approver,omitempty"`
	// Lead marks the member a message goes to when the router picks nobody.
	Lead bool `json:"lead,omitempty"`
	// Owns lists the topics this member answers in the Team channel ("press
	// list", "pricing"). The router prefers a topic's owner.
	Owns []string `json:"owns,omitempty"`
}

// MaximumOwnedTopics bounds one member's owned topics.
const MaximumOwnedTopics = 16

type DeploymentRestrictions struct {
	AllowedSkillIDs    []string             `json:"allowedSkillIds,omitempty"`
	MaximumRisk        capability.RiskLevel `json:"maximumRisk,omitempty"`
	MaximumConcurrency int                  `json:"maximumConcurrency,omitempty"`
}

type Deployment struct {
	ID            string                    `json:"id"`
	Scope         capability.ScopeReference `json:"scope"`
	DefinitionID  string                    `json:"definitionId"`
	ActiveVersion string                    `json:"activeVersion"`
	Roster        []RosterAssignment        `json:"roster"`
	Restrictions  DeploymentRestrictions    `json:"restrictions,omitempty"`
	Status        DeploymentStatus          `json:"status"`
	Revision      int64                     `json:"revision"`
	CreatedAt     time.Time                 `json:"createdAt"`
	UpdatedAt     time.Time                 `json:"updatedAt"`
}

func (d *Deployment) Validate(definition *Definition) error {
	if d == nil || definition == nil {
		return errors.New("team deployment and definition are required")
	}
	if strings.TrimSpace(d.ID) == "" || strings.TrimSpace(d.Scope.Kind) == "" || strings.TrimSpace(d.Scope.ID) == "" ||
		d.DefinitionID != definition.ID || d.ActiveVersion != definition.Version || d.Revision < 1 || !validDeploymentStatus(d.Status) {
		return errors.New("team deployment identity, scope, active definition, status, and revision are required")
	}
	// An archived Team keeps whatever roster it had.
	if len(d.Roster) == 0 && d.Status != DeploymentArchived {
		return ErrRosterMinimum
	}
	assignments := make(map[string]bool, len(d.Roster))
	agents := make(map[string]bool, len(d.Roster))
	for _, assignment := range d.Roster {
		if strings.TrimSpace(assignment.ID) == "" || strings.TrimSpace(assignment.AgentDeploymentID) == "" ||
			!validChannelParticipation(assignment.ChannelParticipation) || assignments[assignment.ID] || agents[assignment.AgentDeploymentID] {
			return errors.New("team roster members require unique ids and agents, and a valid channel participation")
		}
		if len(assignment.Owns) > MaximumOwnedTopics {
			return errors.New("team roster member owns too many topics")
		}
		for _, topic := range assignment.Owns {
			if strings.TrimSpace(topic) == "" || topic != strings.TrimSpace(topic) || len(topic) > 80 {
				return errors.New("team roster owned topics must be 1-80 trimmed characters")
			}
		}
		assignments[assignment.ID] = true
		agents[assignment.AgentDeploymentID] = true
	}
	if d.Restrictions.MaximumConcurrency < 0 || d.Restrictions.MaximumRisk != "" && !validRisk(d.Restrictions.MaximumRisk) {
		return errors.New("team deployment restrictions are invalid")
	}
	return nil
}

func validDeploymentStatus(status DeploymentStatus) bool {
	switch status {
	case DeploymentDraft, DeploymentActive, DeploymentPaused, DeploymentArchived:
		return true
	default:
		return false
	}
}
