package runtime

func pageAgentRuns(values []*AgentRun, offset, limit int) []*AgentRun {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(values) {
		return []*AgentRun{}
	}
	values = values[offset:]
	if limit > 0 && limit < len(values) {
		values = values[:limit]
	}
	return values
}
