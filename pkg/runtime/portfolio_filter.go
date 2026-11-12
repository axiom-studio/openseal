package runtime

func portfolioKey(scope Scope, id string) string { return scope.Key() + ":" + id }

func pageObjectives(values []*Objective, offset, limit int) []*Objective {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(values) {
		return []*Objective{}
	}
	values = values[offset:]
	if limit > 0 && limit < len(values) {
		values = values[:limit]
	}
	return values
}

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

func cloneObjectiveExecutionPolicy(in *ObjectiveExecutionPolicy) *ObjectiveExecutionPolicy {
	if in == nil {
		return nil
	}
	out := *in
	out.ResourceCapacities = cloneResourceQuantities(in.ResourceCapacities)
	return &out
}
