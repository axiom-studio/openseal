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

func logicalSkillRuntimeUsage(ctx context.Context, q skillRuntimeUsageQuerier, scope Scope, skillID, actions, runs string, postgres bool) (bool, error) {
	parameter := func(n int) string {
		if postgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}
	identity := "a.scope_kind=" + parameter(1) + " AND a.scope_id=" + parameter(2) + " AND a.skill_id=" + parameter(3)
	query := "SELECT EXISTS (SELECT 1 FROM " + actions + " a WHERE " + identity + " AND a.status IN (" + unfinishedSkillActionSQL + ") LIMIT 1) OR EXISTS (SELECT 1 FROM " + runs + " r WHERE r.scope_kind=" + parameter(1) + " AND r.scope_id=" + parameter(2) + " AND r.status IN (" + unfinishedSkillRunSQL + ") AND EXISTS (SELECT 1 FROM " + actions + " a WHERE " + identity + " AND a.run_id=r.id AND a.status='succeeded' LIMIT 1) LIMIT 1)"
	args := []interface{}{scope.Kind, scope.ID, skillID}
	if !postgres {
		args = append(args, scope.Kind, scope.ID, scope.Kind, scope.ID, skillID)
	}
	query += " OR EXISTS (SELECT 1 FROM " + skillRuntimeDependencyTable(runs) + " d WHERE d.scope_kind=" + parameter(1) + " AND d.scope_id=" + parameter(2) + " AND d.skill_id=" + parameter(3) + " LIMIT 1)"
	if !postgres {
		args = append(args, scope.Kind, scope.ID, skillID)
	}
	var busy bool
	err := q.QueryRowContext(ctx, query, args...).Scan(&busy)
	return busy, err
}

func maintenanceBindingsMatch(ctx context.Context, q skillRuntimeUsageQuerier, g *SkillRuntimeMaintenance, version, bindings string, postgres bool) (bool, error) {
	parameter := func(n int) string {
		if postgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}
	enabled := "COALESCE(json_extract(payload,'$.disabled'),0)=0"
	if postgres {
		enabled = "COALESCE((payload->>'disabled')::boolean,false)=false"
	}
	query := "SELECT NOT EXISTS (SELECT 1 FROM " + bindings + " WHERE scope_kind=" + parameter(1) + " AND scope_id=" + parameter(2) + " AND skill_id=" + parameter(3) + " AND " + enabled + " AND (skill_version<>" + parameter(4) + " OR source_identity<>" + parameter(5) + ") LIMIT 1)"
	var matched bool
	err := q.QueryRowContext(ctx, query, g.Scope.Kind, g.Scope.ID, g.SkillID, version, g.SourceIdentity).Scan(&matched)
	return matched, err
}

var _ skillRuntimeUsageQuerier = (*sql.Conn)(nil)

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
			if err := AddSkillRuntimeMaintenanceBindingDigest(h, SkillRuntimeMaintenanceBindingRevision{DeploymentID: deployment, BindingID: id, Revision: revision}); err != nil {
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
