package daemon

import (
	"context"
	"errors"
	"fmt"
	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/team"
	"strings"
)

// DesktopWorkRequest constructs a local root task. Renderer-owned lifecycle
// state and policy must never be imported into the execution authority.
func DesktopWorkRequest(scope runtime.Scope, request runtime.CreateAgentRunRequest) (runtime.CreateAgentRunRequest, error) {
	validOwner := request.Owner.Type == runtime.OwnerTypeAgent && request.Owner.ID == request.AssignedAgentID || request.Owner.Type == runtime.OwnerTypeTeam && strings.TrimSpace(request.Owner.ID) != ""
	if scope.Kind != "local" || scope.ID == "" || request.Scope != scope || request.Kind != runtime.RunKindAgentWork || !validOwner || strings.TrimSpace(request.AssignedAgentID) == "" || strings.TrimSpace(request.Goal) == "" || strings.TrimSpace(request.IdempotencyKey) == "" {
		return runtime.CreateAgentRunRequest{}, errors.New("desktop work requires a local agent, goal, and idempotency key")
	}
	if request.ParentRunID != "" || request.ObjectiveID != "" || len(request.Context) > 0 || len(request.Checkpoint) > 0 || len(request.Plan) > 0 || len(request.Policy) > 0 || request.WakeCondition != nil || request.Budget != nil || len(request.ResourceRequirements) > 0 || request.ConcurrencyKey != "" || request.Entrypoint != "" {
		return runtime.CreateAgentRunRequest{}, errors.New("desktop work cannot supply host-owned execution state or policy")
	}
	return runtime.CreateAgentRunRequest{Scope: scope, Kind: runtime.RunKindAgentWork, Owner: request.Owner, AssignedAgentID: request.AssignedAgentID, Goal: strings.TrimSpace(request.Goal), Source: runtime.RunSource("manual"), IdempotencyKey: request.IdempotencyKey, Actor: runtime.ActivityActor{Type: "user", ID: "local-operator"}, Visibility: runtime.ActivityVisibility("scope"), Budget: &runtime.BudgetPolicy{MaxTurns: 16, MaxAttempts: 24, MaxTotalTokens: 128000, MaxOutputTokens: 32000, MaxActions: 16, MaxDurationMS: 15 * 60 * 1000}}, nil
}

type DesktopWorkCatalog interface {
	GetAgentDeployment(context.Context, capability.ScopeReference, string) (*agent.AgentDeployment, error)
	GetTeamDeployment(context.Context, capability.ScopeReference, string) (*team.Deployment, error)
}

// ResolveDesktopWork validates current host-owned membership before creating work.
// The canonical turn resolver rechecks team authority when work executes.
func ResolveDesktopWork(ctx context.Context, catalog DesktopWorkCatalog, scope runtime.Scope, request runtime.CreateAgentRunRequest) (runtime.CreateAgentRunRequest, error) {
	canonical, err := DesktopWorkRequest(scope, request)
	if err != nil {
		return canonical, err
	}
	ref := capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	ids := []string{canonical.AssignedAgentID}
	if canonical.Owner.Type == runtime.OwnerTypeTeam {
		deployment, err := catalog.GetTeamDeployment(ctx, ref, canonical.Owner.ID)
		if err != nil {
			return canonical, err
		}
		if deployment == nil || deployment.Status != team.DeploymentActive || deployment.Activation != nil {
			return canonical, errors.New("the team must be active before starting work")
		}
		ids = nil
		member := false
		for _, assignment := range deployment.Roster {
			ids = append(ids, assignment.AgentDeploymentID)
			member = member || assignment.AgentDeploymentID == canonical.AssignedAgentID
		}
		if !member {
			return canonical, errors.New("the lead agent must belong to this team")
		}
	}
	for _, id := range ids {
		deployment, err := catalog.GetAgentDeployment(ctx, ref, id)
		if err != nil {
			return canonical, err
		}
		if deployment == nil || deployment.RolloutStatus != agent.RolloutActive {
			return canonical, fmt.Errorf("agent %s must be active before starting work", id)
		}
	}
	return canonical, nil
}
