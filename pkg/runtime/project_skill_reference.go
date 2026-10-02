package runtime

import (
	"context"
	"fmt"
	"reflect"
	"sort"
)

// ProjectSkillReferenceStore pages only Projects with exact live references.
// The projection is maintained by the same canonical Project transaction.
type ProjectSkillReferenceStore interface {
	ListProjectSkillReferencesForUpgrade(context.Context, Scope, string, string, string, string, int) ([]*Project, error)
}

type memoryProjectSkillReferenceKey struct {
	Scope                               Scope
	DeploymentID, SkillID, SkillVersion string
}

func projectSkillReferenceKeys(project *Project) map[memoryProjectSkillReferenceKey]struct{} {
	keys := make(map[memoryProjectSkillReferenceKey]struct{})
	if project == nil {
		return keys
	}
	for _, monitor := range project.SourceMonitors {
		for _, owner := range []string{project.Owner.ID, monitor.AssignedAgentID} {
			keys[memoryProjectSkillReferenceKey{project.Scope, owner, monitor.SkillID, monitor.SkillVersion}] = struct{}{}
		}
	}
	return keys
}

func (s *MemoryStore) saveProjectSkillReferencesLocked(project *Project) {
	key := projectKey(project.Scope, project.ID)
	for reference := range projectSkillReferenceKeys(s.projects[key]) {
		delete(s.projectSkillReferences[reference], project.ID)
		if len(s.projectSkillReferences[reference]) == 0 {
			delete(s.projectSkillReferences, reference)
		}
	}
	if s.projectSkillReferences == nil {
		s.projectSkillReferences = make(map[memoryProjectSkillReferenceKey]map[string]struct{})
	}
	s.projects[key] = cloneProject(project)
	for reference := range projectSkillReferenceKeys(project) {
		if s.projectSkillReferences[reference] == nil {
			s.projectSkillReferences[reference] = make(map[string]struct{})
		}
		s.projectSkillReferences[reference][project.ID] = struct{}{}
	}
}

func changedProjectSkillMonitors(previous, next *Project) []SourceMonitorReference {
	old := make(map[string]SourceMonitorReference)
	if previous != nil {
		for _, monitor := range previous.SourceMonitors {
			old[monitor.ID] = monitor
		}
	}
	var changed []SourceMonitorReference
	for _, monitor := range next.SourceMonitors {
		if before, found := old[monitor.ID]; !found || !reflect.DeepEqual(before, monitor) || previous != nil && previous.Owner.ID != next.Owner.ID {
			changed = append(changed, monitor)
		}
	}
	return changed
}

func (s *MemoryStore) validateProjectSkillReferenceAdmissionLocked(previous, next *Project) error {
	for _, monitor := range changedProjectSkillMonitors(previous, next) {
		known, available := false, false
		for _, binding := range s.skillBindings {
			if binding.Scope.Kind != next.Scope.Kind || binding.Scope.ID != next.Scope.ID || binding.DeploymentID != monitor.AssignedAgentID || binding.SkillID != monitor.SkillID {
				continue
			}
			known = true
			// Reference configuration grants no execution authority. Dormant
			// exact bindings remain usable while their actions stay disabled.
			if binding.SkillVersion == monitor.SkillVersion {
				available = true
			}
		}
		if known && !available {
			return ErrProjectSkillReferenceUnavailable
		}
	}
	return nil
}

func (s *MemoryStore) ListProjectSkillReferencesForUpgrade(ctx context.Context, scope Scope, deploymentID, skillID, skillVersion, afterProjectID string, limit int) ([]*Project, error) {
	if err := validateProjectSkillReferencePage(scope, deploymentID, skillID, skillVersion, limit); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0)
	for id := range s.projectSkillReferences[memoryProjectSkillReferenceKey{scope, deploymentID, skillID, skillVersion}] {
		if id > afterProjectID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) > limit {
		ids = ids[:limit]
	}
	result := make([]*Project, 0, len(ids))
	for _, id := range ids {
		result = append(result, cloneProject(s.projects[projectKey(scope, id)]))
	}
	return result, nil
}

func validateProjectSkillReferencePage(scope Scope, deploymentID, skillID, skillVersion string, limit int) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if deploymentID == "" || skillID == "" || skillVersion == "" || limit <= 0 || limit > 500 {
		return fmt.Errorf("%w: exact bounded Project Skill reference page is required", ErrSkillReferenceUpgradeInvalid)
	}
	return nil
}

func (s *MemoryStore) validateProjectSkillUpgradeReferencesLocked(application *SkillReferenceUpgradeMutation) error {
	expected := make(map[string]SkillReferenceProjectImpact)
	for _, impact := range application.Plan.Projects {
		expected[impact.ID] = impact
	}
	plan := application.Plan
	for id := range s.projectSkillReferences[memoryProjectSkillReferenceKey{plan.Scope, plan.DeploymentID, plan.From.ID, plan.From.Version}] {
		project := s.projects[projectKey(plan.Scope, id)]
		impact, found := expected[id]
		if project == nil || !found || project.Revision != impact.ExpectedRevision {
			return ErrSkillReferenceUpgradeConflict
		}
		var monitors []string
		for _, monitor := range project.SourceMonitors {
			if referenceOwnedOrAssigned(project.Owner, monitor.AssignedAgentID, plan.DeploymentID) && monitor.SkillID == plan.From.ID && monitor.SkillVersion == plan.From.Version {
				monitors = append(monitors, monitor.ID)
			}
		}
		sort.Strings(monitors)
		if !reflect.DeepEqual(monitors, impact.MonitorIDs) {
			return ErrSkillReferenceUpgradeConflict
		}
		delete(expected, id)
	}
	if len(expected) != 0 {
		return ErrSkillReferenceUpgradeConflict
	}
	return nil
}

func projectSkillUpgradeLockIDs(application *SkillReferenceUpgradeMutation) []string {
	ids := map[string]bool{application.Plan.From.ID: true}
	for _, mutation := range application.Projects {
		for _, monitor := range mutation.Value.SourceMonitors {
			ids[monitor.SkillID] = true
		}
	}
	result := make([]string, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}
