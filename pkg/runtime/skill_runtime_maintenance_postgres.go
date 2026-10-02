package runtime

import (
	"context"
	"database/sql"
	"errors"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func (s *PostgresStore) postgresSkillRuntimeMaintenanceActiveTx(ctx context.Context, tx *sql.Tx, scope Scope, skillID string) (*SkillRuntimeMaintenance, error) {
	g, err := readSkillRuntimeMaintenance(ctx, tx, s.table("skill_runtime_maintenance"), scope, skillID, true, false)
	if errors.Is(err, ErrSkillRuntimeMaintenanceNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !g.Active {
		return nil, nil
	}
	return g, nil
}

func (s *PostgresStore) withMaintenanceTransaction(ctx context.Context, scope Scope, skillID string, work func(*sql.Tx) (*SkillRuntimeMaintenance, error)) (*SkillRuntimeMaintenance, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, skillRuntimeMaintenanceLockKey(scope, skillID)); err != nil {
		return nil, err
	}
	result, err := work(tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *PostgresStore) AcquireSkillRuntimeMaintenance(ctx context.Context, r SkillRuntimeMaintenanceRequest) (*SkillRuntimeMaintenance, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return s.withMaintenanceTransaction(ctx, r.Scope, r.SkillID, func(tx *sql.Tx) (*SkillRuntimeMaintenance, error) {
		current, err := readSkillRuntimeMaintenance(ctx, tx, s.table("skill_runtime_maintenance"), r.Scope, r.SkillID, true, true)
		if err != nil && !errors.Is(err, ErrSkillRuntimeMaintenanceNotFound) {
			return nil, err
		}
		next, initial, err := nextSkillRuntimeMaintenance(current, r)
		if err != nil {
			return nil, err
		}
		if initial {
			busy, err := logicalSkillRuntimeUsage(ctx, tx, r.Scope, r.SkillID, s.table("action_calls"), s.table("agent_runs"), true)
			if err != nil {
				return nil, err
			}
			if busy {
				return nil, ErrSkillReferenceUpgradeBusy
			}
			if err := validateMaintenanceBindingSnapshot(ctx, tx, s.table("skill_bindings"), r, true); err != nil {
				return nil, err
			}
		}
		if err := writeSkillRuntimeMaintenance(ctx, tx, s.table("skill_runtime_maintenance"), next, true); err != nil {
			return nil, err
		}
		return next, nil
	})
}

func (s *PostgresStore) GetSkillRuntimeMaintenance(ctx context.Context, scope Scope, skillID string) (*SkillRuntimeMaintenance, error) {
	if err := validateMaintenanceIdentity(scope, skillID); err != nil {
		return nil, err
	}
	return readSkillRuntimeMaintenance(ctx, s.db, s.table("skill_runtime_maintenance"), scope, skillID, true, false)
}
func (s *PostgresStore) ListActiveSkillRuntimeMaintenances(ctx context.Context, scope Scope) ([]*SkillRuntimeMaintenance, error) {
	return listActiveSkillRuntimeMaintenances(ctx, s.db, s.table("skill_runtime_maintenance"), scope, true)
}
func (s *PostgresStore) ListSkillRuntimeMaintenanceBindings(ctx context.Context, scope Scope, skillID, afterDeploymentID, afterBindingID string, limit int) ([]*skill.Binding, error) {
	return listMaintenanceBindings(ctx, s.db, s.table("skill_bindings"), scope, skillID, afterDeploymentID, afterBindingID, limit, true)
}
func (s *PostgresStore) CompleteSkillRuntimeMaintenance(ctx context.Context, r SkillRuntimeMaintenanceCompletion) (*SkillRuntimeMaintenance, error) {
	return s.transitionSkillRuntimeMaintenance(ctx, r, false)
}
func (s *PostgresStore) BeginSkillRuntimeMaintenanceRollback(ctx context.Context, r SkillRuntimeMaintenanceCompletion) (*SkillRuntimeMaintenance, error) {
	return s.transitionSkillRuntimeMaintenance(ctx, r, true)
}
func (s *PostgresStore) transitionSkillRuntimeMaintenance(ctx context.Context, r SkillRuntimeMaintenanceCompletion, rollback bool) (*SkillRuntimeMaintenance, error) {
	if err := validateMaintenanceIdentity(r.Scope, r.SkillID); err != nil {
		return nil, err
	}
	return s.withMaintenanceTransaction(ctx, r.Scope, r.SkillID, func(tx *sql.Tx) (*SkillRuntimeMaintenance, error) {
		g, err := readSkillRuntimeMaintenance(ctx, tx, s.table("skill_runtime_maintenance"), r.Scope, r.SkillID, true, true)
		if err != nil {
			return nil, err
		}
		if err := validateMaintenanceCompletion(g, r); err != nil {
			return nil, err
		}
		var next *SkillRuntimeMaintenance
		if rollback {
			next = beginMaintenanceRollbackRecord(g, r)
		} else {
			if r.VerifiedVersion != g.DesiredVersion {
				return nil, ErrSkillRuntimeMaintenanceConflict
			}
			match, err := maintenanceBindingsMatch(ctx, tx, g, r.VerifiedVersion, s.table("skill_bindings"), true)
			if err != nil {
				return nil, err
			}
			if !match {
				return nil, ErrSkillRuntimeMaintenanceConflict
			}
			busy, err := logicalSkillRuntimeUsage(ctx, tx, r.Scope, r.SkillID, s.table("action_calls"), s.table("agent_runs"), true)
			if err != nil {
				return nil, err
			}
			if busy {
				return nil, ErrSkillReferenceUpgradeBusy
			}
			next = completeMaintenanceRecord(g, r)
		}
		if err := writeSkillRuntimeMaintenance(ctx, tx, s.table("skill_runtime_maintenance"), next, true); err != nil {
			return nil, err
		}
		return next, nil
	})
}
