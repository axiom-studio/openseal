package openseal

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// The builder actions changed three bundled Skills. A changed definition
// under an old id@version is refused by hosts, so each one carries a new
// version, and the versions bound before the upgrade keep working for the
// actions they had until the host reconcile moves the bindings.
func TestBundledSkillVersionsForAgentBuilder(t *testing.T) {
	for _, tc := range []struct {
		definition *skill.Definition
		alias      string
		version    string
		actions    []string
	}{
		{runtime.AgentManagementSkill(), AgentManagementSkillVersion, "1.5.0",
			[]string{AgentActionAmendBehavior, AgentActionListChannels, AgentActionConfigureChannel,
				runtime.AgentActionListAgents, runtime.AgentActionCreateAgent, runtime.AgentActionUpdateAgent, runtime.AgentActionTryAgent}},
		{runtime.TeamManagementSkill(), TeamManagementSkillVersion, "2.1.0",
			[]string{TeamActionUpdateRole, TeamActionUpdateProfile, TeamActionCreateTeam}},
		{runtime.SkillManagementSkill(), SkillManagementSkillVersion, "1.7.0",
			[]string{SkillActionDiscover, SkillActionUpsertBinding, SkillActionDisableBinding, SkillActionRequestSetup, SkillActionRequestConnection}},
	} {
		if tc.definition.Version != tc.version || tc.alias != tc.version {
			t.Fatalf("%s version = %s (alias %s), want %s", tc.definition.ID, tc.definition.Version, tc.alias, tc.version)
		}
		for _, name := range tc.actions {
			if _, ok := tc.definition.Actions[name]; !ok {
				t.Fatalf("%s@%s has no %s action", tc.definition.ID, tc.version, name)
			}
		}
	}

	skills := runtime.SkillManagementSkill()
	for _, name := range []string{SkillActionDiscover, SkillActionUpsertBinding, SkillActionDisableBinding, SkillActionRequestSetup, SkillActionRequestConnection} {
		properties, _ := skills.Actions[name].InputSchema["properties"].(map[string]interface{})
		if _, ok := properties[runtime.SkillActionTargetAgentArgument]; !ok {
			t.Fatalf("%s@%s %s has no %s", skills.ID, skills.Version, name, runtime.SkillActionTargetAgentArgument)
		}
	}

	if !IsSupportedAgentManagementSkillVersion("1.4.0") || !IsSupportedAgentManagementSkillVersion("1.5.0") {
		t.Fatal("openseal.agents 1.4.0 and 1.5.0 must both run")
	}
	if !runtime.IsSupportedTeamManagementSkillVersion("2.0.0") || !runtime.IsSupportedTeamManagementSkillVersion("2.1.0") {
		t.Fatal("openseal.teams 2.0.0 and 2.1.0 must both run")
	}
	if IsSupportedAgentManagementSkillVersion("1.6.0") || runtime.IsSupportedTeamManagementSkillVersion("2.2.0") {
		t.Fatal("an unreleased version must not be supported")
	}
}
