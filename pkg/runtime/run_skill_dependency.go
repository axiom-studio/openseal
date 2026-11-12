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
