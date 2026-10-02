package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/lib/pq"
)

var ErrProjectSkillReferenceUnavailable = errors.New("selected Project Skill executable version is unavailable")

type projectSkillReferenceSQLQuerier interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
}

func projectSkillReferenceParameter(n int, postgres bool) string {
	if postgres {
		return fmt.Sprintf("$%d", n)
	}
	return fmt.Sprintf("?%d", n)
}

func projectSkillReferenceAdmissionError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), ErrProjectSkillReferenceUnavailable.Error()) {
		return ErrProjectSkillReferenceUnavailable
	}
	if strings.Contains(err.Error(), "Project source monitor identity is invalid") {
		return ErrInvalidProject
	}
	var pg *pq.Error
	if errors.As(err, &pg) && pg.Code == "40P01" {
		return ErrProjectConflict
	}
	return err
}

// New writers take this fence before obtaining a Project row lock. The trigger
// also fences earlier binaries; their reverse lock order can produce a safe
// deadlock abort instead of admitting an obsolete reference.
func lockProjectSkillReferencesPostgresTx(ctx context.Context, tx *sql.Tx, scope Scope, previous, next *Project) error {
	ids := make(map[string]struct{})
	for _, project := range []*Project{previous, next} {
		if project == nil {
			continue
		}
		for _, monitor := range project.SourceMonitors {
			if monitor.SkillID != "" {
				ids[monitor.SkillID] = struct{}{}
			}
		}
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	for _, id := range ordered {
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended($1,0))`, skillRuntimeMaintenanceLockKey(scope, id)); err != nil {
			return projectSkillReferenceAdmissionError(err)
		}
	}
	return nil
}

// Unchanged historical monitors retain their exact executable identity. Only
// newly selected or changed references need admission against current bindings.
// Project references are configuration metadata: an exact disabled binding
// retains its version without enabling execution. Action admission independently
// requires enabled authority. No durable binding permits standalone Project mode.
func validateProjectSkillReferenceAdmissionSQL(ctx context.Context, q projectSkillReferenceSQLQuerier, previous, next *Project, bindingsTable string, postgres bool) error {
	if next == nil {
		return nil
	}
	old := make(map[string]SourceMonitorReference)
	if previous != nil {
		for _, monitor := range previous.SourceMonitors {
			old[monitor.ID] = monitor
		}
	}
	changed := make([]SourceMonitorReference, 0, len(next.SourceMonitors))
	seen := make(map[string]bool)
	for _, monitor := range next.SourceMonitors {
		if !validOpaqueIdentifier(monitor.ID, 128) || !validOpaqueIdentifier(monitor.AssignedAgentID, 128) ||
			!validOpaqueIdentifier(monitor.SkillID, 128) || strings.TrimSpace(monitor.SkillVersion) == "" ||
			len(monitor.SkillVersion) > 128 || !validOpaqueIdentifier(monitor.Action, 128) || seen[monitor.ID] {
			return ErrInvalidProject
		}
		seen[monitor.ID] = true
		prior, exists := old[monitor.ID]
		if !exists || prior != monitor || (previous != nil && (previous.Owner.ID != next.Owner.ID || previous.Scope != next.Scope)) {
			changed = append(changed, monitor)
		}
	}
	// Batching keeps SQL parameter and statement sizes bounded, independently
	// of the number of monitors attached to a Project.
	for start := 0; start < len(changed); start += 128 {
		end := start + 128
		if end > len(changed) {
			end = len(changed)
		}
		args := []interface{}{next.Scope.Kind, next.Scope.ID}
		candidates := make([]string, 0, end-start)
		for _, monitor := range changed[start:end] {
			parameter := func() string { return projectSkillReferenceParameter(len(args)+1, postgres) }
			deployment := parameter()
			args = append(args, monitor.AssignedAgentID)
			skillID := parameter()
			args = append(args, monitor.SkillID)
			version := parameter()
			args = append(args, monitor.SkillVersion)
			candidates = append(candidates, "SELECT "+deployment+" AS deployment_id,"+skillID+" AS skill_id,"+version+" AS skill_version")
		}
		identity := `b.scope_kind=` + projectSkillReferenceParameter(1, postgres) + ` AND b.scope_id=` + projectSkillReferenceParameter(2, postgres) + ` AND b.deployment_id=c.deployment_id AND b.skill_id=c.skill_id`
		query := `SELECT EXISTS(SELECT 1 FROM (` + strings.Join(candidates, " UNION ALL ") + `) c WHERE EXISTS(SELECT 1 FROM ` + bindingsTable + ` b WHERE ` + identity + `) AND NOT EXISTS(SELECT 1 FROM ` + bindingsTable + ` b WHERE ` + identity + ` AND b.skill_version=c.skill_version))`
		var unavailable bool
		if err := q.QueryRowContext(ctx, query, args...).Scan(&unavailable); err != nil {
			return projectSkillReferenceAdmissionError(err)
		}
		if unavailable {
			return ErrProjectSkillReferenceUnavailable
		}
	}
	return nil
}

// The transaction already holds the logical Skill fence. Compare the complete
// indexed monitor set with the reviewed Plan, including revisions and monitor
// IDs; a newly added Project or a legacy write that reused a revision conflicts.
func validateProjectSkillUpgradeReferencesSQL(ctx context.Context, q projectSkillReferenceSQLQuerier, application *SkillReferenceUpgradeMutation, referencesTable string, postgres bool) error {
	if application == nil || application.Plan == nil {
		return ErrSkillReferenceUpgradeConflict
	}
	type identity struct{ projectID, monitorID string }
	expected := make(map[identity]int64)
	for _, project := range application.Plan.Projects {
		for _, monitorID := range project.MonitorIDs {
			key := identity{project.ID, monitorID}
			if _, duplicate := expected[key]; duplicate || project.ExpectedRevision <= 0 {
				return ErrSkillReferenceUpgradeConflict
			}
			expected[key] = project.ExpectedRevision
		}
	}
	plan := application.Plan
	base := `scope_kind=` + projectSkillReferenceParameter(1, postgres) + ` AND scope_id=` + projectSkillReferenceParameter(2, postgres) + ` AND skill_id=` + projectSkillReferenceParameter(3, postgres) + ` AND skill_version=` + projectSkillReferenceParameter(4, postgres)
	// UNION ALL retains indexed seeks and permits an early LIMIT. A monitor can
	// occur twice (owner and assignee), so 2*N+1 rows suffice to prove a phantom.
	query := `SELECT project_id,monitor_id,project_revision FROM ` + referencesTable + ` WHERE ` + base + ` AND owner_id=` + projectSkillReferenceParameter(5, postgres) + ` UNION ALL SELECT project_id,monitor_id,project_revision FROM ` + referencesTable + ` WHERE ` + base + ` AND assigned_agent_id=` + projectSkillReferenceParameter(5, postgres) + ` LIMIT ` + projectSkillReferenceParameter(6, postgres)
	rows, err := q.QueryContext(ctx, query, plan.Scope.Kind, plan.Scope.ID, plan.From.ID, plan.From.Version, plan.DeploymentID, 2*len(expected)+1)
	if err != nil {
		return err
	}
	defer rows.Close()
	actual := make(map[identity]int64, len(expected))
	for rows.Next() {
		var key identity
		var revision int64
		if err := rows.Scan(&key.projectID, &key.monitorID, &revision); err != nil {
			return err
		}
		if expectedRevision, exists := expected[key]; !exists || expectedRevision != revision {
			return ErrSkillReferenceUpgradeConflict
		}
		actual[key] = revision
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return ErrSkillReferenceUpgradeConflict
	}
	return nil
}

func (s *SQLiteStore) ListProjectSkillReferencesForUpgrade(ctx context.Context, scope Scope, deploymentID, skillID, skillVersion, afterProjectID string, limit int) ([]*Project, error) {
	return listProjectSkillReferenceUpgradePageSQL(ctx, s.db, scope, deploymentID, skillID, skillVersion, afterProjectID, limit, "projects", "project_skill_references", false)
}

func (s *PostgresStore) ListProjectSkillReferencesForUpgrade(ctx context.Context, scope Scope, deploymentID, skillID, skillVersion, afterProjectID string, limit int) ([]*Project, error) {
	return listProjectSkillReferenceUpgradePageSQL(ctx, s.db, scope, deploymentID, skillID, skillVersion, afterProjectID, limit, s.table("projects"), s.table("project_skill_references"), true)
}

func listProjectSkillReferenceUpgradePageSQL(ctx context.Context, q projectSkillReferenceSQLQuerier, scope Scope, deploymentID, skillID, skillVersion, afterProjectID string, limit int, projectsTable, referencesTable string, postgres bool) ([]*Project, error) {
	if err := validateProjectSkillReferencePage(scope, deploymentID, skillID, skillVersion, limit); err != nil {
		return nil, err
	}
	base := `scope_kind=` + projectSkillReferenceParameter(1, postgres) + ` AND scope_id=` + projectSkillReferenceParameter(2, postgres) + ` AND skill_id=` + projectSkillReferenceParameter(3, postgres) + ` AND skill_version=` + projectSkillReferenceParameter(4, postgres) + ` AND project_id>` + projectSkillReferenceParameter(6, postgres)
	branch := func(column, alias string) string {
		return `SELECT project_id FROM (SELECT DISTINCT project_id FROM ` + referencesTable + ` WHERE ` + base + ` AND ` + column + `=` + projectSkillReferenceParameter(5, postgres) + ` ORDER BY project_id LIMIT ` + projectSkillReferenceParameter(7, postgres) + `) ` + alias
	}
	matched := `(` + branch("owner_id", "owner_refs") + ` UNION ` + branch("assigned_agent_id", "assigned_refs") + ` ORDER BY project_id LIMIT ` + projectSkillReferenceParameter(7, postgres) + `) matched`
	identity := `p.scope_kind=` + projectSkillReferenceParameter(1, postgres) + ` AND p.scope_id=` + projectSkillReferenceParameter(2, postgres) + ` AND p.id=matched.project_id`
	// Start from the bounded metadata set. SQLite CROSS JOIN prevents reversing
	// the loop into a scan of every same-scope Project; PostgreSQL LATERAL with
	// LIMIT 1 preserves a Project primary-key lookup for each matched identity.
	query := `SELECT p.payload FROM ` + matched + ` CROSS JOIN ` + projectsTable + ` p WHERE ` + identity + ` ORDER BY matched.project_id`
	if postgres {
		// Project IDs retain the existing table's default collation for equality
		// and its primary index; metadata cursors and ordering use byte order.
		identity += ` COLLATE "default"`
		query = `SELECT p.payload FROM ` + matched + ` CROSS JOIN LATERAL (SELECT p.payload FROM ` + projectsTable + ` p WHERE ` + identity + ` LIMIT 1) p ORDER BY matched.project_id`
	}
	rows, err := q.QueryContext(ctx, query, scope.Kind, scope.ID, skillID, skillVersion, deploymentID, afterProjectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := make([]*Project, 0, limit)
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var project Project
		if err := json.Unmarshal(payload, &project); err != nil {
			return nil, err
		}
		projects = append(projects, &project)
	}
	return projects, rows.Err()
}
