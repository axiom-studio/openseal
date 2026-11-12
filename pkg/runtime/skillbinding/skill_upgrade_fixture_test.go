package skillbinding

import (
	"encoding/json"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// cloneUpgradeDefinition is a copy of runtime's cloneUpgradeTestDefinition
// for the skill reference upgrade tests, which live with the skill binding
// code and cannot see runtime's test helpers. Change the copies together.
// It deep-copies a Skill definition through JSON.
func cloneUpgradeDefinition(t *testing.T, value *skill.Definition) *skill.Definition {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result skill.Definition
	if err = json.Unmarshal(payload, &result); err != nil {
		t.Fatal(err)
	}
	return &result
}
