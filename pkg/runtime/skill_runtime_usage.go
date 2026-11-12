package runtime

import (
	"context"
	"database/sql"
	"fmt"
)

type skillRuntimeUsageQuerier interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

const unfinishedSkillActionSQL = "'ready','waiting_for_approval','running','compensating'"
const unfinishedSkillRunSQL = "'queued','planning','running','paused','sleeping','waiting_for_dependency','waiting_for_agent','waiting_for_approval','waiting_for_event'"

// Indexed EXISTS probes inspect metadata only. The receipt probe begins
// with active Runs, so completed receipt history does not become a growing
// payload scan on every reconciliation pass.
func skillRuntimeUsageQuery(filter SkillRuntimeUsageFilter, actions, runs string, postgres bool) (string, []interface{}) {
	args := []interface{}{filter.Scope.Kind, filter.Scope.ID, filter.SkillID, filter.SkillVersion}
	parameter := func(n int) string {
		if postgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}
	identity := func() string {
		predicate := "a.scope_kind=" + parameter(1) + " AND a.scope_id=" + parameter(2) + " AND a.skill_id=" + parameter(3) + " AND a.skill_version=" + parameter(4)
		if filter.DeploymentID != "" {
			// Unbound historical calls cannot prove which account supplied their
			// authority. Conservatively drain them before changing any binding of
			// this same deployment and immutable Skill version.
			predicate += " AND a.deployment_id=" + parameter(5) + " AND a.binding_id IN (" + parameter(6) + ",'')"
		}
		return predicate
	}
	if filter.DeploymentID != "" {
		args = append(args, filter.DeploymentID, filter.BindingID)
	}
	first := "SELECT 1 FROM " + actions + " a WHERE " + identity() + " AND a.status IN (" + unfinishedSkillActionSQL + ") LIMIT 1"
	second := "SELECT 1 FROM " + runs + " r WHERE r.scope_kind=" + parameter(1) + " AND r.scope_id=" + parameter(2) + " AND r.status IN (" + unfinishedSkillRunSQL + ") AND EXISTS (SELECT 1 FROM " + actions + " a WHERE " + identity() + " AND a.run_id=r.id AND a.status='succeeded' LIMIT 1) LIMIT 1"
	dependency := "SELECT 1 FROM " + skillRuntimeDependencyTable(runs) + " d WHERE d.scope_kind=" + parameter(1) + " AND d.scope_id=" + parameter(2) + " AND d.skill_id=" + parameter(3) + " AND d.skill_version=" + parameter(4)
	if filter.DeploymentID != "" {
		dependency += " AND d.deployment_id IN (" + parameter(5) + ",'')"
	}
	dependency += " LIMIT 1"
	if !postgres {
		// SQLite positional placeholders occur again in the second EXISTS probe.
		original := append([]interface{}(nil), args...)
		args = append(args, filter.Scope.Kind, filter.Scope.ID)
		args = append(args, original...)
		args = append(args, filter.Scope.Kind, filter.Scope.ID, filter.SkillID, filter.SkillVersion)
		if filter.DeploymentID != "" {
			args = append(args, filter.DeploymentID)
		}
	}
	return "SELECT EXISTS (" + first + ") OR EXISTS (" + second + ") OR EXISTS (" + dependency + ")", args
}

func hasSkillRuntimeUsage(ctx context.Context, query skillRuntimeUsageQuerier, filter SkillRuntimeUsageFilter, actions, runs string, postgres bool) (bool, error) {
	if err := filter.Validate(); err != nil {
		return false, err
	}
	statement, args := skillRuntimeUsageQuery(filter, actions, runs, postgres)
	var busy bool
	err := query.QueryRowContext(ctx, statement, args...).Scan(&busy)
	return busy, err
}

func listReferencedSkillRuntimeVersionsQuery(bindings, actions, runs string, postgres bool) string {
	kind, id := "?1", "?2"
	enabled := "COALESCE(json_extract(payload,'$.disabled'),0)=0"
	if postgres {
		kind, id = "$1", "$2"
		enabled = "COALESCE((payload->>'disabled')::boolean,false)=false"
	}
	bindingJoin := " LEFT JOIN " + bindings + " b ON b.scope_kind=a.scope_kind AND b.scope_id=a.scope_id AND b.deployment_id=a.deployment_id AND b.id=a.binding_id AND b.skill_id=a.skill_id AND b.skill_version=a.skill_version"
	receiptActions := actions + " a"
	if !postgres {
		receiptActions += " INDEXED BY idx_action_calls_skill_run_receipt"
	}
	return "SELECT skill_id,skill_version,source_identity FROM " + bindings + " WHERE scope_kind=" + kind + " AND scope_id=" + id +
		" AND " + enabled +
		" UNION SELECT a.skill_id,a.skill_version,COALESCE(b.source_identity,'') FROM " + actions + " a" + bindingJoin +
		" WHERE a.scope_kind=" + kind + " AND a.scope_id=" + id + " AND a.status IN (" + unfinishedSkillActionSQL + ")" +
		" UNION SELECT a.skill_id,a.skill_version,COALESCE(b.source_identity,'') FROM " + runs + " r CROSS JOIN " + receiptActions + bindingJoin +
		" WHERE r.scope_kind=" + kind + " AND r.scope_id=" + id + " AND r.status IN (" + unfinishedSkillRunSQL + ") AND a.scope_kind=r.scope_kind AND a.scope_id=r.scope_id AND a.run_id=r.id AND a.status='succeeded'" +
		" UNION SELECT skill_id,skill_version,'' FROM " + skillRuntimeDependencyTable(runs) + " WHERE scope_kind=" + kind + " AND scope_id=" + id + " ORDER BY 1,2,3"
}

func scanReferencedSkillRuntimeVersions(rows *sql.Rows) ([]SkillRuntimeReference, error) {
	result := make([]SkillRuntimeReference, 0)
	for rows.Next() {
		var reference SkillRuntimeReference
		if err := rows.Scan(&reference.SkillID, &reference.SkillVersion, &reference.SourceIdentity); err != nil {
			return nil, err
		}
		if reference.SkillID != "" && reference.SkillVersion != "" {
			result = append(result, reference)
		}
	}
	return result, rows.Err()
}
