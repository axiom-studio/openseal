package runtime

import (
	"errors"
	"strings"
)

var ErrRunSkillDependencyUnavailable = errors.New("selected Skill executable version is unavailable")

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
