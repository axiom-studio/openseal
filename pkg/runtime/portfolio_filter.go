package runtime

func portfolioKey(scope Scope, id string) string { return scope.Key() + ":" + id }

func cloneObjectiveExecutionPolicy(in *ObjectiveExecutionPolicy) *ObjectiveExecutionPolicy {
	if in == nil {
		return nil
	}
	out := *in
	out.ResourceCapacities = cloneResourceQuantities(in.ResourceCapacities)
	return &out
}
