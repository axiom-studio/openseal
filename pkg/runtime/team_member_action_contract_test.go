package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// Earlier bindings keep matching update_profile in hosts until the reconcile
// upgrades them; update_member exists only in the current version.
func TestTeamMemberActionIsOnlyInTheCurrentContract(t *testing.T) {
	for version, want := range map[string]bool{"2.2.0": true, "2.1.0": false, "2.0.0": false, "1.0.0": false} {
		if got := IsSupportedTeamManagementSkillVersion(version); got != (version != "1.0.0") {
			t.Errorf("IsSupportedTeamManagementSkillVersion(%s) = %v", version, got)
		}
		definition := TeamManagementSkill()
		definition.Version = version
		bound := &skill.BoundAction{Definition: definition, Action: definition.Actions[TeamActionUpdateMember]}
		if got := isTeamMemberAction(bound); got != want {
			t.Errorf("isTeamMemberAction(%s) = %v", version, got)
		}
	}
	encoded, err := json.Marshal(TeamManagementSkill())
	if err != nil || strings.Contains(strings.ToLower(string(encoded)), "role") {
		t.Fatalf("openseal.teams still mentions roles: %s, %v", encoded, err)
	}
}
