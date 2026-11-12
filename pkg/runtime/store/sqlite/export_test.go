package sqlite

import (
	"context"
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/models"
)

// CreateRunSessionInOwnTx runs the store's in-transaction
// createRunAgentSessionTx in a write transaction of its own. It is the hook
// storetest.RunAgentSessionContract forks sessions through.
func CreateRunSessionInOwnTx(ctx context.Context, s *Store, req models.CreateRunAgentSessionTxRequest) (*models.OpenAgentSessionResult, error) {
	var result *models.OpenAgentSessionResult
	err := s.withImmediateTx(ctx, func(conn *sql.Conn) error {
		var err error
		result, err = s.createRunAgentSessionTx(ctx, conn, req)
		return err
	})
	return result, err
}
