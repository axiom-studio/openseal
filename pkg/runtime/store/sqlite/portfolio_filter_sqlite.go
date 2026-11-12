package sqlite

import "github.com/axiom-studio/openseal/pkg/models"

func pageAgentRuns(values []*models.AgentRun, offset, limit int) []*models.AgentRun {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(values) {
		return []*models.AgentRun{}
	}
	values = values[offset:]
	if limit > 0 && limit < len(values) {
		values = values[:limit]
	}
	return values
}
