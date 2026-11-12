package sqlite

import (
	"context"
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/models"
)

func (s *SQLiteStore) WithSkillRuntimeMaintenanceMutation(ctx context.Context, proof *models.SkillRuntimeMaintenance, mutation func(context.Context) error) error {
	if err := models.ValidateMaintenanceMutationArguments(proof, mutation); err != nil {
		return err
	}
	_, err := s.withMaintenanceTransaction(ctx, func(conn *sql.Conn) (*models.SkillRuntimeMaintenance, error) {
		current, err := sqliteSkillRuntimeMaintenanceActiveTx(ctx, conn, proof.Scope, proof.SkillID)
		if err != nil {
			return nil, err
		}
		return current, models.ExecuteSkillRuntimeMaintenanceMutation(ctx, proof, current, mutation)
	})
	return err
}
