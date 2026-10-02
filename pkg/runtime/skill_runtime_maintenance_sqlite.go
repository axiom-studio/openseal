package runtime

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/skill"
)

type maintenanceSQLQuery interface {
	skillRuntimeUsageQuerier
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}

func readSkillRuntimeMaintenance(ctx context.Context, q skillRuntimeUsageQuerier, table string, scope Scope, skillID string, postgres, lock bool) (*SkillRuntimeMaintenance, error) {
	query := "SELECT payload FROM " + table + " WHERE scope_kind=? AND scope_id=? AND skill_id=?"
	if postgres {
		query = "SELECT payload FROM " + table + " WHERE scope_kind=$1 AND scope_id=$2 AND skill_id=$3"
		if lock {
			query += " FOR UPDATE"
		}
	}
	var payload string
	if err := q.QueryRowContext(ctx, query, scope.Kind, scope.ID, skillID).Scan(&payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSkillRuntimeMaintenanceNotFound
		}
		return nil, err
	}
	var g SkillRuntimeMaintenance
	if err := json.Unmarshal([]byte(payload), &g); err != nil {
		return nil, err
	}
	if g.Scope != scope || g.SkillID != skillID || g.Revision < 1 || g.OperationID == "" {
		return nil, ErrSkillRuntimeMaintenanceConflict
	}
	return &g, nil
}

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

func writeSkillRuntimeMaintenance(ctx context.Context, q maintenanceSQLQuery, table string, g *SkillRuntimeMaintenance, postgres bool) error {
	payload, err := json.Marshal(g)
	if err != nil {
		return err
	}
	query := "INSERT INTO " + table + "(scope_kind,scope_id,skill_id,active,revision,payload) VALUES(?,?,?,?,?,?) ON CONFLICT(scope_kind,scope_id,skill_id) DO UPDATE SET active=excluded.active,revision=excluded.revision,payload=excluded.payload"
	if postgres {
		query = "INSERT INTO " + table + "(scope_kind,scope_id,skill_id,active,revision,payload) VALUES($1,$2,$3,$4,$5,$6::jsonb) ON CONFLICT(scope_kind,scope_id,skill_id) DO UPDATE SET active=excluded.active,revision=excluded.revision,payload=excluded.payload"
	}
	_, err = q.ExecContext(ctx, query, g.Scope.Kind, g.Scope.ID, g.SkillID, g.Active, g.Revision, string(payload))
	return err
}

func validateMaintenanceBindingSnapshot(ctx context.Context, q maintenanceSQLQuery, table string, r SkillRuntimeMaintenanceRequest, postgres bool) error {
	expected := make(map[string]int64, len(r.ExpectedBindings))
	for _, b := range r.ExpectedBindings {
		expected[b.DeploymentID+"\x00"+b.BindingID] = b.Revision
	}
	query := "SELECT deployment_id,id,revision,source_identity FROM " + table + " WHERE scope_kind=? AND scope_id=? AND skill_id=? AND COALESCE(json_extract(payload,'$.disabled'),0)=0 ORDER BY deployment_id,id"
	if postgres {
		query = "SELECT deployment_id,id,revision,source_identity FROM " + table + " WHERE scope_kind=$1 AND scope_id=$2 AND skill_id=$3 AND COALESCE((payload->>'disabled')::boolean,false)=false ORDER BY deployment_id,id"
	}
	rows, err := q.QueryContext(ctx, query, r.Scope.Kind, r.Scope.ID, r.SkillID)
	if err != nil {
		return err
	}
	defer rows.Close()
	h := sha256.New()
	for rows.Next() {
		var deployment, id, source string
		var revision int64
		if err := rows.Scan(&deployment, &id, &revision, &source); err != nil {
			return err
		}
		key := deployment + "\x00" + id
		if source != r.SourceIdentity {
			return ErrSkillRuntimeMaintenanceConflict
		}
		if r.ExpectedBindingDigest == "" {
			if expected[key] != revision {
				return ErrSkillRuntimeMaintenanceConflict
			}
			delete(expected, key)
		} else {
			if err := AddSkillRuntimeMaintenanceBindingDigest(h, SkillRuntimeMaintenanceBindingRevision{deployment, id, revision}); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(expected) != 0 {
		return ErrSkillRuntimeMaintenanceConflict
	}
	if r.ExpectedBindingDigest != "" && fmt.Sprintf("sha256:%x", h.Sum(nil)) != r.ExpectedBindingDigest {
		return ErrSkillRuntimeMaintenanceConflict
	}
	return nil
}

func listActiveSkillRuntimeMaintenances(ctx context.Context, q maintenanceSQLQuery, table string, scope Scope, postgres bool) ([]*SkillRuntimeMaintenance, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	query := "SELECT payload FROM " + table + " WHERE scope_kind=? AND scope_id=? AND active=1 ORDER BY skill_id"
	if postgres {
		query = "SELECT payload FROM " + table + " WHERE scope_kind=$1 AND scope_id=$2 AND active=TRUE ORDER BY skill_id"
	}
	rows, err := q.QueryContext(ctx, query, scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*SkillRuntimeMaintenance, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var g SkillRuntimeMaintenance
		if err := json.Unmarshal([]byte(payload), &g); err != nil {
			return nil, err
		}
		if g.Scope != scope || !g.Active || g.Revision < 1 {
			return nil, ErrSkillRuntimeMaintenanceConflict
		}
		result = append(result, &g)
	}
	return result, rows.Err()
}

func listMaintenanceBindings(ctx context.Context, q maintenanceSQLQuery, table string, scope Scope, skillID, afterDeploymentID, afterBindingID string, limit int, postgres bool) ([]*skill.Binding, error) {
	if err := validateMaintenanceBindingPage(scope, skillID, afterDeploymentID, afterBindingID, limit); err != nil {
		return nil, err
	}
	query := "SELECT payload FROM " + table + " WHERE scope_kind=? AND scope_id=? AND skill_id=? AND (deployment_id>? OR (deployment_id=? AND id>?)) AND COALESCE(json_extract(payload,'$.disabled'),0)=0 ORDER BY deployment_id,id LIMIT ?"
	args := []interface{}{scope.Kind, scope.ID, skillID, afterDeploymentID, afterDeploymentID, afterBindingID, limit}
	if postgres {
		query = "SELECT payload FROM " + table + " WHERE scope_kind=$1 AND scope_id=$2 AND skill_id=$3 AND (deployment_id>$4 OR (deployment_id=$4 AND id>$5)) AND COALESCE((payload->>'disabled')::boolean,false)=false ORDER BY deployment_id,id LIMIT $6"
		args = []interface{}{scope.Kind, scope.ID, skillID, afterDeploymentID, afterBindingID, limit}
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*skill.Binding, 0, limit)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var b skill.Binding
		if err := json.Unmarshal([]byte(payload), &b); err != nil {
			return nil, err
		}
		if b.Scope.Kind != scope.Kind || b.Scope.ID != scope.ID || b.SkillID != skillID || b.Disabled {
			return nil, ErrSkillRuntimeMaintenanceConflict
		}
		result = append(result, &b)
	}
	return result, rows.Err()
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
