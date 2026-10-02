package runtime

import (
	"context"
	"database/sql"
	"errors"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
)

type endpointUpdateExecutor interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
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
		switch store := s.store.(type) {
		case *PostgresStore:
			if registryStore != store {
				return errors.New("Agent and channel activation must share the same store")
			}
			return store.activateAmendment(ctx, amendment, amendmentRevision, definition, deployment, deploymentRevision, activation, func(tx *sql.Tx) error {
				return store.updateExternalConversationEndpoint(ctx, tx, next, expectedRevision)
			})
		case *SQLiteStore:
			if registryStore != store {
				return errors.New("Agent and channel activation must share the same store")
			}
			return store.activateAmendment(ctx, amendment, amendmentRevision, definition, deployment, deploymentRevision, activation, func(tx *sql.Tx) error {
				return store.updateExternalConversationEndpoint(ctx, tx, next, expectedRevision)
			})
		case *MemoryStore:
			// Hold endpoint access until the all-or-nothing memory registry commit
			// succeeds. Everything after that commit is an infallible map update.
			if _, ok := registryStore.(*kernelagent.MemoryStore); !ok {
				return errors.New("Agent and channel activation stores cannot commit atomically")
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			key := externalConversationEndpointKey(next.Scope, next.ID)
			current := store.externalEndpoints[key]
			if current == nil || current.Revision != expectedRevision || current.IngressRoute != next.IngressRoute {
				return ErrExternalConversationConflict
			}
			if err := registryStore.ActivateAmendment(ctx, amendment, amendmentRevision, definition, deployment, deploymentRevision, activation); err != nil {
				return err
			}
			store.externalEndpoints[key] = cloneExternalConversationEndpoint(next)
			return nil
		default:
			return errors.New("Agent and channel activation store does not support atomic commits")
		}
	}
}
