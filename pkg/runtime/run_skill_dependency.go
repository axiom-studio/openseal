package runtime

import (
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
)

func skillRuntimeDependencyTable(runs string) string {
	if strings.HasSuffix(runs, `"agent_runs"`) {
		return strings.TrimSuffix(runs, `"agent_runs"`) + `"run_skill_dependencies"`
	}
	return strings.TrimSuffix(runs, "agent_runs") + "run_skill_dependencies"
}

func normalizeRunSkillDependencyError(err error) error {
	if err != nil && strings.Contains(err.Error(), models.ErrSkillRuntimeMaintenance.Error()) {
		return models.ErrSkillRuntimeMaintenance
	}
	if err != nil && strings.Contains(err.Error(), models.ErrRunSkillDependencyUnavailable.Error()) {
		return models.ErrRunSkillDependencyUnavailable
	}
	if err != nil && strings.Contains(err.Error(), models.ErrAcceptedRunExecution.Error()) {
		return models.ErrAcceptedRunExecution
	}
	return err
}
