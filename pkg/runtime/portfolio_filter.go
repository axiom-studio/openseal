package runtime

import "github.com/axiom-studio/openseal/pkg/models"

func portfolioKey(scope models.Scope, id string) string { return scope.Key() + ":" + id }

func cloneObjectiveExecutionPolicy(in *models.ObjectiveExecutionPolicy) *models.ObjectiveExecutionPolicy {
	if in == nil {
		return nil
	}
	out := *in
	out.ResourceCapacities = cloneResourceQuantities(in.ResourceCapacities)
	return &out
}
