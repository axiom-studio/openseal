package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
)

type workforceSQLQuery interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}

// Restrict reconciliation to the deployments being changed. The same selector
// supplies the maintenance fence and the rows that may be updated or removed.
func workforceReconciledBindingPredicate(value *authoring.ChangeSet, desired []*capability.Binding, postgres bool) (string, []interface{}) {
	deployments := workforceBindingReconciliationDeployments(value)
	if len(deployments) == 0 {
		return "", nil
	}
	deploymentIDs := make([]string, 0, len(deployments))
	for id := range deployments {
		deploymentIDs = append(deploymentIDs, id)
	}
	sort.Strings(deploymentIDs)
	ids := make(map[string]struct{}, len(desired))
	for _, binding := range desired {
		if binding != nil {
			ids[binding.ID] = struct{}{}
		}
	}
	bindingIDs := make([]string, 0, len(ids))
	for id := range ids {
		bindingIDs = append(bindingIDs, id)
	}
	sort.Strings(bindingIDs)
	args := []interface{}{value.Scope.Kind, value.Scope.ID}
	parameter := func(value interface{}) string {
		args = append(args, value)
		if postgres {
			return fmt.Sprintf("$%d", len(args))
		}
		return "?"
	}
	deploymentParameters := make([]string, 0, len(deploymentIDs))
	for _, id := range deploymentIDs {
		deploymentParameters = append(deploymentParameters, parameter(id))
	}
	predicate := "scope_kind=? AND scope_id=?"
	if postgres {
		predicate = "scope_kind=$1 AND scope_id=$2"
	}
	predicate += " AND deployment_id IN (" + strings.Join(deploymentParameters, ",") + ") AND (id LIKE 'workforce:%'"
	if len(bindingIDs) > 0 {
		parameters := make([]string, 0, len(bindingIDs))
		for _, id := range bindingIDs {
			parameters = append(parameters, parameter(id))
		}
		predicate += " OR id IN (" + strings.Join(parameters, ",") + ")"
	}
	return predicate + ")", args
}

func workforceMaintenanceSkills(ctx context.Context, query workforceSQLQuery, table string, value *authoring.ChangeSet, desired []*capability.Binding, postgres bool) ([]string, error) {
	skills := make(map[string]struct{}, len(desired))
	for _, binding := range desired {
		if binding == nil || binding.Scope != value.Scope {
			return nil, ErrInvalidScope
		}
		skills[binding.SkillID] = struct{}{}
	}
	if predicate, args := workforceReconciledBindingPredicate(value, desired, postgres); predicate != "" {
		rows, err := query.QueryContext(ctx, "SELECT DISTINCT skill_id FROM "+table+" WHERE "+predicate, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			skills[id] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	result := make([]string, 0, len(skills))
	for id := range skills {
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
}

func fenceSQLiteWorkforceMaintenance(ctx context.Context, query workforceSQLQuery, value *authoring.ChangeSet, desired []*capability.Binding) error {
	skills, err := workforceMaintenanceSkills(ctx, query, "skill_bindings", value, desired, false)
	if err != nil {
		return err
	}
	for _, id := range skills {
		gate, err := sqliteSkillRuntimeMaintenanceActiveTx(ctx, query, Scope(value.Scope), id)
		if err != nil {
			return err
		}
		if gate != nil {
			return &SkillRuntimeMaintenanceError{Maintenance: *gate}
		}
	}
	return nil
}

func (s *PostgresStore) fencePostgresWorkforceMaintenance(ctx context.Context, tx *sql.Tx, value *authoring.ChangeSet, desired []*capability.Binding) (map[string]struct{}, error) {
	skills, err := workforceMaintenanceSkills(ctx, tx, s.table("skill_bindings"), value, desired, true)
	if err != nil {
		return nil, err
	}
	fenced := make(map[string]struct{}, len(skills))
	for _, id := range skills {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended($1, 0))`, skillRuntimeMaintenanceLockKey(Scope(value.Scope), id)); err != nil {
			return nil, err
		}
		gate, err := s.postgresSkillRuntimeMaintenanceActiveTx(ctx, tx, Scope(value.Scope), id)
		if err != nil {
			return nil, err
		}
		if gate != nil {
			return nil, &SkillRuntimeMaintenanceError{Maintenance: *gate}
		}
		fenced[id] = struct{}{}
	}
	return fenced, nil
}
