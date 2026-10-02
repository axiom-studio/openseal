package runtime

import (
	"context"
	"database/sql"
	"time"
)

// WithSkillRuntimeMaintenanceMutation fences publication in an external host
// store against controller takeover and canonical binding changes. The trusted
// callback must honor its context and must not reenter this kernel store. All
// immutable catalog registration and binding Plan/Apply happen outside it.
func (s *MemoryStore) WithSkillRuntimeMaintenanceMutation(ctx context.Context, proof *SkillRuntimeMaintenance, mutation func(context.Context) error) error {
	if err := validateMaintenanceMutationArguments(proof, mutation); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.memorySkillRuntimeMaintenanceActiveLocked(proof.Scope, proof.SkillID)
	return executeSkillRuntimeMaintenanceMutation(ctx, proof, current, mutation)
}

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

func validateMaintenanceMutationArguments(proof *SkillRuntimeMaintenance, mutation func(context.Context) error) error {
	if proof == nil || mutation == nil {
		return ErrSkillRuntimeMaintenanceConflict
	}
	return validateMaintenanceIdentity(proof.Scope, proof.SkillID)
}

func executeSkillRuntimeMaintenanceMutation(ctx context.Context, proof, current *SkillRuntimeMaintenance, mutation func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if current == nil {
		return ErrSkillRuntimeMaintenanceConflict
	}
	now := time.Now().UTC()
	if err := ValidateSkillRuntimeMaintenanceContext(WithSkillRuntimeMaintenance(ctx, proof), current, now); err != nil {
		return err
	}
	deadline := now.Add(15 * time.Second)
	if safeExpiry := current.LeaseExpiresAt.Add(-250 * time.Millisecond); safeExpiry.Before(deadline) {
		deadline = safeExpiry
	}
	if !deadline.After(now) {
		return ErrSkillRuntimeMaintenanceConflict
	}
	bounded, cancel := context.WithDeadline(WithSkillRuntimeMaintenance(ctx, current), deadline)
	defer cancel()
	if err := mutation(bounded); err != nil {
		return err
	}
	if err := bounded.Err(); err != nil {
		return err
	}
	return ValidateSkillRuntimeMaintenanceContext(bounded, current, time.Now().UTC())
}
