package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func sqliteSkillRuntimeMaintenanceActiveTx(ctx context.Context, q sqlstore.RowQuerier, scope models.Scope, skillID string) (*models.SkillRuntimeMaintenance, error) {
	g, err := sqlstore.ReadSkillRuntimeMaintenance(ctx, q, "skill_runtime_maintenance", scope, skillID, false, false)
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

func (s *SQLiteStore) withMaintenanceTransaction(ctx context.Context, work func(*sql.Conn) (*models.SkillRuntimeMaintenance, error)) (*models.SkillRuntimeMaintenance, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	result, err := work(conn)
	if err != nil {
		return nil, err
	}
	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return result, nil
}

func (s *SQLiteStore) AcquireSkillRuntimeMaintenance(ctx context.Context, r models.SkillRuntimeMaintenanceRequest) (*models.SkillRuntimeMaintenance, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return s.withMaintenanceTransaction(ctx, func(conn *sql.Conn) (*models.SkillRuntimeMaintenance, error) {
		current, err := sqlstore.ReadSkillRuntimeMaintenance(ctx, conn, "skill_runtime_maintenance", r.Scope, r.SkillID, false, false)
		if err != nil && !errors.Is(err, models.ErrSkillRuntimeMaintenanceNotFound) {
			return nil, err
		}
		next, initial, err := models.NextSkillRuntimeMaintenance(current, r)
		if err != nil {
			return nil, err
		}
		if initial {
			busy, err := sqlstore.LogicalSkillRuntimeUsage(ctx, conn, r.Scope, r.SkillID, "action_calls", "agent_runs", false)
			if err != nil {
				return nil, err
			}
			if busy {
				return nil, models.ErrSkillReferenceUpgradeBusy
			}
			if err := sqlstore.ValidateMaintenanceBindingSnapshot(ctx, conn, "skill_bindings", r, false); err != nil {
				return nil, err
			}
		}
		if err := sqlstore.WriteSkillRuntimeMaintenance(ctx, conn, "skill_runtime_maintenance", next, false); err != nil {
			return nil, err
		}
		return next, nil
	})
}

func (s *SQLiteStore) GetSkillRuntimeMaintenance(ctx context.Context, scope models.Scope, skillID string) (*models.SkillRuntimeMaintenance, error) {
	if err := models.ValidateMaintenanceIdentity(scope, skillID); err != nil {
		return nil, err
	}
	return sqlstore.ReadSkillRuntimeMaintenance(ctx, s.db, "skill_runtime_maintenance", scope, skillID, false, false)
}
func (s *SQLiteStore) ListActiveSkillRuntimeMaintenances(ctx context.Context, scope models.Scope) ([]*models.SkillRuntimeMaintenance, error) {
	return sqlstore.ListActiveSkillRuntimeMaintenances(ctx, s.db, "skill_runtime_maintenance", scope, false)
}
func (s *SQLiteStore) ListSkillRuntimeMaintenanceBindings(ctx context.Context, scope models.Scope, skillID, afterDeploymentID, afterBindingID string, limit int) ([]*skill.Binding, error) {
	return sqlstore.ListMaintenanceBindings(ctx, s.db, "skill_bindings", scope, skillID, afterDeploymentID, afterBindingID, limit, false)
}

func (s *SQLiteStore) CompleteSkillRuntimeMaintenance(ctx context.Context, r models.SkillRuntimeMaintenanceCompletion) (*models.SkillRuntimeMaintenance, error) {
	return s.transitionSkillRuntimeMaintenance(ctx, r, false)
}
func (s *SQLiteStore) BeginSkillRuntimeMaintenanceRollback(ctx context.Context, r models.SkillRuntimeMaintenanceCompletion) (*models.SkillRuntimeMaintenance, error) {
	return s.transitionSkillRuntimeMaintenance(ctx, r, true)
}
func (s *SQLiteStore) transitionSkillRuntimeMaintenance(ctx context.Context, r models.SkillRuntimeMaintenanceCompletion, rollback bool) (*models.SkillRuntimeMaintenance, error) {
	if err := models.ValidateMaintenanceIdentity(r.Scope, r.SkillID); err != nil {
		return nil, err
	}
	return s.withMaintenanceTransaction(ctx, func(conn *sql.Conn) (*models.SkillRuntimeMaintenance, error) {
		g, err := sqlstore.ReadSkillRuntimeMaintenance(ctx, conn, "skill_runtime_maintenance", r.Scope, r.SkillID, false, false)
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
			match, err := sqlstore.MaintenanceBindingsMatch(ctx, conn, g, r.VerifiedVersion, "skill_bindings", false)
			if err != nil {
				return nil, err
			}
			if !match {
				return nil, models.ErrSkillRuntimeMaintenanceConflict
			}
			busy, err := sqlstore.LogicalSkillRuntimeUsage(ctx, conn, r.Scope, r.SkillID, "action_calls", "agent_runs", false)
			if err != nil {
				return nil, err
			}
			if busy {
				return nil, models.ErrSkillReferenceUpgradeBusy
			}
			next = models.CompleteMaintenanceRecord(g, r)
		}
		if err := sqlstore.WriteSkillRuntimeMaintenance(ctx, conn, "skill_runtime_maintenance", next, false); err != nil {
			return nil, err
		}
		return next, nil
	})
}
