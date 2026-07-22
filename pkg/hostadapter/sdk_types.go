package hostadapter

import (
	"encoding/json"

	"github.com/axiom-studio/openseal/pkg/executor"
)

// Host adapters implement the shared execution SDK contract while remaining
// leaf dependencies of the OpenSeal kernel.
type StepDefinition = executor.StepDefinition
type StepResult = executor.StepResult
type TemplateResolver = executor.TemplateResolver

func cloneMap(value map[string]interface{}) map[string]interface{} {
	encoded, _ := json.Marshal(value)
	var result map[string]interface{}
	_ = json.Unmarshal(encoded, &result)
	return result
}
