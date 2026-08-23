package agent

import (
	"errors"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

type RolloutStatus string

const (
	ModelProviderCredentialBinding = "MODEL_PROVIDER"

	RolloutPending  RolloutStatus = "pending"
	RolloutActive   RolloutStatus = "active"
	RolloutDegraded RolloutStatus = "degraded"
	RolloutPaused   RolloutStatus = "paused"
	RolloutRetired  RolloutStatus = "retired"
)

type DeploymentRestrictions struct {
	MaximumRisk       *capability.RiskLevel `json:"maximumRisk,omitempty"`
	AllowedSkillIDs   []string              `json:"allowedSkillIds,omitempty"`
	MaxConcurrentRuns *int                  `json:"maxConcurrentRuns,omitempty"`
	BudgetCeilings    map[string]float64    `json:"budgetCeilings,omitempty"`
}

type DeploymentCapacity struct {
	MaxConcurrentRuns int `json:"maxConcurrentRuns"`
	MaxQueuedRuns     int `json:"maxQueuedRuns,omitempty"`
}

type DeploymentHealth struct {
	Status          string     `json:"status,omitempty"`
	Message         string     `json:"message,omitempty"`
	LastHeartbeatAt *time.Time `json:"lastHeartbeatAt,omitempty"`
}

type AgentDeployment struct {
	ID                 string                                    `json:"id"`
	DisplayName        string                                    `json:"displayName,omitempty"`
	Scope              capability.ScopeReference                 `json:"scope"`
	DefinitionID       string                                    `json:"definitionId"`
	ActiveVersion      string                                    `json:"activeVersion"`
	PreviousVersion    string                                    `json:"previousVersion,omitempty"`
	RolloutStatus      RolloutStatus                             `json:"rolloutStatus"`
	Environment        string                                    `json:"environment"`
	Placement          map[string]string                         `json:"placement,omitempty"`
	SkillBindingIDs    []string                                  `json:"skillBindingIds,omitempty"`
	Credentials        map[string]capability.CredentialReference `json:"credentials,omitempty"`
	DefaultWorkspaceID string                                    `json:"defaultWorkspaceId,omitempty"`
	Workspaces         []workspace.Spec                          `json:"workspaces,omitempty"`
	Restrictions       DeploymentRestrictions                    `json:"restrictions,omitempty"`
	Capacity           DeploymentCapacity                        `json:"capacity"`
	Health             DeploymentHealth                          `json:"health,omitempty"`
	Activation         *workforce.ActivationContinuation         `json:"activation,omitempty"`
	Revision           int64                                     `json:"revision"`
	CreatedAt          time.Time                                 `json:"createdAt"`
	UpdatedAt          time.Time                                 `json:"updatedAt"`
}

type DefinitionActivation = workforce.DefinitionActivation

func (d *AgentDeployment) Validate() error {
	if d == nil || strings.TrimSpace(d.ID) == "" || strings.TrimSpace(d.Scope.Kind) == "" || strings.TrimSpace(d.Scope.ID) == "" || strings.TrimSpace(d.DefinitionID) == "" || !versionPattern.MatchString(d.ActiveVersion) || strings.TrimSpace(d.Environment) == "" {
		return errors.New("deployment id, scope, definition, active version, and environment are required")
	}
	if d.Capacity.MaxConcurrentRuns < 1 || d.Capacity.MaxQueuedRuns < 0 || d.Revision < 1 {
		return errors.New("deployment capacity and revision are invalid")
	}
	if len(strings.TrimSpace(d.DisplayName)) > 200 {
		return errors.New("deployment display name must not exceed 200 characters")
	}
	if d.RolloutStatus != RolloutPending && d.RolloutStatus != RolloutActive && d.RolloutStatus != RolloutDegraded && d.RolloutStatus != RolloutPaused && d.RolloutStatus != RolloutRetired {
		return errors.New("deployment rollout status is invalid")
	}
	if d.Activation != nil {
		if (d.RolloutStatus != RolloutPending && d.RolloutStatus != RolloutPaused) || strings.TrimSpace(d.Activation.ChangeSetID) == "" {
			return errors.New("deployment activation continuation requires a pending or paused deployment and Change Set")
		}
	}
	for name, reference := range d.Credentials {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(reference.Kind) == "" || strings.TrimSpace(reference.ID) == "" {
			return errors.New("deployment credentials must be opaque named references")
		}
	}
	// Older portable deployments did not carry Workspace desired state. Hosts
	// normalize those deployments during creation; validation remains backward
	// compatible for stored manifests while rejecting partial Workspace state.
	if strings.TrimSpace(d.DefaultWorkspaceID) != "" || len(d.Workspaces) != 0 {
		if strings.TrimSpace(d.DefaultWorkspaceID) == "" || len(d.Workspaces) == 0 {
			return errors.New("deployment workspace state is incomplete")
		}
		workspaceIDs := make(map[string]struct{}, len(d.Workspaces))
		for _, candidate := range d.Workspaces {
			if err := candidate.Validate(); err != nil {
				return err
			}
			if _, duplicate := workspaceIDs[candidate.ID]; duplicate {
				return errors.New("deployment workspace ids must be unique")
			}
			workspaceIDs[candidate.ID] = struct{}{}
		}
		if _, found := workspaceIDs[d.DefaultWorkspaceID]; !found {
			return errors.New("deployment default workspace is not attached")
		}
	}
	return validateDeploymentPlacement(d)
}

func validateDeploymentPlacement(d *AgentDeployment) error {
	placement := make(map[string]interface{}, len(d.Placement))
	for key, value := range d.Placement {
		placement[key] = value
	}
	if err := validateNoSecrets(placement, "placement"); err != nil {
		return err
	}
	return nil
}

// EnsureDefaultWorkspace materializes portable defaults for a deployment that
// predates first-class Workspaces. It does not replace explicitly configured
// Workspace state.
func EnsureDefaultWorkspace(d *AgentDeployment) {
	if d == nil || len(d.Workspaces) != 0 || strings.TrimSpace(d.DefaultWorkspaceID) != "" {
		return
	}
	d.DefaultWorkspaceID = workspace.DefaultID
	d.Workspaces = []workspace.Spec{workspace.DefaultSpec()}
}

func validateNarrowing(definition *AgentDefinition, deployment *AgentDeployment) error {
	if deployment.Restrictions.MaximumRisk != nil && riskRank(*deployment.Restrictions.MaximumRisk) > riskRank(definition.Authority.MaximumRisk) {
		return errors.New("deployment maximum risk cannot widen definition authority")
	}
	if deployment.Restrictions.MaxConcurrentRuns != nil && *deployment.Restrictions.MaxConcurrentRuns > definition.Authority.MaxConcurrentRuns {
		return errors.New("deployment concurrency cannot widen definition authority")
	}
	if deployment.Capacity.MaxConcurrentRuns > definition.Authority.MaxConcurrentRuns {
		return errors.New("deployment capacity cannot exceed definition authority")
	}
	if deployment.Restrictions.AllowedSkillIDs != nil && !isSubset(deployment.Restrictions.AllowedSkillIDs, definition.Authority.AllowedSkillIDs) {
		return errors.New("deployment skills cannot widen definition authority")
	}
	for name, ceiling := range deployment.Restrictions.BudgetCeilings {
		defined, ok := definition.Authority.BudgetCeilings[name]
		if !ok || ceiling < 0 || ceiling > defined {
			return errors.New("deployment budgets cannot widen definition authority")
		}
	}
	return nil
}
