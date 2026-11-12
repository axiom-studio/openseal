package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func (s *PostgresStore) postgresSkillRuntimeMaintenanceActiveTx(ctx context.Context, tx *sql.Tx, scope models.Scope, skillID string) (*models.SkillRuntimeMaintenance, error) {
	g, err := sqlstore.ReadSkillRuntimeMaintenance(ctx, tx, s.table("skill_runtime_maintenance"), scope, skillID, true, false)
	if errors.Is(err, models.ErrSkillRuntimeMaintenanceNotFound) {
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

func (s *PostgresStore) withMaintenanceTransaction(ctx context.Context, scope models.Scope, skillID string, work func(*sql.Tx) (*models.SkillRuntimeMaintenance, error)) (*models.SkillRuntimeMaintenance, error) {
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

func (s *PostgresStore) AcquireSkillRuntimeMaintenance(ctx context.Context, r models.SkillRuntimeMaintenanceRequest) (*models.SkillRuntimeMaintenance, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return s.withMaintenanceTransaction(ctx, r.Scope, r.SkillID, func(tx *sql.Tx) (*models.SkillRuntimeMaintenance, error) {
		current, err := sqlstore.ReadSkillRuntimeMaintenance(ctx, tx, s.table("skill_runtime_maintenance"), r.Scope, r.SkillID, true, true)
		if err != nil && !errors.Is(err, models.ErrSkillRuntimeMaintenanceNotFound) {
			return nil, err
		}
		next, initial, err := models.NextSkillRuntimeMaintenance(current, r)
		if err != nil {
			return nil, err
		}
		if initial {
			busy, err := sqlstore.LogicalSkillRuntimeUsage(ctx, tx, r.Scope, r.SkillID, s.table("action_calls"), s.table("agent_runs"), true)
			if err != nil {
				return nil, err
			}
			if busy {
				return nil, models.ErrSkillReferenceUpgradeBusy
			}
			if err := sqlstore.ValidateMaintenanceBindingSnapshot(ctx, tx, s.table("skill_bindings"), r, true); err != nil {
				return nil, err
			}
		}
		if err := sqlstore.WriteSkillRuntimeMaintenance(ctx, tx, s.table("skill_runtime_maintenance"), next, true); err != nil {
			return nil, err
		}
		return next, nil
	})
}

func (s *PostgresStore) GetSkillRuntimeMaintenance(ctx context.Context, scope models.Scope, skillID string) (*models.SkillRuntimeMaintenance, error) {
	if err := models.ValidateMaintenanceIdentity(scope, skillID); err != nil {
		return nil, err
	}
	return sqlstore.ReadSkillRuntimeMaintenance(ctx, s.db, s.table("skill_runtime_maintenance"), scope, skillID, true, false)
}
func (s *PostgresStore) ListActiveSkillRuntimeMaintenances(ctx context.Context, scope models.Scope) ([]*models.SkillRuntimeMaintenance, error) {
	return sqlstore.ListActiveSkillRuntimeMaintenances(ctx, s.db, s.table("skill_runtime_maintenance"), scope, true)
}
func (s *PostgresStore) ListSkillRuntimeMaintenanceBindings(ctx context.Context, scope models.Scope, skillID, afterDeploymentID, afterBindingID string, limit int) ([]*skill.Binding, error) {
	return sqlstore.ListMaintenanceBindings(ctx, s.db, s.table("skill_bindings"), scope, skillID, afterDeploymentID, afterBindingID, limit, true)
}
func (s *PostgresStore) CompleteSkillRuntimeMaintenance(ctx context.Context, r models.SkillRuntimeMaintenanceCompletion) (*models.SkillRuntimeMaintenance, error) {
	return s.transitionSkillRuntimeMaintenance(ctx, r, false)
}
func (s *PostgresStore) BeginSkillRuntimeMaintenanceRollback(ctx context.Context, r models.SkillRuntimeMaintenanceCompletion) (*models.SkillRuntimeMaintenance, error) {
	return s.transitionSkillRuntimeMaintenance(ctx, r, true)
}
func (s *PostgresStore) transitionSkillRuntimeMaintenance(ctx context.Context, r models.SkillRuntimeMaintenanceCompletion, rollback bool) (*models.SkillRuntimeMaintenance, error) {
	if err := models.ValidateMaintenanceIdentity(r.Scope, r.SkillID); err != nil {
		return nil, err
	}
	return s.withMaintenanceTransaction(ctx, r.Scope, r.SkillID, func(tx *sql.Tx) (*models.SkillRuntimeMaintenance, error) {
		g, err := sqlstore.ReadSkillRuntimeMaintenance(ctx, tx, s.table("skill_runtime_maintenance"), r.Scope, r.SkillID, true, true)
		if err != nil {
			return nil, err
		}
		if err := models.ValidateMaintenanceCompletion(g, r); err != nil {
			return nil, err
		}
		var next *models.SkillRuntimeMaintenance
		if rollback {
			next = models.BeginMaintenanceRollbackRecord(g, r)
		} else {
			if r.VerifiedVersion != g.DesiredVersion {
				return nil, models.ErrSkillRuntimeMaintenanceConflict
			}
			match, err := sqlstore.MaintenanceBindingsMatch(ctx, tx, g, r.VerifiedVersion, s.table("skill_bindings"), true)
			if err != nil {
				return nil, err
			}
			if !match {
				return nil, models.ErrSkillRuntimeMaintenanceConflict
			}
			busy, err := sqlstore.LogicalSkillRuntimeUsage(ctx, tx, r.Scope, r.SkillID, s.table("action_calls"), s.table("agent_runs"), true)
			if err != nil {
				return nil, err
			}
			if busy {
				return nil, models.ErrSkillReferenceUpgradeBusy
			}
			next = models.CompleteMaintenanceRecord(g, r)
		}
		if err := sqlstore.WriteSkillRuntimeMaintenance(ctx, tx, s.table("skill_runtime_maintenance"), next, true); err != nil {
			return nil, err
		}
		return next, nil
	})
}

func skillRuntimeMaintenanceLockKey(scope models.Scope, skillID string) string {
	return fmt.Sprintf("skill-runtime-maintenance:%d:%s:%d:%s:%d:%s", len(scope.Kind), scope.Kind, len(scope.ID), scope.ID, len(skillID), skillID)
}
