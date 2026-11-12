package runtime

import (
	"context"
	"database/sql"
)

func (s *SQLiteStore) WithSkillRuntimeMaintenanceMutation(ctx context.Context, proof *SkillRuntimeMaintenance, mutation func(context.Context) error) error {
	if err := validateMaintenanceMutationArguments(proof, mutation); err != nil {
		return err
	}
	_, err := s.withMaintenanceTransaction(ctx, func(conn *sql.Conn) (*SkillRuntimeMaintenance, error) {
		current, err := sqliteSkillRuntimeMaintenanceActiveTx(ctx, conn, proof.Scope, proof.SkillID)
		if err != nil {
			return nil, err
		}
		return current, executeSkillRuntimeMaintenanceMutation(ctx, proof, current, mutation)
	})
	return err
}
