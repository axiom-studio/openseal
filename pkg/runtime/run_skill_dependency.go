package runtime

import (
	"errors"
	"strings"
)

var ErrRunSkillDependencyUnavailable = errors.New("selected Skill executable version is unavailable")

// A Run dependency retains executable identity; it never confers permission to
// invoke a Skill. The normal live binding and action policy still authorize use.
type runSkillDependency struct {
	Scope                                      Scope
	RunID, SkillID, SkillVersion, DeploymentID string
}

func typedRunSkillDependency(run *AgentRun) *runSkillDependency {
	if run == nil || isTerminalAgentRunStatus(run.Status) {
		return nil
	}
	invocation, ok := run.Context[capabilityInvocationContextKey].(map[string]interface{})
	if !ok {
		return nil
	}
	id, _ := invocation["skillId"].(string)
	version, _ := invocation["skillVersion"].(string)
	id, version = strings.TrimSpace(id), strings.TrimSpace(version)
	if id == "" || version == "" {
		return nil
	}
	return &runSkillDependency{Scope: run.Scope, RunID: run.ID, SkillID: id, SkillVersion: version, DeploymentID: runSkillDependencyDeployment(run)}
}

func runSkillDependencies(run *AgentRun) ([]runSkillDependency, error) {
	if run == nil || isTerminalAgentRunStatus(run.Status) {
		return nil, nil
	}
	dependencies := []runSkillDependency{}
	if typed := typedRunSkillDependency(run); typed != nil {
		dependencies = append(dependencies, *typed)
	}
	pin, err := AcceptedRunExecutionForRun(run)
	if err != nil {
		return nil, err
	}
	if pin != nil {
		for _, reference := range pin.SkillDependencies {
			dependencies = append(dependencies, runSkillDependency{Scope: run.Scope, RunID: run.ID, SkillID: reference.SkillID, SkillVersion: reference.SkillVersion, DeploymentID: runSkillDependencyDeployment(run)})
		}
	}
	return dependencies, nil
}

func runSkillDependencyDeployment(run *AgentRun) string {
	// A Team-owned method may use either member or Team grants. Its immutable
	// method does not identify which live account supplies each future action.
	if run.Owner.Type == OwnerTypeTeam {
		return ""
	}
	return run.AssignedAgentID
}

func skillRuntimeDependencyTable(runs string) string {
	if strings.HasSuffix(runs, `"agent_runs"`) {
		return strings.TrimSuffix(runs, `"agent_runs"`) + `"run_skill_dependencies"`
	}
	return strings.TrimSuffix(runs, "agent_runs") + "run_skill_dependencies"
}

func normalizeRunSkillDependencyError(err error) error {
	if err != nil && strings.Contains(err.Error(), ErrSkillRuntimeMaintenance.Error()) {
		return ErrSkillRuntimeMaintenance
	}
	if err != nil && strings.Contains(err.Error(), ErrRunSkillDependencyUnavailable.Error()) {
		return ErrRunSkillDependencyUnavailable
	}
	if err != nil && strings.Contains(err.Error(), ErrAcceptedRunExecution.Error()) {
		return ErrAcceptedRunExecution
	}
	return err
}
