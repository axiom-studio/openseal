// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/runtime/portfolio. Remove once in-flight branches have landed.
//
// Re-exports of declarations moved to pkg/runtime/portfolio, kept so that
// importers of pkg/runtime continue to compile.

package runtime

import (
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

type ResolveRunDependencyRequest = portfolio.ResolveRunDependencyRequest

type DependencyCoordinator = portfolio.DependencyCoordinator

// NewDependencyCoordinator forwards to portfolio.NewDependencyCoordinator.
func NewDependencyCoordinator(store models.DependencyKernelStore, opts ...models.ServiceOption) *DependencyCoordinator {
	return portfolio.NewDependencyCoordinator(store, opts...)
}

type ReconcileRunDependencyGroupRequest = portfolio.ReconcileRunDependencyGroupRequest

var ErrInvalidObjectiveTransition = portfolio.ErrInvalidObjectiveTransition

var ErrInvalidRunCommand = portfolio.ErrInvalidRunCommand

type CreateObjectiveRequest = portfolio.CreateObjectiveRequest

type UpdateObjectiveRequest = portfolio.UpdateObjectiveRequest

type PortfolioService = portfolio.PortfolioService

type CreateObjectiveResult = portfolio.CreateObjectiveResult

// NewPortfolioService forwards to portfolio.NewPortfolioService.
func NewPortfolioService(store models.PortfolioStore, opts ...models.ServiceOption) *PortfolioService {
	return portfolio.NewPortfolioService(store, opts...)
}

type CreateSpaceRequest = portfolio.CreateSpaceRequest

type SetSpaceAccessStateRequest = portfolio.SetSpaceAccessStateRequest

type AddSpacePersonRequest = portfolio.AddSpacePersonRequest

type UpdateSpaceRequest = portfolio.UpdateSpaceRequest

type PutSpaceMemberRequest = portfolio.PutSpaceMemberRequest

type DeleteSpaceMemberRequest = portfolio.DeleteSpaceMemberRequest

type SpaceTeamOutcome = portfolio.SpaceTeamOutcome

const SpaceTeamLeaves = portfolio.SpaceTeamLeaves

const SpaceTeamArchived = portfolio.SpaceTeamArchived

type SpaceAgentRemovalTeam = portfolio.SpaceAgentRemovalTeam

type SpaceAgentRemovalNeedsConfirmation = portfolio.SpaceAgentRemovalNeedsConfirmation

type SpaceService = portfolio.SpaceService

// NewSpaceService forwards to portfolio.NewSpaceService.
func NewSpaceService(store models.SpaceStore, opts ...models.ServiceOption) *SpaceService {
	return portfolio.NewSpaceService(store, opts...)
}
