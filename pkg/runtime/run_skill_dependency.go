package runtime

import (
	"strings"
)

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
