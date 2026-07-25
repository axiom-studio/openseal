package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// ExternalConversationWorkforceCatalog is the narrow durable workforce view
// needed to resolve a reviewed Runbook handler. SQLiteStore, PostgresStore,
// and embedding hosts may implement it without importing conversation logic
// into their Agent or Team registries.
type ExternalConversationWorkforceCatalog interface {
	GetDeployment(context.Context, capability.ScopeReference, string) (*agent.AgentDeployment, error)
	GetDefinition(context.Context, string, string) (*agent.AgentDefinition, error)
	GetTeamDeployment(context.Context, capability.ScopeReference, string) (*kernelteam.Deployment, error)
}

// CatalogExternalConversationRunbookResolver binds an endpoint's exact
// assigned Agent deployment to its currently active immutable definition.
// Team-owned endpoints additionally prove current roster membership.
type CatalogExternalConversationRunbookResolver struct {
	catalog ExternalConversationWorkforceCatalog
}

func NewCatalogExternalConversationRunbookResolver(
	catalog ExternalConversationWorkforceCatalog,
) (*CatalogExternalConversationRunbookResolver, error) {
	if catalog == nil {
		return nil, errors.New("external conversation workforce catalog is required")
	}
	return &CatalogExternalConversationRunbookResolver{catalog: catalog}, nil
}

func (r *CatalogExternalConversationRunbookResolver) ResolveExternalConversationRunbook(
	ctx context.Context,
	scope Scope,
	endpoint *ExternalConversationEndpoint,
	handler ExternalConversationHandler,
) (*ResolvedExternalConversationRunbook, error) {
	if r == nil || r.catalog == nil || endpoint == nil || endpoint.Scope != scope ||
		endpoint.Handler != handler || handler.Kind != ExternalConversationHandlerRunbook {
		return nil, fmt.Errorf("%w: exact Runbook endpoint binding is required", ErrInvalidExternalConversation)
	}
	if err := handler.Validate(endpoint.Owner); err != nil {
		return nil, err
	}
	catalogScope := capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	switch endpoint.Owner.Type {
	case OwnerTypeAgent:
		if handler.AssignedAgentID != endpoint.Owner.ID {
			return nil, fmt.Errorf("%w: Agent-owned Runbook must use its endpoint owner deployment", ErrInvalidExternalConversation)
		}
	case OwnerTypeTeam:
		deployment, err := r.catalog.GetTeamDeployment(ctx, catalogScope, endpoint.Owner.ID)
		if err != nil {
			return nil, fmt.Errorf("resolve conversation Team deployment: %w", err)
		}
		if deployment == nil || deployment.Status != kernelteam.DeploymentActive ||
			!teamRosterContainsAgent(deployment, handler.AssignedAgentID) {
			return nil, fmt.Errorf("%w: Runbook Agent is not active in the endpoint Team roster", ErrInvalidExternalConversation)
		}
	default:
		return nil, fmt.Errorf("%w: conversation endpoint owner is invalid", ErrInvalidExternalConversation)
	}
	deployment, err := r.catalog.GetDeployment(ctx, catalogScope, handler.AssignedAgentID)
	if err != nil {
		return nil, fmt.Errorf("resolve conversation Agent deployment: %w", err)
	}
	if deployment == nil || deployment.RolloutStatus != agent.RolloutActive ||
		deployment.ID != strings.TrimSpace(handler.AssignedAgentID) {
		return nil, fmt.Errorf("%w: Runbook Agent deployment is not active", ErrInvalidExternalConversation)
	}
	definition, err := r.catalog.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return nil, fmt.Errorf("resolve conversation Agent definition: %w", err)
	}
	if definition == nil || definition.Runbook == nil ||
		definition.Runbook.ID != handler.ID || definition.Runbook.Version != handler.Version {
		return nil, fmt.Errorf("%w: deployed Agent does not contain the exact Runbook", ErrExternalConversationConflict)
	}
	return &ResolvedExternalConversationRunbook{
		Definition: definition.Runbook, AssignedAgentID: deployment.ID,
	}, nil
}

func teamRosterContainsAgent(deployment *kernelteam.Deployment, agentDeploymentID string) bool {
	if deployment == nil {
		return false
	}
	agentDeploymentID = strings.TrimSpace(agentDeploymentID)
	for _, assignment := range deployment.Roster {
		if assignment.AgentDeploymentID == agentDeploymentID {
			return true
		}
	}
	return false
}

var _ ExternalConversationRunbookResolver = (*CatalogExternalConversationRunbookResolver)(nil)
