package runtime

import (
	"context"
	"database/sql"
	"errors"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func sqliteSkillRuntimeMaintenanceActiveTx(ctx context.Context, q skillRuntimeUsageQuerier, scope Scope, skillID string) (*SkillRuntimeMaintenance, error) {
	g, err := readSkillRuntimeMaintenance(ctx, q, "skill_runtime_maintenance", scope, skillID, false, false)
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

func (s *SQLiteStore) withMaintenanceTransaction(ctx context.Context, work func(*sql.Conn) (*SkillRuntimeMaintenance, error)) (*SkillRuntimeMaintenance, error) {
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

func (s *SQLiteStore) AcquireSkillRuntimeMaintenance(ctx context.Context, r SkillRuntimeMaintenanceRequest) (*SkillRuntimeMaintenance, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return s.withMaintenanceTransaction(ctx, func(conn *sql.Conn) (*SkillRuntimeMaintenance, error) {
		current, err := readSkillRuntimeMaintenance(ctx, conn, "skill_runtime_maintenance", r.Scope, r.SkillID, false, false)
		if err != nil && !errors.Is(err, ErrSkillRuntimeMaintenanceNotFound) {
			return nil, err
		}
		next, initial, err := nextSkillRuntimeMaintenance(current, r)
		if err != nil {
			return nil, err
		}
		if initial {
			busy, err := logicalSkillRuntimeUsage(ctx, conn, r.Scope, r.SkillID, "action_calls", "agent_runs", false)
			if err != nil {
				return nil, err
			}
			if busy {
				return nil, ErrSkillReferenceUpgradeBusy
			}
			if err := validateMaintenanceBindingSnapshot(ctx, conn, "skill_bindings", r, false); err != nil {
				return nil, err
			}
		}
		if err := writeSkillRuntimeMaintenance(ctx, conn, "skill_runtime_maintenance", next, false); err != nil {
			return nil, err
		}
		return next, nil
	})
}

func (s *SQLiteStore) GetSkillRuntimeMaintenance(ctx context.Context, scope Scope, skillID string) (*SkillRuntimeMaintenance, error) {
	if err := validateMaintenanceIdentity(scope, skillID); err != nil {
		return nil, err
	}
	return readSkillRuntimeMaintenance(ctx, s.db, "skill_runtime_maintenance", scope, skillID, false, false)
}
func (s *SQLiteStore) ListActiveSkillRuntimeMaintenances(ctx context.Context, scope Scope) ([]*SkillRuntimeMaintenance, error) {
	return listActiveSkillRuntimeMaintenances(ctx, s.db, "skill_runtime_maintenance", scope, false)
}
func (s *SQLiteStore) ListSkillRuntimeMaintenanceBindings(ctx context.Context, scope Scope, skillID, afterDeploymentID, afterBindingID string, limit int) ([]*skill.Binding, error) {
	return listMaintenanceBindings(ctx, s.db, "skill_bindings", scope, skillID, afterDeploymentID, afterBindingID, limit, false)
}

func (s *SQLiteStore) CompleteSkillRuntimeMaintenance(ctx context.Context, r SkillRuntimeMaintenanceCompletion) (*SkillRuntimeMaintenance, error) {
	return s.transitionSkillRuntimeMaintenance(ctx, r, false)
}
func (s *SQLiteStore) BeginSkillRuntimeMaintenanceRollback(ctx context.Context, r SkillRuntimeMaintenanceCompletion) (*SkillRuntimeMaintenance, error) {
	return s.transitionSkillRuntimeMaintenance(ctx, r, true)
}
func (s *SQLiteStore) transitionSkillRuntimeMaintenance(ctx context.Context, r SkillRuntimeMaintenanceCompletion, rollback bool) (*SkillRuntimeMaintenance, error) {
	if err := validateMaintenanceIdentity(r.Scope, r.SkillID); err != nil {
		return nil, err
	}
	return s.withMaintenanceTransaction(ctx, func(conn *sql.Conn) (*SkillRuntimeMaintenance, error) {
		g, err := readSkillRuntimeMaintenance(ctx, conn, "skill_runtime_maintenance", r.Scope, r.SkillID, false, false)
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
			match, err := maintenanceBindingsMatch(ctx, conn, g, r.VerifiedVersion, "skill_bindings", false)
			if err != nil {
				return nil, err
			}
			if !match {
				return nil, ErrSkillRuntimeMaintenanceConflict
			}
			busy, err := logicalSkillRuntimeUsage(ctx, conn, r.Scope, r.SkillID, "action_calls", "agent_runs", false)
			if err != nil {
				return nil, err
			}
			if busy {
				return nil, ErrSkillReferenceUpgradeBusy
			}
			next = completeMaintenanceRecord(g, r)
		}
		if err := writeSkillRuntimeMaintenance(ctx, conn, "skill_runtime_maintenance", next, false); err != nil {
			return nil, err
		}
		return next, nil
	})
}
