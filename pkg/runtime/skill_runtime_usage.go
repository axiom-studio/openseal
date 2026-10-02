package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// SkillRuntimeUsageFilter selects one tenant's immutable executable version.
// The optional deployment/binding pair narrows the same check to one grant.
// No model arguments or action outputs participate in this query.
type SkillRuntimeUsageFilter struct {
	Scope        Scope  `json:"scope"`
	SkillID      string `json:"skillId"`
	SkillVersion string `json:"skillVersion"`
	DeploymentID string `json:"deploymentId,omitempty"`
	BindingID    string `json:"bindingId,omitempty"`
}

func (f SkillRuntimeUsageFilter) Validate() error {
	if err := f.Scope.Validate(); err != nil {
		return err
	}
	for _, value := range []string{f.SkillID, f.SkillVersion} {
		if value == "" || value != strings.TrimSpace(value) || len(value) > 256 || strings.ContainsAny(value, "\r\n\t") {
			return errors.New("an exact bounded Skill ID and version are required")
		}
	}
	if (f.DeploymentID == "") != (f.BindingID == "") {
		return errors.New("deployment and binding must be selected together")
	}
	for _, value := range []string{f.DeploymentID, f.BindingID} {
		if value != strings.TrimSpace(value) || len(value) > 256 || strings.ContainsAny(value, "\r\n\t") {
			return errors.New("deployment and binding must be exact bounded identities")
		}
	}
	return nil
}

// SkillRuntimeReference is a metadata-only retention identity. An empty source
// identity on a historical action means all installed publisher variants of
// that immutable version must be retained, rather than guessing its publisher.
type SkillRuntimeReference struct {
	SkillID        string `json:"skillId"`
	SkillVersion   string `json:"skillVersion"`
	SourceIdentity string `json:"sourceIdentity,omitempty"`
}

type SkillRuntimeUsageStore interface {
	HasSkillRuntimeUsage(context.Context, SkillRuntimeUsageFilter) (bool, error)
	ListReferencedSkillRuntimeVersions(context.Context, Scope) ([]SkillRuntimeReference, error)
}

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

func (s *SQLiteStore) HasSkillRuntimeUsage(ctx context.Context, filter SkillRuntimeUsageFilter) (bool, error) {
	return hasSkillRuntimeUsage(ctx, s.db, filter, "action_calls", "agent_runs", false)
}

func (s *PostgresStore) HasSkillRuntimeUsage(ctx context.Context, filter SkillRuntimeUsageFilter) (bool, error) {
	return hasSkillRuntimeUsage(ctx, s.db, filter, s.table("action_calls"), s.table("agent_runs"), true)
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

func (s *SQLiteStore) ListReferencedSkillRuntimeVersions(ctx context.Context, scope Scope) ([]SkillRuntimeReference, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, listReferencedSkillRuntimeVersionsQuery("skill_bindings", "action_calls", "agent_runs", false), scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanReferencedSkillRuntimeVersions(rows)
}

func (s *PostgresStore) ListReferencedSkillRuntimeVersions(ctx context.Context, scope Scope) ([]SkillRuntimeReference, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, listReferencedSkillRuntimeVersionsQuery(s.table("skill_bindings"), s.table("action_calls"), s.table("agent_runs"), true), scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanReferencedSkillRuntimeVersions(rows)
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

func skillBindingUpgradeUsageFilter(application *SkillReferenceUpgradeMutation) SkillRuntimeUsageFilter {
	return SkillRuntimeUsageFilter{Scope: application.Plan.Scope, SkillID: application.Plan.From.ID, SkillVersion: application.Plan.From.Version, DeploymentID: application.Plan.DeploymentID, BindingID: application.Plan.BindingID}
}
