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

type RosterAssignment struct {
	ID                string `json:"id"`
	RoleID            string `json:"roleId"`
	AgentDeploymentID string `json:"agentDeploymentId"`
	DisplayName       string `json:"displayName,omitempty"`
}

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
	roles := make(map[string]RoleSlot, len(definition.Roles))
	counts := make(map[string]int, len(definition.Roles))
	for _, role := range definition.Roles {
		roles[role.ID] = role
	}
	assignments := make(map[string]bool, len(d.Roster))
	agents := make(map[string]bool, len(d.Roster))
	for _, assignment := range d.Roster {
		if strings.TrimSpace(assignment.ID) == "" || strings.TrimSpace(assignment.AgentDeploymentID) == "" ||
			roles[assignment.RoleID].ID == "" || assignments[assignment.ID] || agents[assignment.AgentDeploymentID] {
			return errors.New("team roster assignments require unique ids, agents, and declared roles")
		}
		assignments[assignment.ID] = true
		agents[assignment.AgentDeploymentID] = true
		counts[assignment.RoleID]++
	}
	for id, role := range roles {
		if counts[id] < role.MinimumMembers || role.MaximumMembers > 0 && counts[id] > role.MaximumMembers {
			return errors.New("team roster does not satisfy role member bounds")
		}
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
