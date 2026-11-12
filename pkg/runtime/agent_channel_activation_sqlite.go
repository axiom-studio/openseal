package runtime

import (
	"context"
	"database/sql"
	"errors"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
)

// ActivateAmendmentWithEndpoint activates an Agent definition amendment and
// writes the Agent's channel endpoint in the same transaction. The registry
// must be this store: the endpoint write cannot join another database's
// transaction.
func (s *SQLiteStore) ActivateAmendmentWithEndpoint(ctx context.Context, registry kernelagent.Store, amendment *kernelagent.DefinitionAmendment, amendmentRevision int64, definition *kernelagent.AgentDefinition, deployment *kernelagent.AgentDeployment, deploymentRevision int64, activation kernelagent.DefinitionActivation, next *models.ExternalConversationEndpoint, expectedRevision int64) error {
	if registry != s {
		return errors.New("Agent and channel activation must share the same store")
	}
	return s.activateAmendment(ctx, amendment, amendmentRevision, definition, deployment, deploymentRevision, activation, func(tx *sql.Tx) error {
		return s.updateExternalConversationEndpoint(ctx, tx, next, expectedRevision)
	})
}
