package runtime

import "reflect"

// Every canonical Run commit updates this metadata projection under the same
// lock. Accepted identities remain pinned across checkpoint/context changes,
// and terminal transitions remove them without editing historical Run payloads.
func (s *MemoryStore) refreshMemoryRunSkillDependenciesLocked(runKey string, run *AgentRun) {
	if isTerminalAgentRunStatus(run.Status) {
		for key := range s.skillRuntimeRunDependencies[runKey] {
			s.adjustMemorySkillRuntimeUsageLocked(runSkillDependencyUsageKeys(key), -1)
			s.adjustMemoryUnqualifiedRunSkillDependencyLocked(key, -1)
		}
		delete(s.skillRuntimeRunDependencies, runKey)
		return
	}
	dependencies, _ := runSkillDependencies(run) // Canonical writes validated before their first mutation.
	for _, dependency := range dependencies {
		key := memorySkillRuntimeUsageKey{Scope: dependency.Scope, SkillID: dependency.SkillID, SkillVersion: dependency.SkillVersion, DeploymentID: dependency.DeploymentID}
		if _, exists := s.skillRuntimeRunDependencies[runKey][key]; exists {
			continue
		}
		if s.skillRuntimeRunDependencies == nil {
			s.skillRuntimeRunDependencies = make(map[string]map[memorySkillRuntimeUsageKey]struct{})
		}
		if s.skillRuntimeRunDependencies[runKey] == nil {
			s.skillRuntimeRunDependencies[runKey] = make(map[memorySkillRuntimeUsageKey]struct{})
		}
		s.skillRuntimeRunDependencies[runKey][key] = struct{}{}
		s.adjustMemorySkillRuntimeUsageLocked(runSkillDependencyUsageKeys(key), 1)
		s.adjustMemoryUnqualifiedRunSkillDependencyLocked(key, 1)
	}
}

func runSkillDependencyUsageKeys(key memorySkillRuntimeUsageKey) [2]memorySkillRuntimeUsageKey {
	runtime := key
	runtime.DeploymentID, runtime.BindingID = "", ""
	return [2]memorySkillRuntimeUsageKey{runtime, key}
}

func (s *MemoryStore) validateMemoryRunSkillDependenciesLocked(run *AgentRun) error {
	if run == nil {
		return nil
	}
	pin, err := AcceptedRunExecutionForRun(run)
	if err != nil {
		return err
	}
	if current := s.agentRuns[portfolioKey(run.Scope, run.ID)]; current != nil {
		previous, err := AcceptedRunExecutionForRun(current)
		if err != nil {
			return err
		}
		if previous != nil && !reflect.DeepEqual(previous, pin) {
			return ErrAcceptedRunExecution
		}
	}
	dependencies, err := runSkillDependencies(run)
	if err != nil {
		return err
	}
	for _, dependency := range dependencies {
		key := memorySkillRuntimeUsageKey{Scope: dependency.Scope, SkillID: dependency.SkillID, SkillVersion: dependency.SkillVersion, DeploymentID: dependency.DeploymentID}
		if _, accepted := s.skillRuntimeRunDependencies[portfolioKey(run.Scope, run.ID)][key]; accepted {
			continue
		}
		if maintenance := s.memorySkillRuntimeMaintenanceActiveLocked(run.Scope, dependency.SkillID); maintenance != nil {
			return &SkillRuntimeMaintenanceError{Maintenance: *maintenance}
		}
		known, exact := false, false
		for _, binding := range s.skillBindings {
			if binding.Scope.Kind != run.Scope.Kind || binding.Scope.ID != run.Scope.ID || binding.SkillID != dependency.SkillID || (dependency.DeploymentID != "" && binding.DeploymentID != dependency.DeploymentID) {
				continue
			}
			known = true
			if !binding.Disabled && binding.SkillVersion == dependency.SkillVersion {
				exact = true
				break
			}
		}
		if known && !exact {
			return ErrRunSkillDependencyUnavailable
		}
	}
	return nil
}

func (s *MemoryStore) memoryUnqualifiedRunSkillDependencyLocked(key memorySkillRuntimeUsageKey) bool {
	return s.skillRuntimeUnqualifiedDependencies[key] > 0
}

func (s *MemoryStore) adjustMemoryUnqualifiedRunSkillDependencyLocked(key memorySkillRuntimeUsageKey, delta int) {
	if key.DeploymentID != "" {
		return
	}
	if s.skillRuntimeUnqualifiedDependencies == nil {
		s.skillRuntimeUnqualifiedDependencies = make(map[memorySkillRuntimeUsageKey]int)
	}
	count := s.skillRuntimeUnqualifiedDependencies[key] + delta
	if count > 0 {
		s.skillRuntimeUnqualifiedDependencies[key] = count
	} else {
		delete(s.skillRuntimeUnqualifiedDependencies, key)
	}
}
