package postgres

import (
	"context"
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/models"
)

func (s *Store) WithSkillRuntimeMaintenanceMutation(ctx context.Context, proof *models.SkillRuntimeMaintenance, mutation func(context.Context) error) error {
	if err := models.ValidateMaintenanceMutationArguments(proof, mutation); err != nil {
		return err
	}
	_, err := s.withMaintenanceTransaction(ctx, proof.Scope, proof.SkillID, func(tx *sql.Tx) (*models.SkillRuntimeMaintenance, error) {
		current, err := s.postgresSkillRuntimeMaintenanceActiveTx(ctx, tx, proof.Scope, proof.SkillID)
		if err != nil {
			return nil, err
		}
		return current, models.ExecuteSkillRuntimeMaintenanceMutation(ctx, proof, current, mutation)
	})
	return err
}
