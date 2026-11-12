package runtime

import (
	"context"
	"errors"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
)

// channelActivationStore activates an Agent definition amendment and writes
// the Agent's channel endpoint in one transaction.
type channelActivationStore interface {
	ActivateAmendmentWithEndpoint(ctx context.Context, registry kernelagent.Store, amendment *kernelagent.DefinitionAmendment, amendmentRevision int64, definition *kernelagent.AgentDefinition, deployment *kernelagent.AgentDeployment, deploymentRevision int64, activation kernelagent.DefinitionActivation, next *ExternalConversationEndpoint, expectedRevision int64) error
}

// channelActivationCommit joins the endpoint CAS to the registry's activation
// transaction. Separate databases and unknown stores fail closed rather than
// activating a definition whose channel could not be materialized.
func (s *ExternalConversationEndpointService) channelActivationCommit(next *ExternalConversationEndpoint, expectedRevision, expectedDeploymentRevision int64) kernelagent.AmendmentActivationCommit {
	return func(ctx context.Context, registryStore kernelagent.Store, amendment *kernelagent.DefinitionAmendment, amendmentRevision int64, definition *kernelagent.AgentDefinition, deployment *kernelagent.AgentDeployment, deploymentRevision int64, activation kernelagent.DefinitionActivation) error {
		if deploymentRevision != expectedDeploymentRevision {
			return kernelagent.ErrRevisionConflict
		}
		if next == nil || next.Validate() != nil || next.Revision != expectedRevision+1 ||
			next.Scope.Kind != deployment.Scope.Kind || next.Scope.ID != deployment.Scope.ID ||
			next.DeploymentID != deployment.ID || next.Owner.Type != OwnerTypeAgent || next.Owner.ID != deployment.ID {
			return ErrInvalidExternalConversation
		}
		store, ok := s.store.(channelActivationStore)
		if !ok {
			return errors.New("Agent and channel activation store does not support atomic commits")
		}
		return store.ActivateAmendmentWithEndpoint(ctx, registryStore, amendment, amendmentRevision, definition, deployment, deploymentRevision, activation, next, expectedRevision)
	}
}
