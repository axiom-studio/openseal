package runtime

import (
	"context"
	"database/sql"
)

func (s *PostgresStore) WithSkillRuntimeMaintenanceMutation(ctx context.Context, proof *SkillRuntimeMaintenance, mutation func(context.Context) error) error {
	if err := validateMaintenanceMutationArguments(proof, mutation); err != nil {
		return err
	}
	_, err := s.withMaintenanceTransaction(ctx, proof.Scope, proof.SkillID, func(tx *sql.Tx) (*SkillRuntimeMaintenance, error) {
		current, err := s.postgresSkillRuntimeMaintenanceActiveTx(ctx, tx, proof.Scope, proof.SkillID)
		if err != nil {
			return nil, err
		}
		return current, executeSkillRuntimeMaintenanceMutation(ctx, proof, current, mutation)
	})
	return err
}
