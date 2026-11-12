package skillbinding

import (
	"encoding/json"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// newUpgradeTestStore and cloneUpgradeDefinition are copies of runtime's
// newTestKernelStore and cloneUpgradeTestDefinition for the skill reference
// upgrade tests, which live with the skill binding code and cannot see
// runtime's test helpers. Change the copies together.

// newUpgradeTestStore returns an empty in-memory kernel store that is closed
// when the test ends.
func newUpgradeTestStore(t testing.TB) *sqlite.Store {
	t.Helper()
	store, err := sqlite.NewInMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// cloneUpgradeDefinition deep-copies a Skill definition through JSON.
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
